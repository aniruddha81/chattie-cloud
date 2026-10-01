package postgres

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/aniruddha81/chattie-cloud/internal/model"
)

var errDuplicate = errors.New("duplicate client message id")

// SendMessage stores a message and its outbox event in one transaction.
//
// Updating the room row locks it, so sequences in a room are handed out one
// at a time, and the same statement checks that the sender is a member. A
// retry with the same client message ID stores nothing new and returns the
// original message.
func (s *Store) SendMessage(ctx context.Context, senderID, roomID int64, clientMessageID, content string) (model.Message, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var sequence int64
		err := tx.QueryRow(ctx, `
			UPDATE rooms SET last_sequence = last_sequence + 1
			WHERE id = $1 AND deleted_at IS NULL
			  AND EXISTS (SELECT 1 FROM memberships WHERE room_id = $1 AND user_id = $2)
			RETURNING last_sequence`, roomID, senderID).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO messages (room_id, sequence, sender_id, client_message_id, content)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (sender_id, client_message_id) DO NOTHING
			RETURNING id`, roomID, sequence, senderID, clientMessageID, content).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDuplicate // rolls back, so the sequence is not used up
		}
		if err != nil {
			return err
		}
		return addEvent(ctx, tx, model.Event{Type: model.EventMessage, RoomID: roomID, MessageID: id, Sequence: sequence})
	})
	if errors.Is(err, errDuplicate) {
		return s.oneMessage(ctx, `WHERE m.sender_id = $1 AND m.client_message_id = $2`, senderID, clientMessageID)
	}
	if err != nil {
		return model.Message{}, err
	}
	return s.GetMessage(ctx, id)
}

func (s *Store) GetMessage(ctx context.Context, id int64) (model.Message, error) {
	return s.oneMessage(ctx, `WHERE m.id = $1`, id)
}

// MessagesAfter returns messages newer than a sequence, oldest first. Clients
// use it to catch up on anything they missed.
func (s *Store) MessagesAfter(ctx context.Context, roomID, after int64, limit int) ([]model.Message, error) {
	return s.messages(ctx, `WHERE m.room_id = $1 AND m.sequence > $2 ORDER BY m.sequence LIMIT $3`, roomID, after, limit)
}

// LatestMessages returns the newest messages of a room, oldest first.
func (s *Store) LatestMessages(ctx context.Context, roomID int64, limit int) ([]model.Message, error) {
	msgs, err := s.messages(ctx, `WHERE m.room_id = $1 ORDER BY m.sequence DESC LIMIT $2`, roomID, limit)
	slices.Reverse(msgs)
	return msgs, err
}

func (s *Store) messages(ctx context.Context, where string, args ...any) ([]model.Message, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT m.id, m.room_id, m.sequence, m.sender_id, u.username,
		       m.client_message_id::text, m.content, m.created_at
		FROM messages m JOIN users u ON u.id = m.sender_id `+where, args...)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[model.Message])
}

func (s *Store) oneMessage(ctx context.Context, where string, args ...any) (model.Message, error) {
	msgs, err := s.messages(ctx, where, args...)
	if err != nil {
		return model.Message{}, err
	}
	if len(msgs) == 0 {
		return model.Message{}, ErrNotFound
	}
	return msgs[0], nil
}
