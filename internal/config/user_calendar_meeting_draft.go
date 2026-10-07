package config

import (
	"context"
	"encoding/json"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A copied meeting draft has an original-principal reservation and exact row
// authority. Its public payload is data, never authority for a write or cleanup.
type UserCalendarMeetingDraftSnapshot struct {
	repository *UserAccountStore
	source     *UserCalendarSourceSnapshot
	draft      *storage.UserCalendarMeetingDraftSnapshot
}

func (d *UserCalendarMeetingDraftSnapshot) Source() *UserCalendarSourceSnapshot { return d.source }
func (d *UserCalendarMeetingDraftSnapshot) Google() storage.CalendarMeetDraft {
	return d.draft.Google()
}
func (d *UserCalendarMeetingDraftSnapshot) Teams() storage.CalendarTeamsDraft { return d.draft.Teams() }

// Account directory discovery precedes this local read. Listing neither opens
// other owners' stores nor hands the consumer a draft mutation capability.
func (s *UserAccountStore) ListCalendarMeetingCleanup(ctx context.Context, owner, account string, limit int) ([]storage.CalendarMeetingCleanupCandidate, error) {
	if limit < 1 || limit > 16 {
		return nil, storage.ErrCalendarCreateConflict
	}
	var candidates []storage.CalendarMeetingCleanupCandidate
	err := s.WithAccountForUser(ctx, owner, account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		candidates, err = db.ListAccountCalendarMeetingCleanup(ctx, owner, account, limit, s.calendarControlGuard(ctx, owner))
		return err
	})
	return candidates, err
}

func (s *UserAccountStore) BeginCalendarMeetingDraft(ctx context.Context, source *UserCalendarSourceSnapshot, id string, extra ...func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	return s.calendarMeetingDraft(ctx, source, id, true, extra)
}
func (s *UserAccountStore) SnapshotCalendarMeetingDraft(ctx context.Context, source *UserCalendarSourceSnapshot, id string, extra ...func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	return s.calendarMeetingDraft(ctx, source, id, false, extra)
}
func (s *UserAccountStore) calendarMeetingDraft(ctx context.Context, source *UserCalendarSourceSnapshot, id string, create bool, extra []func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	guard, err := s.calendarCreateGuard(ctx, source, extra)
	if err != nil {
		return nil, err
	}
	binding, err := calendarCreateBinding(source)
	if err != nil {
		return nil, err
	}
	d := &UserCalendarMeetingDraftSnapshot{repository: s, source: source}
	err = s.WithAccountForUser(ctx, source.service.OwnerID(), source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		if create {
			d.draft, err = db.BeginUserCalendarMeetingDraft(ctx, source.source, id, binding, guard)
		} else {
			d.draft, err = db.SnapshotUserCalendarMeetingDraft(ctx, source.source, id, binding, guard)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}
func (s *UserAccountStore) ValidateCalendarMeetingDraft(ctx context.Context, d *UserCalendarMeetingDraftSnapshot, extra ...func() error) error {
	if d == nil || d.repository != s || d.source == nil || d.draft == nil {
		return storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, d.source, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, d.source.service.OwnerID(), d.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarMeetingDraft(ctx, d.draft, guard)
	})
}
func (s *UserAccountStore) PublishCalendarMeetingDraft(ctx context.Context, d *UserCalendarMeetingDraftSnapshot, transition storage.CalendarMeetingDraftTransition, value string, extra ...func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	if d == nil || d.repository != s || d.source == nil || d.draft == nil {
		return nil, storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, d.source, extra)
	if err != nil {
		return nil, err
	}
	if transition == storage.CalendarMeetingDraftBind {
		return nil, storage.ErrCalendarCreateConflict
	}
	if transition == storage.CalendarMeetingDraftConference {
		link := calendar.MeetingJoinURL(value)
		if !json.Valid([]byte(value)) || (d.source.Source().Provider == "gmail" && calendar.GoogleMeetJoinURL(link) == "") || (d.source.Source().Provider == "outlook" && calendar.TeamsJoinURL(link) == "") {
			return nil, storage.ErrCalendarCreateConflict
		}
		if d.source.Source().Provider == "gmail" {
			var data map[string]json.RawMessage
			if json.Unmarshal([]byte(value), &data) != nil || data["createRequest"] != nil {
				return nil, storage.ErrCalendarCreateConflict
			}
		}
	}
	next := &UserCalendarMeetingDraftSnapshot{repository: s, source: d.source}
	err = s.WithAccountForUser(ctx, d.source.service.OwnerID(), d.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.draft, err = db.PublishUserCalendarMeetingDraft(ctx, d.draft, transition, value, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

// The repository derives the final target from a private create/event claim.
// A string from a form or copied draft cannot reserve a conference for a target.
func (s *UserAccountStore) BindCalendarMeetingDraft(ctx context.Context, d *UserCalendarMeetingDraftSnapshot, create *UserCalendarCreateClaim, event *UserCalendarEventSnapshot, extra ...func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	if d == nil || d.repository != s || d.source == nil || d.draft == nil || (create == nil) == (event == nil) {
		return nil, storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, d.source, extra)
	if err != nil {
		return nil, err
	}
	if create != nil && (create.repository != s || create.source == nil || create.source.service == nil || create.claim == nil || create.source.service.connection != d.source.service.connection || create.source.service.calendar != d.source.service.calendar) {
		return nil, storage.ErrCalendarCreateConflict
	}
	if event != nil && (d.source.Source().Provider != "gmail" || event.repository != s || event.service == nil || event.event == nil || event.service.connection != d.source.service.connection || event.service.calendar != d.source.service.calendar) {
		return nil, storage.ErrCalendarEventChanged
	}
	var c *storage.UserCalendarCreateClaim
	var e *storage.UserCalendarEventSnapshot
	if create != nil {
		c = create.claim
	}
	if event != nil {
		e = event.event
	}
	next := &UserCalendarMeetingDraftSnapshot{repository: s, source: d.source}
	err = s.WithAccountForUser(ctx, d.source.service.OwnerID(), d.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.draft, err = db.BindUserCalendarMeetingDraft(ctx, d.draft, c, e, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

func (s *UserAccountStore) FinishCalendarTeamsCreate(ctx context.Context, d *UserCalendarMeetingDraftSnapshot, create *UserCalendarCreateClaim, extra ...func() error) (*UserCalendarMeetingDraftSnapshot, error) {
	if d == nil || d.repository != s || d.source == nil || d.draft == nil || create == nil || create.repository != s || create.source == nil || create.source.service == nil || create.claim == nil || create.source.service.connection != d.source.service.connection || create.source.service.calendar != d.source.service.calendar {
		return nil, storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, d.source, extra)
	if err != nil {
		return nil, err
	}
	next := &UserCalendarMeetingDraftSnapshot{repository: s, source: d.source}
	err = s.WithAccountForUser(ctx, d.source.service.OwnerID(), d.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.draft, err = db.FinishUserCalendarTeamsCreate(ctx, d.draft, create.claim, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}
