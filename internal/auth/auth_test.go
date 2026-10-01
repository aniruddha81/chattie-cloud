package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

var tokens = Tokens{Secret: []byte("0123456789012345678901234567890123456789")}

func TestTokenRoundTrip(t *testing.T) {
	want := model.User{ID: 42, Username: "alice"}
	got, ok := tokens.Verify(tokens.Sign(want))
	if !ok || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, ok, want)
	}
}

// A token is rejected if it was changed, signed with another secret, or expired.
func TestTokenRejected(t *testing.T) {
	alice := tokens.Sign(model.User{ID: 42, Username: "alice"})
	admin := tokens.Sign(model.User{ID: 1, Username: "admin"})

	// Alice's signature on someone else's payload.
	forged := admin[:len(admin)-43] + alice[len(alice)-43:]
	if _, ok := tokens.Verify(forged); ok {
		t.Error("accepted a payload with the wrong signature")
	}

	other := Tokens{Secret: []byte("another-secret-another-secret-another-secret")}
	if _, ok := other.Verify(alice); ok {
		t.Error("accepted a token signed with another secret")
	}

	payload, _ := json.Marshal(claims{42, "alice", time.Now().Add(-time.Minute).Unix()})
	body := base64.RawURLEncoding.EncodeToString(payload)
	if _, ok := tokens.Verify(body + "." + tokens.signature(body)); ok {
		t.Error("accepted an expired token")
	}

	for _, junk := range []string{"", "no-dot", "a.b", "."} {
		if _, ok := tokens.Verify(junk); ok {
			t.Errorf("accepted %q", junk)
		}
	}
}

func TestPassword(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(hash, "correct horse") {
		t.Error("rejected the right password")
	}
	if CheckPassword(hash, "wrong horse") {
		t.Error("accepted the wrong password")
	}
}

// The stored hash of a refresh token matches the token and nothing else.
func TestRefreshToken(t *testing.T) {
	token, hash := NewRefreshToken()
	other, _ := NewRefreshToken()
	if string(HashToken(token)) != string(hash) {
		t.Error("hash does not match its token")
	}
	if token == other || string(HashToken(other)) == string(hash) {
		t.Error("two tokens should differ")
	}
}
