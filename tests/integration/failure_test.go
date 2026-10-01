//go:build integration

package integration

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Failure tests stop and start containers of the local Compose stack, so they
// only run when asked for:
//
//	FAILURE_TESTS=1 go test -tags integration -run Outage ./tests/integration/

func compose(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "-f", "../../deploy/local/compose.yaml"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
}

// A message sent while Redis is down is still committed and acknowledged.
// When Redis returns, a client that never disconnected is told to resync and
// finds the message in history.
func TestRedisOutage(t *testing.T) {
	if os.Getenv("FAILURE_TESTS") == "" {
		t.Skip("set FAILURE_TESTS=1 to run tests that stop containers")
	}
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
