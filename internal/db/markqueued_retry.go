package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *Store) MarkQueuedWithRetry(ctx context.Context, id uuid.UUID, attempts int, backoff time.Duration) error {
	if attempts < 1 {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 && backoff > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		err = s.MarkQueued(ctx, tx, id)
		if err != nil {
			_ = tx.Rollback(ctx)
			lastErr = err
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("mark queued: no attempts")
	}
	return fmt.Errorf("mark queued after %d attempts: %w", attempts, lastErr)
}
