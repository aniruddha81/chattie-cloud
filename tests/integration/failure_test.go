//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Failure tests stop and start containers of the local Compose stack, so they
// only run when asked for:
//
//	FAILURE_TESTS=1 go test -tags integration ./tests/integration/
//
// They expect the default CHATTIE_URLS, where the first URL is app1.

func skipUnlessFailureTests(t *testing.T) {
	if os.Getenv("FAILURE_TESTS") == "" {
		t.Skip("set FAILURE_TESTS=1 to run tests that stop containers")
	}
}

// compose runs a docker compose command against the local stack.
func compose(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "-f", "../../deploy/local/compose.yaml"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// eventually retries check until it passes or 30 seconds are up.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if check() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ready(base string) bool {
	res, err := http.Get(base + "/readyz")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func pendingOutbox(t *testing.T) string {
	return compose(t, "exec", "-T", "postgres", "psql", "-U", "chattie", "-tA", "-c",
		"SELECT count(*) FROM outbox WHERE published_at IS NULL")
}

// A message sent while Redis is down is still committed and acknowledged.
// When Redis returns, a client that never disconnected is told to resync and
// finds the message in history.
func TestRedisOutage(t *testing.T) {
	skipUnlessFailureTests(t)
	alice, bob := newUser(t), newUser(t)
	room := alice.createRoom()
	bob.join(instances[1], room)
	a, b := alice.dial(instances[0]), bob.dial(instances[1])

	compose(t, "stop", "redis")
	t.Cleanup(func() { compose(t, "start", "redis") })

	id := newUUID()
	a.send(room, id, "sent while redis was down")
	acked := a.waitAck(id) // Postgres is enough to commit

	compose(t, "start", "redis")
	b.waitFor(30*time.Second, func(f frame) bool { return f.Type == "resync" })

	msgs := bob.history(instances[1], room, 0)
	if len(msgs) != 1 || msgs[0].ID != acked.ID {
		t.Fatalf("history after resync is %+v, want message %d", msgs, acked.ID)
	}
}

// An instance dies while messages are in flight. The client reconnects to
// another instance and resends everything it has no acknowledgement for, with
// the same IDs. Every message ends up stored exactly once.
func TestInstanceKilledDuringSends(t *testing.T) {
	skipUnlessFailureTests(t)
	alice := newUser(t)
	room := alice.createRoom()
	a := alice.dial(instances[0])

	ids := make([]string, 20)
	for i := range ids {
		ids[i] = newUUID()
	}
	text := func(i int) string { return fmt.Sprintf("message %d", i+1) }

	for i := range 10 { // sent and acknowledged
		a.send(room, ids[i], text(i))
		a.waitAck(ids[i])
	}
	for i := 10; i < 15; i++ { // sent, fate unknown
		a.send(room, ids[i], text(i))
	}
	compose(t, "kill", "app1")
	t.Cleanup(func() {
		compose(t, "start", "app1")
		eventually(t, "app1 to be ready again", func() bool { return ready(instances[0]) })
	})

	b := alice.dial(instances[1])
	for i := 10; i < 20; i++ { // resend the unacknowledged, then the rest
		b.send(room, ids[i], text(i))
		b.waitAck(ids[i])
	}

	msgs := alice.history(instances[1], room, 0)
	if len(msgs) != 20 {
		t.Fatalf("history has %d messages, want 20", len(msgs))
	}
	seen := map[string]bool{}
	for i, m := range msgs {
		if m.Sequence != int64(i+1) || seen[m.Content] {
			t.Fatalf("message %d is %+v: want sequence %d and no duplicates", i, m, i+1)
		}
		seen[m.Content] = true
	}
}

// With the publisher stopped, a send is still committed and acknowledged, and
// its event waits in the outbox. When the publisher returns, the event is
// delivered live and the outbox empties.
func TestPublisherRestart(t *testing.T) {
	skipUnlessFailureTests(t)
	alice, bob := newUser(t), newUser(t)
	room := alice.createRoom()
	bob.join(instances[1], room)
	a, b := alice.dial(instances[0]), bob.dial(instances[1])

	compose(t, "stop", "publisher")
	t.Cleanup(func() { compose(t, "start", "publisher") })

	id := newUUID()
	a.send(room, id, "sent while the publisher was down")
	acked := a.waitAck(id)
	if pendingOutbox(t) == "0" {
		t.Fatal("the outbox is empty although nothing is publishing")
	}

	compose(t, "start", "publisher")
	got := b.waitFor(30*time.Second, func(f frame) bool { return f.Type == "message" && f.Message.RoomID == room })
	if got.Message.ID != acked.ID {
		t.Fatalf("bob got message %d, want %d", got.Message.ID, acked.ID)
	}
	eventually(t, "the outbox to empty", func() bool { return pendingOutbox(t) == "0" })
}
