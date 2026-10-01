package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

// CreateUser adds a user and puts them in the lobby.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (model.User, error) {
	u := model.User{Username: username}
	err := s.pool.QueryRow(ctx, `
		WITH u AS (
			INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id
		), lobby AS (
			INSERT INTO memberships (room_id, user_id)
			SELECT r.id, u.id FROM rooms r, u
			WHERE r.name = 'lobby' AND r.kind = 'public' AND r.deleted_at IS NULL
		)
		SELECT id FROM u`, username, passwordHash).Scan(&u.ID)
	if isUniqueViolation(err) {
		return u, ErrConflict
	}
	return u, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (model.User, string, error) {
	u := model.User{Username: username}
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE username = $1`, username).Scan(&u.ID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, "", ErrNotFound
	}
	return u, hash, err
}

// SaveRefreshToken stores the first token of a new login (a new family).
func (s *Store) SaveRefreshToken(ctx context.Context, userID int64, hash []byte, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (token_hash, family_id, user_id, expires_at)
		VALUES ($1, gen_random_uuid(), $2, $3)`, hash, userID, expires)
	return err
}

// RotateRefreshToken swaps a refresh token for a new one in the same family.
// Each token works once: presenting a used token means it leaked, so the whole
// family is revoked and the user must sign in again.
func (s *Store) RotateRefreshToken(ctx context.Context, oldHash, newHash []byte, expires time.Time) (model.User, error) {
	var u model.User
	reused := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var used bool
		err := tx.QueryRow(ctx, `
			SELECT t.used_at IS NOT NULL, u.id, u.username
			FROM refresh_tokens t JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = $1 AND t.expires_at > now()
			FOR UPDATE OF t`, oldHash).Scan(&used, &u.ID, &u.Username)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if used {
			reused = true
			_, err := tx.Exec(ctx, `
				DELETE FROM refresh_tokens
				WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`, oldHash)
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO refresh_tokens (token_hash, family_id, user_id, expires_at)
			SELECT $2, family_id, user_id, $3 FROM refresh_tokens WHERE token_hash = $1`, oldHash, newHash, expires)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET used_at = now() WHERE token_hash = $1`, oldHash)
		return err
	})
	if err == nil && reused {
		return u, ErrForbidden
	}
	return u, err
}

// RevokeRefreshToken signs out the login that the token belongs to.
func (s *Store) RevokeRefreshToken(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM refresh_tokens
		WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`, hash)
	return err
}
