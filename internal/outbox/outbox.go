// Package outbox forwards committed events from Postgres to Redis.
//
// Chat instances write an outbox row in the same transaction as the change it
// describes. This worker publishes those rows afterwards, so an event can be
// delayed by a crash but never lost. It may publish an event twice (a crash
// after publishing, before marking the row); receivers deduplicate.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const batchSize = 100

type PublishFunc func(ctx context.Context, event []byte) error

// Run publishes pending events until ctx ends. Several publishers may run at
// once: each claims rows with SKIP LOCKED, so they never take the same row.
func Run(ctx context.Context, pool *pgxpool.Pool, publish PublishFunc) error {
	slog.Info("publisher started")
	for ctx.Err() == nil {
		if err := loop(ctx, pool, publish); err != nil && ctx.Err() == nil {
			slog.Error("publisher failed, retrying", "err", err)
			time.Sleep(time.Second)
		}
	}
	return nil
}

func loop(ctx context.Context, pool *pgxpool.Pool, publish PublishFunc) error {
	// A dedicated connection listens for the NOTIFY sent when rows are added.
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN outbox`); err != nil {
		return err
	}

	var cleaned time.Time
	for {
		n, err := publishBatch(ctx, pool, publish)
		if err != nil {
			return err
		}
		if n == batchSize {
			continue // more rows are probably waiting
		}
		if time.Since(cleaned) > time.Minute {
			if err := cleanup(ctx, pool); err != nil {
				return err
			}
			cleaned = time.Now()
		}
		// Sleep until notified. The timeout is a fallback poll in case a
		// notification was missed.
		wait, cancel := context.WithTimeout(ctx, time.Second)
		_, err = listener.Conn().WaitForNotification(wait)
		timedOut := wait.Err() != nil
		cancel()
		if err != nil && !timedOut {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

type pendingEvent struct {
	ID    int64
	Event string
}

// publishBatch publishes up to batchSize pending events and marks them
// published. Rows stay locked until the transaction ends, so a crash midway
// leaves them pending for the next attempt.
func publishBatch(ctx context.Context, pool *pgxpool.Pool, publish PublishFunc) (int, error) {
	published := 0
	var publishErr error
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `
			SELECT id, event FROM outbox
			WHERE published_at IS NULL
			ORDER BY id LIMIT $1
			FOR UPDATE SKIP LOCKED`, batchSize)
		pending, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pendingEvent])
		if err != nil {
			return err
		}
		var done []int64
		for _, p := range pending {
			if publishErr = publish(ctx, []byte(p.Event)); publishErr != nil {
				break
			}
			done = append(done, p.ID)
		}
		// Commit what was published even if a later event failed.
		published = len(done)
		_, err = tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, done)
		return err
	})
	if err == nil {
		err = publishErr
	}
	return published, err
}

// cleanup deletes rows that are no longer needed.
func cleanup(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DELETE FROM outbox WHERE published_at < now() - interval '1 hour'`)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now()`)
	return err
}
