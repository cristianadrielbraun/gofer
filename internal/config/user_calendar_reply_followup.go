package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The private saved authority is restored before claiming this generation.
// SMTP delivery stays recorded; this claim can authorize only Calendar work.
type UserCalendarReplyFollowupClaim struct {
	repository *UserAccountStore
	snapshot   *UserCalendarReplySnapshot
	claim      *storage.UserCalendarReplyFollowupClaim
}

func (c *UserCalendarReplyFollowupClaim) Snapshot() *UserCalendarReplySnapshot { return c.snapshot }

func (s *UserAccountStore) replyFollowupGuard(ctx context.Context, snapshot *UserCalendarReplySnapshot) (func(*sql.Tx, string) error, error) {
	if snapshot == nil || snapshot.repository != s || snapshot.source == nil || snapshot.event != nil || snapshot.send.Status != storage.OutgoingSendSent || snapshot.job.State != "pending" {
		return nil, storage.ErrCalendarEventChanged
	}
	source := snapshot.source
	if err := s.checkServiceSnapshot(source.service); err != nil {
		return nil, err
	}
	return func(tx *sql.Tx, id string) error {
		if id != snapshot.proof.Account {
			return storage.ErrAccountRoute
		}
		return s.serviceGuard(ctx, source.service, false)(tx)
	}, nil
}
func (s *UserAccountStore) StartCalendarReplyFollowup(ctx context.Context, snapshot *UserCalendarReplySnapshot) (*UserCalendarReplyFollowupClaim, error) {
	guard, err := s.replyFollowupGuard(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	c := &UserCalendarReplyFollowupClaim{repository: s, snapshot: snapshot}
	p := snapshot.proof
	err = s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		c.claim, err = db.StartUserCalendarReplyFollowup(ctx, snapshot.job, p.Claim, snapshot.source.source, snapshot.send, p.SendState, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
func (s *UserAccountStore) ValidateCalendarReplyFollowup(ctx context.Context, claim *UserCalendarReplyFollowupClaim) error {
	if claim == nil || claim.repository != s || claim.claim == nil {
		return storage.ErrCalendarEventChanged
	}
	guard, err := s.replyFollowupGuard(ctx, claim.snapshot)
	if err != nil {
		return err
	}
	p := claim.snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarReplyFollowup(ctx, claim.claim, guard)
	})
}
func (s *UserAccountStore) FinishCalendarReplyFollowup(ctx context.Context, claim *UserCalendarReplyFollowupClaim, state string) error {
	if claim == nil || claim.repository != s || claim.claim == nil {
		return storage.ErrCalendarEventChanged
	}
	guard, err := s.replyFollowupGuard(ctx, claim.snapshot)
	if err != nil {
		return err
	}
	p := claim.snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.FinishUserCalendarReplyFollowup(ctx, claim.claim, state, guard)
	})
}

// This record authorizes only a local notice for already accepted email. It can
// be captured after Calendar reconfiguration, selection changes or nonce
// replacement, without adopting the new Calendar or releasing its reservation.
func (s *UserAccountStore) SnapshotCalendarReplyFollowupRecord(ctx context.Context, owner, id string) (*UserCalendarReplySnapshot, error) {
	snapshot, err := s.snapshotCalendarReply(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if snapshot.send.Status != storage.OutgoingSendSent || snapshot.job.SendStatus != storage.OutgoingSendSent || snapshot.send.AccountID != snapshot.proof.Account {
		return nil, storage.ErrCalendarEventChanged
	}
	err = s.WithAccountForUser(ctx, owner, snapshot.proof.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarReplySentRecord(ctx, snapshot.job, snapshot.send, s.calendarControlGuard(ctx, owner))
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
func (s *UserAccountStore) MarkCalendarReplyConflict(ctx context.Context, snapshot *UserCalendarReplySnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.send.Status != storage.OutgoingSendSent || snapshot.send.AccountID != snapshot.proof.Account {
		return storage.ErrCalendarEventChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.MarkUserCalendarReplyConflict(ctx, snapshot.job, snapshot.send, s.calendarControlGuard(ctx, p.Owner))
	})
}
