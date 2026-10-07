package mail

import (
	"context"
	"time"
)

const ConnectionTestAttempts = 3

func RetryConnectionTest(ctx context.Context, retryDelay time.Duration, test func() error) error {
	var lastErr error
	for attempt := 1; attempt <= ConnectionTestAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = test()
		if lastErr == nil {
			return nil
		}
		if attempt == ConnectionTestAttempts {
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
