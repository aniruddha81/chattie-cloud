package hub

import (
	"testing"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

func newConn(id string, userID int64, closed *bool) *Conn {
	return NewConn(id, model.User{ID: userID}, func() { *closed = true })
}

// A client that does not read its queue is disconnected once the queue is
// full, and it does not hold up anyone else in the room.
func TestSlowClientIsDisconnected(t *testing.T) {
	h := New()
	var slowClosed, fastClosed bool
	slow, fast := newConn("slow", 1, &slowClosed), newConn("fast", 2, &fastClosed)
	h.Add(slow)
	h.Add(fast)
	h.SetRooms(1, []int64{7})
	h.SetRooms(2, []int64{7})

	for range queueSize + 1 {
		h.ToRoom(7, []byte("frame"))
		<-fast.Out() // the fast client reads; the slow one never does
	}

	if !slowClosed {
		t.Error("slow client was not disconnected")
	}
	if fastClosed {
		t.Error("fast client was disconnected")
	}
}

// Frames go only to connections in the room, and to every connection of a
// user who has several.
func TestRoomDelivery(t *testing.T) {
	h := New()
	var closed bool
	phone, laptop, other := newConn("phone", 1, &closed), newConn("laptop", 1, &closed), newConn("other", 2, &closed)
	h.Add(phone)
	h.Add(laptop)
	h.Add(other)
	h.SetRooms(1, []int64{7})
	h.SetRooms(2, []int64{8})

	h.ToRoom(7, []byte("hello"))
	if len(phone.Out()) != 1 || len(laptop.Out()) != 1 {
		t.Error("both of the member's connections should get the frame")
	}
	if len(other.Out()) != 0 {
		t.Error("a non-member got the frame")
	}
}

// Leaving a room, a deleted room and a closed connection all stop delivery.
func TestMembershipChanges(t *testing.T) {
	h := New()
	var closed bool
	c := newConn("c", 1, &closed)
	h.Add(c)

	h.SetRooms(1, []int64{7, 8})
	h.SetRooms(1, []int64{8}) // left room 7
	if h.InRoom(c, 7) || !h.InRoom(c, 8) {
		t.Error("SetRooms should replace the room list")
	}

	h.DropRoom(8)
	if h.InRoom(c, 8) || h.HasRoom(8) {
		t.Error("DropRoom should remove the room")
	}

	h.SetRooms(1, []int64{9})
	h.Remove(c)
	if h.HasUser(1) || h.HasRoom(9) {
		t.Error("Remove should forget the connection")
	}
}
