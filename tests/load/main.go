// Command load opens many chat connections, sends messages at a steady rate
// and reports how long acknowledgements and deliveries take.
//
//	go run ./tests/load -urls http://localhost:8081,http://localhost:8082 -clients 50 -duration 30s
//
// Clients are spread across the URLs and grouped into rooms. Every message is
// delivered to everyone in its room, so deliveries = messages x room size.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	urls     = flag.String("urls", "http://localhost:8081,http://localhost:8082", "instance URLs, comma separated")
	clients  = flag.Int("clients", 50, "number of connected users")
	roomSize = flag.Int("room", 10, "users per room")
	rate     = flag.Float64("rate", 1, "messages per second per user (the server allows up to 5)")
	duration = flag.Duration("duration", 30*time.Second, "how long to send")
)

// results collects counts and latencies from every client.
type results struct {
	sent, acked, delivered, errors atomic.Int64

	mu         sync.Mutex
	ackTimes   []time.Duration
	deliveries []time.Duration
}

func (r *results) add(list *[]time.Duration, d time.Duration) {
	r.mu.Lock()
	*list = append(*list, d)
	r.mu.Unlock()
}

type client struct {
	http *http.Client
	conn *websocket.Conn
}

func main() {
	flag.Parse()
	bases := strings.Split(*urls, ",")
	ctx := context.Background()

	// Sign up and connect everyone, eight at a time so sign-up (which hashes a
	// password) does not swamp the server before the test starts.
	log.Printf("connecting %d clients to %d instances", *clients, len(bases))
	all := make([]*client, *clients)
	rooms := make([]int64, *clients)
	var setup sync.WaitGroup
	slots := make(chan struct{}, 8)
	for start := 0; start < *clients; start += *roomSize {
		setup.Go(func() {
			end := min(start+*roomSize, *clients)
			var room int64
			for i := start; i < end; i++ {
				slots <- struct{}{}
				c := signUp(bases[0])
				if i == start {
					room = c.createRoom(bases[0])
				} else {
					c.post(bases[0], fmt.Sprintf("/api/rooms/%d/join", room), nil, nil)
				}
				c.dial(ctx, bases[i%len(bases)])
				all[i], rooms[i] = c, room
				<-slots
			}
		})
	}
	setup.Wait()

	log.Printf("sending for %s at %.1f messages per second per client", *duration, *rate)
	var res results
	var readers, senders sync.WaitGroup
	stop := time.Now().Add(*duration)
	for i, c := range all {
		readers.Go(func() { c.read(ctx, &res) })
		senders.Go(func() { c.send(ctx, rooms[i], stop, &res) })
	}
	senders.Wait()
	time.Sleep(2 * time.Second) // let the last deliveries arrive
	for _, c := range all {
		c.conn.CloseNow()
	}
	readers.Wait()

	fmt.Printf("\nclients %d on %d instances, rooms of %d\n", *clients, len(bases), *roomSize)
	fmt.Printf("sent %d  acknowledged %d  errors %d\n", res.sent.Load(), res.acked.Load(), res.errors.Load())
	fmt.Printf("delivered %d of %d expected\n", res.delivered.Load(), expectedDeliveries(res.sent.Load()))
	fmt.Printf("acknowledge latency  %s\n", percentiles(res.ackTimes))
	fmt.Printf("delivery latency     %s\n", percentiles(res.deliveries))
}

// expectedDeliveries is exact when the clients divide evenly into rooms.
func expectedDeliveries(sent int64) int64 {
	return sent * int64(min(*roomSize, *clients))
}

func percentiles(d []time.Duration) string {
	if len(d) == 0 {
		return "no samples"
	}
	slices.Sort(d)
	at := func(p float64) time.Duration { return d[int(p*float64(len(d)-1))].Round(100 * time.Microsecond) }
	return fmt.Sprintf("p50 %s  p95 %s  p99 %s  max %s", at(0.50), at(0.95), at(0.99), at(1))
}

func signUp(base string) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	name := "load" + strings.ToLower(rand.Text())[:12]
	c.post(base, "/api/auth/register", map[string]string{"username": name, "password": "load test password"}, nil)
	return c
}

func (c *client) createRoom(base string) int64 {
	var room struct {
		ID int64 `json:"id"`
	}
	c.post(base, "/api/rooms", map[string]string{"name": "load" + strings.ToLower(rand.Text())[:12]}, &room)
	return room.ID
}

func (c *client) post(base, path string, body, out any) {
	payload, _ := json.Marshal(body)
	res, err := c.http.Post(base+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		log.Fatalf("POST %s: status %d", path, res.StatusCode)
	}
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
}

func (c *client) dial(ctx context.Context, base string) {
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		log.Fatalf("dial %s: %v", base, err)
	}
	c.conn = conn
}

// send writes messages until the stop time. Each message carries the time it
// was sent, so whoever receives it can work out how long it took.
func (c *client) send(ctx context.Context, room int64, stop time.Time, res *results) {
	interval := time.Duration(float64(time.Second) / *rate)
	time.Sleep(time.Duration(float64(interval) * randomFraction())) // spread the clients out
	for time.Now().Before(stop) {
		frame, _ := json.Marshal(map[string]any{
			"type": "message", "room_id": room, "client_message_id": newUUID(),
			"content": strconv.FormatInt(time.Now().UnixNano(), 10),
		})
		if err := c.conn.Write(ctx, websocket.MessageText, frame); err != nil {
			res.errors.Add(1)
			return
		}
		res.sent.Add(1)
		time.Sleep(interval)
	}
}

// read counts acknowledgements and deliveries until the socket closes.
func (c *client) read(ctx context.Context, res *results) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		sentAt, _ := strconv.ParseInt(f.Message.Content, 10, 64)
		took := time.Since(time.Unix(0, sentAt))
		switch f.Type {
		case "ack":
			res.acked.Add(1)
			res.add(&res.ackTimes, took)
		case "message":
			res.delivered.Add(1)
			res.add(&res.deliveries, took)
		case "error":
			res.errors.Add(1)
		}
	}
}

func randomFraction() float64 {
	b := make([]byte, 1)
	rand.Read(b)
	return float64(b[0]) / 256
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
