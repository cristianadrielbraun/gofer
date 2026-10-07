package config

import (
	"context"
	"encoding/json"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Local status/recovery is independent of current provider configuration. This
// private record cannot authorize SMTP or Calendar requests.
type UserCalendarReplyControlSnapshot struct {
	repository *UserAccountStore
	control    *storage.UserCalendarReplyControlSnapshot
	original   storage.CalendarResponseClaimIdentity
}

func (s *UserCalendarReplyControlSnapshot) Job() storage.CalendarReplyJob { return s.control.Job() }
func (s *UserCalendarReplyControlSnapshot) AccountID() string             { return s.control.AccountID() }

func calendarReplyControlNonce(job storage.CalendarReplyJob, account string) storage.CalendarResponseClaimIdentity {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(job.Payload), &payload) != nil {
		return storage.CalendarResponseClaimIdentity{}
	}
	var proof userCalendarReplyAuthority
	if json.Unmarshal(payload["UserAuthority"], &proof) != nil {
		return storage.CalendarResponseClaimIdentity{}
	}
	delete(payload, "UserAuthority")
	canonical, err := json.Marshal(payload)
	if err != nil {
		return storage.CalendarResponseClaimIdentity{}
	}
	state, err := serviceStateHash(string(canonical))
	if err != nil || state != proof.PayloadState || proof.Format != 1 || proof.Owner != job.UserID || proof.Account != account || proof.Source != job.SourceID || proof.Event == "" || proof.Claim.ID == "" || proof.Claim.RemoteID != job.RemoteID || proof.Claim.Version != job.Version || proof.Claim.Response != job.Response {
		return storage.CalendarResponseClaimIdentity{}
	}
	return proof.Claim
}

func (s *UserAccountStore) SnapshotCalendarReplyControl(ctx context.Context, owner, id string) (*UserCalendarReplyControlSnapshot, error) {
	var control *storage.UserCalendarReplyControlSnapshot
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		control, err = db.SnapshotUserCalendarReplyControl(ctx, owner, id, s.calendarControlGuard(ctx, owner))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &UserCalendarReplyControlSnapshot{repository: s, control: control, original: calendarReplyControlNonce(control.Job(), control.AccountID())}, nil
}

func (s *UserAccountStore) ApplyCalendarReplyControl(ctx context.Context, snapshot *UserCalendarReplyControlSnapshot, action string) error {
	if snapshot == nil || snapshot.repository != s || snapshot.control == nil {
		return storage.ErrCalendarEventChanged
	}
	job := snapshot.Job()
	return s.WithAccountForUser(ctx, job.UserID, snapshot.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ApplyUserCalendarReplyControl(ctx, snapshot.control, action, snapshot.original, s.calendarControlGuard(ctx, job.UserID))
	})
}
