package mail

import (
	"context"
	"errors"
)

// UserMailQueue runs inside the existing account gate, activity guard and
// session bound. Implementations must lease only for local work, never network
// calls, and must not recursively Sync this account.
type UserMailQueue interface {
	Run(context.Context, string, string) error
}

func (s *UserIMAP) SetMailQueue(queue UserMailQueue) error {
	if queue == nil {
		return errors.New("mail queue is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mailQueue != nil || s.background || len(s.gates) != 0 || len(s.queued) != 0 || s.closing || s.ctx.Err() != nil {
		return errors.New("mail queue must be configured once before IMAP work starts")
	}
	s.mailQueue = queue
	return nil
}
