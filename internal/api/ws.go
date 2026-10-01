package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/aniruddha81/chattie-cloud/internal/hub"
	"github.com/aniruddha81/chattie-cloud/internal/model"
	"github.com/aniruddha81/chattie-cloud/internal/store/postgres"
)

const (
	maxFrameBytes = 8 << 10 // largest frame accepted from a client
	maxContent    = 4000    // bytes in one message
	writeTimeout  = 10 * time.Second
	sendRate      = 5  // frames per second a client may send
	sendBurst     = 20 // short bursts above that rate
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// inbound is a frame from the client: "message" or "typing".
type inbound struct {
	Type            string `json:"type"`
	RoomID          int64  `json:"room_id"`
	ClientMessageID string `json:"client_message_id"`
	Content         string `json:"content"`
}

// frame is anything the server sends to a client. Type says which fields matter:
//
//	hello          instance, online            sent once after connecting
//	message        message                     a new message in a joined room
//	ack            client_message_id, message  your send was committed
//	error          client_message_id, error    your send failed; retry says whether to resend
//	typing         room_id, username
//	heartbeat      sequences, online           newest sequence of each joined room
//	resync         live events may have been missed: catch up from history
//	rooms_changed  rooms or memberships changed: reload the room list
//	reconnect      this instance is shutting down: connect again
type frame struct {
	Type            string          `json:"type"`
	Message         *model.Message  `json:"message,omitempty"`
	ClientMessageID string          `json:"client_message_id,omitempty"`
	RoomID          int64           `json:"room_id,omitempty"`
	Username        string          `json:"username,omitempty"`
	Error           string          `json:"error,omitempty"`
	Retry           bool            `json:"retry,omitempty"`
	Instance        string          `json:"instance,omitempty"`
	Sequences       map[int64]int64 `json:"sequences,omitempty"`
	Online          []string        `json:"online,omitempty"`
}

func encode(f frame) []byte {
	data, _ := json.Marshal(f)
	return data
}

// socket runs one WebSocket connection until it closes.
func (s *Server) socket(w http.ResponseWriter, r *http.Request, u model.User) {
	if s.draining.Load() {
		httpError(w, http.StatusServiceUnavailable, "draining")
		return
	}
	// Accept rejects browsers whose Origin differs from the Host.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxFrameBytes)

	ctx, cancel := context.WithCancel(s.sockets)
	defer cancel()
	c := hub.NewConn(rand.Text(), u, cancel)

	s.hub.Add(c)
	defer s.hub.Remove(c)
	if err := s.syncRooms(ctx, u.ID); err != nil {
		slog.Error("load rooms", "err", err)
		ws.Close(websocket.StatusTryAgainLater, "try again")
		return
	}

	// Presence is best effort: errors are ignored and leases expire on their own.
	lease := presenceMember(c)
	s.bus.Renew(ctx, s.presenceTTL(), lease)
	defer s.bus.Forget(context.Background(), lease)
	online, _ := s.bus.Online(ctx)
	c.Push(encode(frame{Type: "hello", Instance: s.cfg.InstanceID, Online: online}))

	go write(ctx, ws, c)
	s.read(ctx, ws, c)
}

// write sends queued frames to the client.
func write(ctx context.Context, ws *websocket.Conn, c *hub.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-c.Out():
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := ws.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.Close()
				return
			}
		}
	}
}

// read handles frames from the client until the socket closes.
func (s *Server) read(ctx context.Context, ws *websocket.Conn, c *hub.Conn) {
	limiter := rate.NewLimiter(sendRate, sendBurst)
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var in inbound
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		switch in.Type {
		case "message":
			if !limiter.Allow() {
				c.Push(encode(frame{Type: "error", ClientMessageID: in.ClientMessageID, Error: "slow down", Retry: true}))
				continue
			}
			s.send(ctx, c, in)
		case "typing":
			if limiter.Allow() && s.hub.InRoom(c, in.RoomID) {
				event, _ := json.Marshal(model.Event{Type: model.EventTyping, RoomID: in.RoomID, Username: c.User.Username})
				s.bus.Publish(ctx, event)
			}
		}
	}
}

// send commits a message and acknowledges it. The acknowledgement is only
// sent after Postgres has the message; other clients hear about it through
// the outbox and Redis.
func (s *Server) send(ctx context.Context, c *hub.Conn, in inbound) {
	content := strings.TrimSpace(in.Content)
	if !uuidRE.MatchString(in.ClientMessageID) || content == "" || len(content) > maxContent {
		c.Push(encode(frame{Type: "error", ClientMessageID: in.ClientMessageID, Error: "invalid message"}))
		return
	}
	msg, err := s.store.SendMessage(ctx, c.User.ID, in.RoomID, in.ClientMessageID, content)
	switch {
	case err == nil:
		c.Push(encode(frame{Type: "ack", ClientMessageID: in.ClientMessageID, Message: &msg}))
	case errors.Is(err, postgres.ErrForbidden):
		c.Push(encode(frame{Type: "error", ClientMessageID: in.ClientMessageID, Error: "you are not in this room"}))
	default:
		slog.Error("send message", "err", err)
		c.Push(encode(frame{Type: "error", ClientMessageID: in.ClientMessageID, Error: "could not save the message", Retry: true}))
	}
}

// onEvent handles an event from Redis by telling the affected local sockets.
// Events arrive one at a time, in the order Redis delivered them.
func (s *Server) onEvent(data []byte) {
	var e model.Event
	if json.Unmarshal(data, &e) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch e.Type {
	case model.EventMessage:
		if !s.hub.HasRoom(e.RoomID) {
			return
		}
		msg, err := s.store.GetMessage(ctx, e.MessageID)
		if err != nil {
			slog.Warn("load message for delivery", "err", err) // clients catch up from the next heartbeat
			return
		}
		s.hub.ToRoom(e.RoomID, encode(frame{Type: "message", Message: &msg}))
	case model.EventTyping:
		s.hub.ToRoom(e.RoomID, encode(frame{Type: "typing", RoomID: e.RoomID, Username: e.Username}))
	case model.EventMembership:
		if !s.hub.HasUser(e.UserID) {
			return
		}
		if err := s.syncRooms(ctx, e.UserID); err != nil {
			slog.Warn("sync rooms", "err", err)
		}
		s.hub.ToUser(e.UserID, encode(frame{Type: "rooms_changed"}))
	case model.EventRoomCreated:
		s.hub.ToAll(encode(frame{Type: "rooms_changed"}))
	case model.EventRoomDeleted:
		s.hub.DropRoom(e.RoomID)
		s.hub.ToAll(encode(frame{Type: "rooms_changed"}))
	}
}

// onResync runs after the Redis subscription was down and came back. Events
// were lost in between, so reload memberships and tell every client to catch
// up from history.
func (s *Server) onResync() {
	slog.Warn("redis subscription restored, resyncing clients")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, userID := range s.hub.Users() {
		if err := s.syncRooms(ctx, userID); err != nil {
			slog.Warn("sync rooms", "err", err)
		}
	}
	s.hub.ToAll(encode(frame{Type: "resync"}))
}

// syncRooms reloads a user's memberships from Postgres into the hub. Events
// only say that something changed; the database decides who is in a room.
// The lock makes reloads apply one after another, so the last one to finish
// is also the one that read the newest state.
func (s *Server) syncRooms(ctx context.Context, userID int64) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	ids, err := s.store.RoomIDsForUser(ctx, userID)
	if err != nil {
		return err
	}
	s.hub.SetRooms(userID, ids)
	return nil
}

func (s *Server) heartbeats(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.heartbeat(ctx)
		}
	}
}

// heartbeat renews presence leases and sends every socket the newest sequence
// of its rooms. A client that sees a sequence ahead of its own knows it missed
// a message, even in a room where nothing else is being said.
func (s *Server) heartbeat(ctx context.Context) {
	snaps := s.hub.Snapshot()
	if len(snaps) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	leases := make([]string, len(snaps))
	seen := map[int64]bool{}
	var roomIDs []int64
	for i, snap := range snaps {
		leases[i] = presenceMember(snap.Conn)
		for _, id := range snap.Rooms {
			if !seen[id] {
				seen[id] = true
				roomIDs = append(roomIDs, id)
			}
		}
	}
	s.bus.Renew(ctx, s.presenceTTL(), leases...)
	online, _ := s.bus.Online(ctx)
	latest, err := s.store.LastSequences(ctx, roomIDs)
	if err != nil {
		slog.Warn("heartbeat sequences", "err", err)
	}

	for _, snap := range snaps {
		sequences := make(map[int64]int64, len(snap.Rooms))
		for _, id := range snap.Rooms {
			if seq, ok := latest[id]; ok {
				sequences[id] = seq
			}
		}
		snap.Conn.Push(encode(frame{Type: "heartbeat", Sequences: sequences, Online: online}))
	}
}

func presenceMember(c *hub.Conn) string { return c.User.Username + ":" + c.ID }

// presenceTTL lets a lease survive two missed heartbeats.
func (s *Server) presenceTTL() time.Duration { return 3 * s.cfg.Heartbeat }
