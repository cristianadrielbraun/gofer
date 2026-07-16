package handler

import (
	"context"
	"time"
)

const accountConnectionTestAttempts = 3

const accountConnectionTestRetryDelay = 250 * time.Millisecond

func runAccountConnectionTest(ctx context.Context, retryDelay time.Duration, test func() error) error {
	var lastErr error
	for attempt := 1; attempt <= accountConnectionTestAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = test()
		if lastErr == nil {
			return nil
		}
		if attempt == accountConnectionTestAttempts {
			return lastErr
		}
		if retryDelay <= 0 {
			continue
		}

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}
