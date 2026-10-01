// Package model holds the types shared by the store, the bus and the API.
package model

import "time"

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Room struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"` // for a DM, the other person's username
	Kind         string `json:"kind"` // "public" or "dm"
	Joined       bool   `json:"joined"`
	Owner        bool   `json:"owner"`
	LastSequence int64  `json:"last_sequence"`
}

type Message struct {
	ID              int64     `json:"id"`
	RoomID          int64     `json:"room_id"`
	Sequence        int64     `json:"sequence"`
	SenderID        int64     `json:"sender_id"`
	Sender          string    `json:"sender"`
	ClientMessageID string    `json:"client_message_id"`
	Content         string    `json:"content"`
	CreatedAt       time.Time `json:"created_at"`
}

// Event types sent between instances through Redis.
const (
	EventMessage     = "message"      // a message was committed
	EventMembership  = "membership"   // a user joined or left a room
	EventRoomCreated = "room_created" // a public room was created
	EventRoomDeleted = "room_deleted" // a room was deleted
	EventTyping      = "typing"       // ephemeral, never stored
)

// Event tells other instances that something changed. It carries IDs only:
// receivers load the real data from Postgres, which stays the authority.
type Event struct {
	Type      string `json:"type"`
	RoomID    int64  `json:"room_id,omitempty"`
	MessageID int64  `json:"message_id,omitempty"`
	Sequence  int64  `json:"sequence,omitempty"`
	UserID    int64  `json:"user_id,omitempty"`
	Username  string `json:"username,omitempty"`
}
