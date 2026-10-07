package handler

import (
	"context"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
)

const accountConnectionTestAttempts = mail.ConnectionTestAttempts
const accountConnectionTestRetryDelay = 250 * time.Millisecond

func runAccountConnectionTest(ctx context.Context, retryDelay time.Duration, test func() error) error {
	return mail.RetryConnectionTest(ctx, retryDelay, test)
}
