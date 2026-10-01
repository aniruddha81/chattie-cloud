package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aniruddha81/chattie-cloud/internal/auth"
	"github.com/aniruddha81/chattie-cloud/internal/model"
	"github.com/aniruddha81/chattie-cloud/internal/store/postgres"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// After this many failed sign-ins for one username, further attempts are
// refused until the window (see bus.LoginFailed) ends.
const maxLoginFailures = 10

// dummyHash is checked when the username does not exist, so a sign-in takes
// the same time whether or not the account is real.
var dummyHash, _ = auth.HashPassword("no such account")

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authedHandler is a handler that needs a signed-in user.
type authedHandler func(http.ResponseWriter, *http.Request, model.User)

// authed takes the user from the signed access cookie. Handlers never trust a
// user ID sent by the client.
func (s *Server) authed(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("access"); err == nil {
			if u, ok := s.tokens.Verify(c.Value); ok {
				next(w, r, u)
				return
			}
		}
		httpError(w, http.StatusUnauthorized, "sign in required")
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if !usernameRE.MatchString(in.Username) {
		httpError(w, http.StatusBadRequest, "username must be 3-20 letters, digits or underscores")
		return
	}
	if len(in.Password) < 8 || len(in.Password) > 72 { // 72 bytes is bcrypt's limit
		httpError(w, http.StatusBadRequest, "password must be 8-72 characters")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := s.store.CreateUser(r.Context(), in.Username, hash)
	if errors.Is(err, postgres.ErrConflict) {
		httpError(w, http.StatusConflict, "username is taken")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u, http.StatusCreated)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	name := strings.ToLower(strings.TrimSpace(in.Username))
	if !usernameRE.MatchString(name) {
		httpError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	// Throttling is best effort: if Redis is down, sign-in still works.
	if failures, _ := s.bus.LoginFailures(ctx, name); failures >= maxLoginFailures {
		httpError(w, http.StatusTooManyRequests, "too many failed sign-ins, try again in 15 minutes")
		return
	}

	u, hash, err := s.store.UserByUsername(ctx, name)
	found := err == nil
	if err != nil && !errors.Is(err, postgres.ErrNotFound) {
		fail(w, err)
		return
	}
	if !found {
		hash = dummyHash
	}
	if !auth.CheckPassword(hash, in.Password) || !found {
		s.bus.LoginFailed(ctx, name)
		httpError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.bus.LoginSucceeded(ctx, name)
	s.startSession(w, r, u, http.StatusOK)
}

// startSession begins a new login: a fresh refresh-token family and cookies.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u model.User, status int) {
	token, hash := auth.NewRefreshToken()
	if err := s.store.SaveRefreshToken(r.Context(), u.ID, hash, time.Now().Add(auth.RefreshTTL)); err != nil {
		fail(w, err)
		return
	}
	s.setCookies(w, s.tokens.Sign(u), token)
	writeJSON(w, status, u)
}

// refresh trades the refresh token for a new one and a new access token.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("refresh")
	if err != nil {
		httpError(w, http.StatusUnauthorized, "sign in required")
		return
	}
	token, hash := auth.NewRefreshToken()
	u, err := s.store.RotateRefreshToken(r.Context(), auth.HashToken(c.Value), hash, time.Now().Add(auth.RefreshTTL))
	if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, postgres.ErrForbidden) {
		s.clearCookies(w)
		httpError(w, http.StatusUnauthorized, "sign in required")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.setCookies(w, s.tokens.Sign(u), token)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("refresh"); err == nil {
		if err := s.store.RevokeRefreshToken(r.Context(), auth.HashToken(c.Value)); err != nil {
			fail(w, err)
			return
		}
	}
	s.clearCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u model.User) {
	writeJSON(w, http.StatusOK, u)
}

// Both cookies are HttpOnly, so page scripts cannot read them. The refresh
// cookie is only sent to the auth endpoints.
func (s *Server) setCookies(w http.ResponseWriter, access, refresh string) {
	http.SetCookie(w, &http.Cookie{
		Name: "access", Value: access, Path: "/", MaxAge: int(auth.AccessTTL.Seconds()),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: "refresh", Value: refresh, Path: "/api/auth", MaxAge: int(auth.RefreshTTL.Seconds()),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "access", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "refresh", Path: "/api/auth", MaxAge: -1})
}
