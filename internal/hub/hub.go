// Package hub tracks the WebSocket connections of one instance and which
// rooms each belongs to. It only knows local sockets; other instances are
// reached through the bus.
package hub

import (
	"sync"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

// queueSize bounds the frames waiting to be written to one client.
const queueSize = 64

// Conn is one WebSocket. A user may have several (tabs, devices).
type Conn struct {
	ID   string
	User model.User

	out   chan []byte
	close func()
	rooms map[int64]bool // guarded by Hub.mu
}

// NewConn makes a connection; close must end the socket and be safe to call
// more than once.
func NewConn(id string, user model.User, close func()) *Conn {
	return &Conn{ID: id, User: user, out: make(chan []byte, queueSize), close: close, rooms: map[int64]bool{}}
}

// Out is the queue of frames to write to the socket.
func (c *Conn) Out() <-chan []byte { return c.out }

func (c *Conn) Close() { c.close() }

// Push queues a frame without blocking. A client too slow to drain its queue
// is disconnected; it reconnects and catches up from history.
func (c *Conn) Push(frame []byte) {
	select {
	case c.out <- frame:
	default:
		c.close()
	}
}

type Hub struct {
	mu    sync.Mutex
	users map[int64]map[*Conn]bool
	rooms map[int64]map[*Conn]bool
}

func New() *Hub {
	return &Hub{users: map[int64]map[*Conn]bool{}, rooms: map[int64]map[*Conn]bool{}}
}

func (h *Hub) Add(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.users[c.User.ID] == nil {
		h.users[c.User.ID] = map[*Conn]bool{}
	}
	h.users[c.User.ID][c] = true
}

func (h *Hub) Remove(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaveAll(c)
	delete(h.users[c.User.ID], c)
	if len(h.users[c.User.ID]) == 0 {
		delete(h.users, c.User.ID)
	}
}

// SetRooms replaces the room list of every connection of a user.
func (h *Hub) SetRooms(userID int64, roomIDs []int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.users[userID] {
		h.leaveAll(c)
		for _, id := range roomIDs {
			c.rooms[id] = true
			if h.rooms[id] == nil {
				h.rooms[id] = map[*Conn]bool{}
			}
			h.rooms[id][c] = true
		}
	}
}

// DropRoom removes a deleted room from every connection.
func (h *Hub) DropRoom(roomID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[roomID] {
		delete(c.rooms, roomID)
	}
	delete(h.rooms, roomID)
}

func (h *Hub) leaveAll(c *Conn) {
	for id := range c.rooms {
		delete(h.rooms[id], c)
		if len(h.rooms[id]) == 0 {
			delete(h.rooms, id)
		}
	}
	clear(c.rooms)
}

func (h *Hub) InRoom(c *Conn, roomID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return c.rooms[roomID]
}

// HasRoom reports whether any local connection is in the room.
func (h *Hub) HasRoom(roomID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms[roomID]) > 0
}

// HasUser reports whether the user has a connection on this instance.
func (h *Hub) HasUser(userID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.users[userID]) > 0
}

func (h *Hub) ToRoom(roomID int64, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[roomID] {
		c.Push(frame)
	}
}

func (h *Hub) ToUser(userID int64, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.users[userID] {
		c.Push(frame)
	}
}

func (h *Hub) ToAll(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, conns := range h.users {
		for c := range conns {
			c.Push(frame)
		}
	}
}

// Users lists the users connected to this instance.
func (h *Hub) Users() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]int64, 0, len(h.users))
	for id := range h.users {
		ids = append(ids, id)
	}
	return ids
}

// Snapshot is a connection with a copy of its room list.
type Snapshot struct {
	Conn  *Conn
	Rooms []int64
}

func (h *Hub) Snapshot() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	var snaps []Snapshot
	for _, conns := range h.users {
		for c := range conns {
			rooms := make([]int64, 0, len(c.rooms))
			for id := range c.rooms {
				rooms = append(rooms, id)
			}
			snaps = append(snaps, Snapshot{Conn: c, Rooms: rooms})
		}
	}
	return snaps
}
