package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aniruddha81/chattie-cloud/internal/model"
	"github.com/aniruddha81/chattie-cloud/internal/store/postgres"
)

var roomNameRE = regexp.MustCompile(`^[a-z0-9_-]{2,30}$`)

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request, u model.User) {
	rooms, err := s.store.ListRooms(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, u model.User) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !roomNameRE.MatchString(name) {
		httpError(w, http.StatusBadRequest, "room name must be 2-30 letters, digits, dashes or underscores")
		return
	}
	room, err := s.store.CreateRoom(r.Context(), u.ID, name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

func (s *Server) joinRoom(w http.ResponseWriter, r *http.Request, u model.User) {
	s.changeRoom(w, r, u, s.store.JoinRoom)
}

func (s *Server) leaveRoom(w http.ResponseWriter, r *http.Request, u model.User) {
	s.changeRoom(w, r, u, s.store.LeaveRoom)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request, u model.User) {
	s.changeRoom(w, r, u, s.store.DeleteRoom)
}

// changeRoom runs a store action on the room named in the URL.
func (s *Server) changeRoom(w http.ResponseWriter, r *http.Request, u model.User, action func(ctx context.Context, userID, roomID int64) error) {
	id, ok := roomID(w, r)
	if !ok {
		return
	}
	if err := action(r.Context(), u.ID, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) openDM(w http.ResponseWriter, r *http.Request, u model.User) {
	var in struct {
		Username string `json:"username"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	room, err := s.store.OpenDM(r.Context(), u.ID, strings.ToLower(strings.TrimSpace(in.Username)))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// listMessages returns room history to members only.
//
//	?after=<sequence>  messages newer than the cursor, oldest first (catch-up)
//	(no after)         the newest messages, oldest first (first load)
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, u model.User) {
	id, ok := roomID(w, r)
	if !ok {
		return
	}
	member, err := s.store.IsMember(r.Context(), u.ID, id)
	if err != nil {
		fail(w, err)
		return
	}
	if !member {
		fail(w, postgres.ErrForbidden)
		return
	}

	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var msgs []model.Message
	if query.Has("after") {
		after, err := strconv.ParseInt(query.Get("after"), 10, 64)
		if err != nil {
			httpError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
		msgs, err = s.store.MessagesAfter(r.Context(), id, after, limit)
	} else {
		msgs, err = s.store.LatestMessages(r.Context(), id, limit)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}
