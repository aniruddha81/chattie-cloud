package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

// ListRooms returns every public room plus the user's own DMs.
func (s *Store) ListRooms(ctx context.Context, userID int64) ([]model.Room, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT r.id,
		       CASE WHEN r.kind = 'dm' THEN COALESCE((
		           SELECT u.username FROM memberships o JOIN users u ON u.id = o.user_id
		           WHERE o.room_id = r.id AND o.user_id <> $1), '')
		       ELSE r.name END,
		       r.kind,
		       m.user_id IS NOT NULL,
		       COALESCE(r.created_by = $1, false),
		       r.last_sequence
		FROM rooms r
		LEFT JOIN memberships m ON m.room_id = r.id AND m.user_id = $1
		WHERE r.deleted_at IS NULL AND (r.kind = 'public' OR m.user_id IS NOT NULL)
		ORDER BY r.kind DESC, 2`, userID)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[model.Room])
}

func (s *Store) CreateRoom(ctx context.Context, userID int64, name string) (model.Room, error) {
	room := model.Room{Name: name, Kind: "public", Joined: true, Owner: true}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO rooms (name, kind, created_by) VALUES ($1, 'public', $2) RETURNING id`,
			name, userID).Scan(&room.ID)
		if err != nil {
			return err
		}
		if err := addMember(ctx, tx, room.ID, userID); err != nil {
			return err
		}
		return addEvent(ctx, tx, model.Event{Type: model.EventRoomCreated, RoomID: room.ID})
	})
	if isUniqueViolation(err) {
		return room, ErrConflict
	}
	return room, err
}

func (s *Store) JoinRoom(ctx context.Context, userID, roomID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var open bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM rooms WHERE id = $1 AND kind = 'public' AND deleted_at IS NULL)`,
			roomID).Scan(&open)
		if err != nil {
			return err
		}
		if !open {
			return ErrNotFound
		}
		return addMember(ctx, tx, roomID, userID)
	})
}

func (s *Store) LeaveRoom(ctx context.Context, userID, roomID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM memberships m USING rooms r
			WHERE r.id = m.room_id AND r.kind = 'public' AND m.room_id = $1 AND m.user_id = $2`,
			roomID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return addEvent(ctx, tx, model.Event{Type: model.EventMembership, RoomID: roomID, UserID: userID})
	})
}

// DeleteRoom marks a room deleted. Only its creator may do this. Its messages
// stay in the database but are no longer served.
func (s *Store) DeleteRoom(ctx context.Context, userID, roomID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE rooms SET deleted_at = now()
			WHERE id = $1 AND created_by = $2 AND deleted_at IS NULL`, roomID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrForbidden
		}
		return addEvent(ctx, tx, model.Event{Type: model.EventRoomDeleted, RoomID: roomID})
	})
}

// OpenDM returns the direct-message room between two users, creating it on
// first use.
func (s *Store) OpenDM(ctx context.Context, userID int64, username string) (model.Room, error) {
	room := model.Room{Name: username, Kind: "dm", Joined: true}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var other int64
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1 AND id <> $2`, username, userID).Scan(&other)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%d:%d", min(userID, other), max(userID, other))
		// The no-op update makes RETURNING work whether or not the DM exists.
		err = tx.QueryRow(ctx, `
			INSERT INTO rooms (name, kind, dm_key) VALUES ('', 'dm', $1)
			ON CONFLICT (dm_key) DO UPDATE SET dm_key = EXCLUDED.dm_key
			RETURNING id, last_sequence`, key).Scan(&room.ID, &room.LastSequence)
		if err != nil {
			return err
		}
		if err := addMember(ctx, tx, room.ID, userID); err != nil {
			return err
		}
		return addMember(ctx, tx, room.ID, other)
	})
	return room, err
}

// IsMember is the authorization check for reading a room.
func (s *Store) IsMember(ctx context.Context, userID, roomID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM memberships m JOIN rooms r ON r.id = m.room_id
			WHERE m.room_id = $1 AND m.user_id = $2 AND r.deleted_at IS NULL)`,
		roomID, userID).Scan(&ok)
	return ok, err
}

func (s *Store) RoomIDsForUser(ctx context.Context, userID int64) ([]int64, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT m.room_id FROM memberships m JOIN rooms r ON r.id = m.room_id
		WHERE m.user_id = $1 AND r.deleted_at IS NULL`, userID)
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// LastSequences returns the newest message sequence of each given room.
func (s *Store) LastSequences(ctx context.Context, roomIDs []int64) (map[int64]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, last_sequence FROM rooms WHERE id = ANY($1)`, roomIDs)
	if err != nil {
		return nil, err
	}
	latest := make(map[int64]int64, len(roomIDs))
	var id, seq int64
	_, err = pgx.ForEachRow(rows, []any{&id, &seq}, func() error {
		latest[id] = seq
		return nil
	})
	return latest, err
}

func addMember(ctx context.Context, tx pgx.Tx, roomID, userID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO memberships (room_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, roomID, userID)
	if err != nil {
		return err
	}
	return addEvent(ctx, tx, model.Event{Type: model.EventMembership, RoomID: roomID, UserID: userID})
}
