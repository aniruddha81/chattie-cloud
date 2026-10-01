//go:build integration

// Package integration tests a running stack through its public interface.
// Start the local stack first, then:
//
//	go test -tags integration ./tests/integration/
//
// CHATTIE_URLS lists two or more instance URLs, comma separated.
package integration

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var instances = strings.Split(cmp.Or(os.Getenv("CHATTIE_URLS"), "http://localhost:8081,http://localhost:8082"), ",")

type message struct {
	ID       int64  `json:"id"`
	RoomID   int64  `json:"room_id"`
	Sequence int64  `json:"sequence"`
	Sender   string `json:"sender"`
	Content  string `json:"content"`
}

type frame struct {
	Type            string   `json:"type"`
	Message         *message `json:"message"`
	ClientMessageID string   `json:"client_message_id"`
	Error           string   `json:"error"`
}

// user is a signed-in account with its own cookie jar, like one browser.
type user struct {
	t    *testing.T
	name string
	http *http.Client
}

func newUser(t *testing.T) *user {
	jar, _ := cookiejar.New(nil)
	u := &user{t: t, name: "u" + strings.ToLower(rand.Text())[:12], http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	u.must(http.StatusCreated, instances[0], "POST", "/api/auth/register", map[string]string{"username": u.name, "password": "correct horse"}, nil)
	return u
}

// call makes a JSON request and returns the status code.
func (u *user) call(base, method, path string, body, out any) int {
	u.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		json.NewEncoder(&payload).Encode(body)
	}
	req, _ := http.NewRequest(method, base+path, &payload)
	req.Header.Set("Content-Type", "application/json")
	res, err := u.http.Do(req)
	if err != nil {
		u.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode < 300 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			u.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return res.StatusCode
}

func (u *user) must(want int, base, method, path string, body, out any) {
	u.t.Helper()
	if got := u.call(base, method, path, body, out); got != want {
		u.t.Fatalf("%s %s: status %d, want %d", method, path, got, want)
	}
}

func (u *user) createRoom() int64 {
	u.t.Helper()
	var room struct {
		ID int64 `json:"id"`
	}
	u.must(http.StatusCreated, instances[0], "POST", "/api/rooms", map[string]string{"name": "r" + strings.ToLower(rand.Text())[:12]}, &room)
	return room.ID
}

func (u *user) join(base string, roomID int64) {
	u.t.Helper()
	u.must(http.StatusNoContent, base, "POST", fmt.Sprintf("/api/rooms/%d/join", roomID), nil, nil)
}

func (u *user) history(base string, roomID, after int64) []message {
	u.t.Helper()
	var msgs []message
	u.must(http.StatusOK, base, "GET", fmt.Sprintf("/api/rooms/%d/messages?after=%d", roomID, after), nil, &msgs)
	return msgs
}

// socket is one WebSocket connection to one instance.
type socket struct {
	t    *testing.T
	conn *websocket.Conn
}

func (u *user) dial(base string) *socket {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPClient: u.http})
	if err != nil {
		u.t.Fatalf("dial %s: %v", base, err)
	}
	u.t.Cleanup(func() { conn.CloseNow() })
	s := &socket{t: u.t, conn: conn}
	s.wait(func(f frame) bool { return f.Type == "hello" })
	return s
}

func (s *socket) send(roomID int64, clientMessageID, content string) {
	s.t.Helper()
	data, _ := json.Marshal(map[string]any{"type": "message", "room_id": roomID, "client_message_id": clientMessageID, "content": content})
	if err := s.conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

// wait reads frames until one matches, failing the test after five seconds.
func (s *socket) wait(match func(frame) bool) frame {
	s.t.Helper()
	return s.waitFor(5*time.Second, match)
}

func (s *socket) waitFor(timeout time.Duration, match func(frame) bool) frame {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.t.Fatalf("waiting for frame: %v", err)
		}
		var f frame
		if json.Unmarshal(data, &f) == nil && match(f) {
			return f
		}
	}
}

func (s *socket) waitAck(clientMessageID string) *message {
	s.t.Helper()
	f := s.wait(func(f frame) bool {
		return (f.Type == "ack" || f.Type == "error") && f.ClientMessageID == clientMessageID
	})
	if f.Type == "error" {
		s.t.Fatalf("send rejected: %s", f.Error)
	}
	return f.Message
}

func (s *socket) waitMessage(roomID int64) *message {
	s.t.Helper()
	return s.wait(func(f frame) bool { return f.Type == "message" && f.Message.RoomID == roomID }).Message
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Two users on different instances receive each other's messages.
func TestCrossInstanceDelivery(t *testing.T) {
	alice, bob := newUser(t), newUser(t)
	room := alice.createRoom()
	bob.join(instances[1], room)
	a, b := alice.dial(instances[0]), bob.dial(instances[1])

	id := newUUID()
	a.send(room, id, "hello from instance one")
	acked := a.waitAck(id)
	got := b.waitMessage(room)
	if got.ID != acked.ID || got.Content != "hello from instance one" || got.Sender != alice.name {
		t.Fatalf("bob got %+v, alice's ack was %+v", got, acked)
	}

	id = newUUID()
	b.send(room, id, "hello back")
	b.waitAck(id)
	// Alice also gets her own message as a live frame, so wait for bob's.
	reply := a.wait(func(f frame) bool { return f.Type == "message" && f.Message.Sender == bob.name }).Message
	if reply.Sequence != acked.Sequence+1 {
		t.Fatalf("reply has sequence %d, want %d", reply.Sequence, acked.Sequence+1)
	}
}

// Joining a room while already connected reaches the socket's instance
// through a membership event.
func TestJoinWhileConnected(t *testing.T) {
	alice, bob := newUser(t), newUser(t)
	room := alice.createRoom()
	a, b := alice.dial(instances[0]), bob.dial(instances[1])
	bob.join(instances[0], room)

	// The membership event is asynchronous, so keep sending until it lands.
	received := make(chan *message, 1)
	go func() { received <- b.waitMessage(room) }()
	for range 20 {
		id := newUUID()
		a.send(room, id, "are you there?")
		a.waitAck(id)
		select {
		case <-received:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("bob never received a message after joining")
}

// The same client message ID sent at once through two instances stores one
// message with one sequence.
func TestIdempotentSend(t *testing.T) {
	alice := newUser(t)
	room := alice.createRoom()
	first, second := alice.dial(instances[0]), alice.dial(instances[1]) // one user, two devices

	id := newUUID()
	var wg sync.WaitGroup
	acks := make([]*message, 2)
	for i, s := range []*socket{first, second} {
		wg.Go(func() {
			s.send(room, id, "only once")
			acks[i] = s.waitAck(id)
		})
	}
	wg.Wait()

	if acks[0].ID != acks[1].ID || acks[0].Sequence != acks[1].Sequence {
		t.Fatalf("acks differ: %+v and %+v", acks[0], acks[1])
	}
	if msgs := alice.history(instances[0], room, 0); len(msgs) != 1 {
		t.Fatalf("history has %d messages, want 1", len(msgs))
	}
}

// A client that was offline gets every later message, in order, from its cursor.
func TestCatchUpFromCursor(t *testing.T) {
	alice, bob := newUser(t), newUser(t)
	room := alice.createRoom()
	bob.join(instances[1], room)
	a := alice.dial(instances[0])

	id := newUUID()
	a.send(room, id, "before")
	cursor := a.waitAck(id).Sequence // the last message bob saw before going offline

	for _, text := range []string{"one", "two", "three"} {
		id := newUUID()
		a.send(room, id, text)
		a.waitAck(id)
	}

	msgs := bob.history(instances[1], room, cursor)
	if len(msgs) != 3 {
		t.Fatalf("caught up %d messages, want 3", len(msgs))
	}
	for i, want := range []string{"one", "two", "three"} {
		if msgs[i].Content != want || msgs[i].Sequence != cursor+int64(i)+1 {
			t.Fatalf("message %d is %+v, want %q at sequence %d", i, msgs[i], want, cursor+int64(i)+1)
		}
	}
}

// Identity comes from the session, and room access from membership.
func TestAuthorization(t *testing.T) {
	alice, mallory := newUser(t), newUser(t)
	room := alice.createRoom()
	path := fmt.Sprintf("/api/rooms/%d", room)

	anonymous := &user{t: t, http: &http.Client{Timeout: 10 * time.Second}}
	anonymous.must(http.StatusUnauthorized, instances[0], "GET", path+"/messages", nil, nil)
	anonymous.must(http.StatusUnauthorized, instances[0], "GET", "/ws", nil, nil)

	mallory.must(http.StatusForbidden, instances[0], "GET", path+"/messages", nil, nil)
	mallory.must(http.StatusForbidden, instances[0], "DELETE", path, nil, nil)

	m := mallory.dial(instances[1])
	id := newUUID()
	m.send(room, id, "let me in")
	f := m.wait(func(f frame) bool { return f.ClientMessageID == id })
	if f.Type != "error" {
		t.Fatalf("non-member send got %q, want error", f.Type)
	}
	if msgs := alice.history(instances[0], room, 0); len(msgs) != 0 {
		t.Fatalf("non-member message was stored: %+v", msgs)
	}
}

// A refresh token works once. Using it again revokes the whole login.
func TestRefreshTokenRotation(t *testing.T) {
	alice := newUser(t)
	authURL, _ := url.Parse(instances[0] + "/api/auth/refresh")
	old := alice.http.Jar.Cookies(authURL)

	alice.must(http.StatusOK, instances[0], "POST", "/api/auth/refresh", nil, nil)

	// Replay the old token, as someone who stole it would.
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(authURL, old)
	thief := &user{t: t, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	thief.must(http.StatusUnauthorized, instances[0], "POST", "/api/auth/refresh", nil, nil)

	// The reuse revoked the family, so the newer token is dead too.
	alice.must(http.StatusUnauthorized, instances[1], "POST", "/api/auth/refresh", nil, nil)
}

// Scrolling back: "before" returns the messages just older than a sequence.
func TestOlderHistory(t *testing.T) {
	alice := newUser(t)
	room := alice.createRoom()
	a := alice.dial(instances[0])
	for i := range 5 {
		id := newUUID()
		a.send(room, id, fmt.Sprintf("message %d", i+1))
		a.waitAck(id)
	}

	var msgs []message
	alice.must(http.StatusOK, instances[1], "GET", fmt.Sprintf("/api/rooms/%d/messages?before=4&limit=2", room), nil, &msgs)
	if len(msgs) != 2 || msgs[0].Sequence != 2 || msgs[1].Sequence != 3 {
		t.Fatalf("before=4 limit=2 returned %+v, want sequences 2 and 3", msgs)
	}
}

// Repeated wrong passwords lock the username for a while, on every instance,
// and even the right password is refused during the lock.
func TestLoginThrottle(t *testing.T) {
	alice := newUser(t)
	guesser := &user{t: t, http: &http.Client{Timeout: 10 * time.Second}}
	wrong := map[string]string{"username": alice.name, "password": "wrong guess"}
	right := map[string]string{"username": alice.name, "password": "correct horse"}

	for i := range 10 {
		guesser.must(http.StatusUnauthorized, instances[i%2], "POST", "/api/auth/login", wrong, nil)
	}
	guesser.must(http.StatusTooManyRequests, instances[0], "POST", "/api/auth/login", wrong, nil)
	guesser.must(http.StatusTooManyRequests, instances[1], "POST", "/api/auth/login", right, nil)
}

// Every response carries the headers that restrict what the page may do.
func TestSecurityHeaders(t *testing.T) {
	res, err := http.Get(instances[0] + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if res.Header.Get(name) == "" {
			t.Errorf("missing header %s", name)
		}
	}
}
