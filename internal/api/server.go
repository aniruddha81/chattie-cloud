// Package api serves the REST endpoints, the WebSocket and the web client.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aniruddha81/chattie-cloud/internal/auth"
	"github.com/aniruddha81/chattie-cloud/internal/bus"
	"github.com/aniruddha81/chattie-cloud/internal/config"
	"github.com/aniruddha81/chattie-cloud/internal/hub"
	"github.com/aniruddha81/chattie-cloud/internal/store/postgres"
	"github.com/aniruddha81/chattie-cloud/web"
)

type Server struct {
	cfg    config.Config
	store  *postgres.Store
	bus    *bus.Bus
	hub    *hub.Hub
	tokens auth.Tokens

	draining     atomic.Bool     // true once shutdown has begun
	sockets      context.Context // cancelled to close every WebSocket
	closeSockets context.CancelFunc
	syncMu       sync.Mutex // see syncRooms
}

func New(cfg config.Config, store *postgres.Store, events *bus.Bus) *Server {
	s := &Server{cfg: cfg, store: store, bus: events, hub: hub.New(), tokens: auth.Tokens{Secret: cfg.Secret}}
	s.sockets, s.closeSockets = context.WithCancel(context.Background())
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)

	mux.HandleFunc("POST /api/auth/register", s.register)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/refresh", s.refresh)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.authed(s.me))

	mux.HandleFunc("GET /api/rooms", s.authed(s.listRooms))
	mux.HandleFunc("POST /api/rooms", s.authed(s.createRoom))
	mux.HandleFunc("POST /api/rooms/{id}/join", s.authed(s.joinRoom))
	mux.HandleFunc("POST /api/rooms/{id}/leave", s.authed(s.leaveRoom))
	mux.HandleFunc("DELETE /api/rooms/{id}", s.authed(s.deleteRoom))
	mux.HandleFunc("GET /api/rooms/{id}/messages", s.authed(s.listMessages))
	mux.HandleFunc("POST /api/dms", s.authed(s.openDM))

	mux.HandleFunc("GET /ws", s.authed(s.socket))
	mux.Handle("GET /", http.FileServerFS(web.Files))

	// Rejects state-changing requests that come from another site (CSRF).
	return s.secure(http.NewCrossOriginProtection().Handler(mux))
}

// A plain host name with an optional port, safe to copy into a header.
var hostRE = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]+)?$`)

// secure adds headers that limit what a browser lets the page do. The content
// policy allows scripts, styles and connections only from this site, so text
// that someone injects into the page cannot run or send data elsewhere.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connect := "'self'"
		if hostRE.MatchString(r.Host) {
			// Older browsers do not count a WebSocket to this site as 'self'.
			connect += " ws://" + r.Host + " wss://" + r.Host
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src "+connect+
			"; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff") // do not guess file types
		h.Set("X-Frame-Options", "DENY")           // do not load inside another site
		h.Set("Referrer-Policy", "no-referrer")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000") // always use HTTPS
		}
		next.ServeHTTP(w, r)
	})
}

// Run serves until ctx is cancelled, then drains: it fails readiness so the
// load balancer stops sending traffic, asks clients to reconnect elsewhere,
// waits, and closes what is left.
func (s *Server) Run(ctx context.Context) error {
	background, stop := context.WithCancel(context.Background())
	defer stop()
	go s.bus.Subscribe(background, s.onEvent, s.onResync)
	go s.heartbeats(background)

	srv := &http.Server{Addr: s.cfg.Addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", s.cfg.Addr)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	slog.Info("draining", "connections", len(s.hub.Snapshot()))
	s.draining.Store(true)
	s.hub.ToAll(encode(frame{Type: "reconnect"}))
	time.Sleep(s.cfg.DrainDelay)
	s.closeSockets()

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// ready tells the load balancer whether to send traffic here.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		httpError(w, http.StatusServiceUnavailable, "draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		httpError(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.cfg.InstanceID, "connections": len(s.hub.Snapshot())})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// fail turns a store error into an HTTP response.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrNotFound):
		httpError(w, http.StatusNotFound, "not found")
	case errors.Is(err, postgres.ErrForbidden):
		httpError(w, http.StatusForbidden, "not allowed")
	case errors.Is(err, postgres.ErrConflict):
		httpError(w, http.StatusConflict, "already exists")
	default:
		slog.Error("request failed", "err", err)
		httpError(w, http.StatusInternalServerError, "something went wrong")
	}
}

// readJSON decodes a small JSON body, answering 400 itself on bad input.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// roomID reads the {id} path value, answering 404 itself if it is not a number.
func roomID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}
