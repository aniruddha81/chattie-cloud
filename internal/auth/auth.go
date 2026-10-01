// Package auth handles passwords, access tokens and refresh tokens.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour
)

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Tokens signs and verifies access tokens. A token is "payload.signature":
// the payload names the user and an expiry, and the HMAC signature proves
// this server issued it, so verifying needs no database lookup.
type Tokens struct {
	Secret []byte
}

type claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"name"`
	Expires  int64  `json:"exp"`
}

func (t Tokens) Sign(u model.User) string {
	payload, _ := json.Marshal(claims{u.ID, u.Username, time.Now().Add(AccessTTL).Unix()})
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + t.signature(body)
}

func (t Tokens) Verify(token string) (model.User, bool) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(t.signature(body))) {
		return model.User{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return model.User{}, false
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil || time.Now().Unix() >= c.Expires {
		return model.User{}, false
	}
	return model.User{ID: c.UserID, Username: c.Username}, true
}

func (t Tokens) signature(body string) string {
	mac := hmac.New(sha256.New, t.Secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// NewRefreshToken returns a random token for the client and the hash the
// server stores. The token itself is never saved.
func NewRefreshToken() (token string, hash []byte) {
	token = rand.Text()
	return token, HashToken(token)
}

func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
