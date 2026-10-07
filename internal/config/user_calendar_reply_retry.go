package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Prepare the original saved reply for a deliberate user retry. The private
// current source/event/profile can authorize a native read, while the original
// message, invitation, selected collection and reservation remain fixed.
func (s *UserAccountStore) PrepareCalendarReplyRetry(ctx context.Context, control *UserCalendarReplyControlSnapshot, confirmed bool) (*UserCalendarReplySnapshot, error) {
	if control == nil || control.repository != s || control.control == nil {
		return nil, storage.ErrCalendarEventChanged
	}
	job := control.Job()
	if job.State != "pending" || (confirmed && job.SendStatus != storage.OutgoingSendAmbiguous) || (!confirmed && job.SendStatus != storage.OutgoingSendFailed) {
		return nil, storage.ErrOutgoingSendNotRetryable
	}
	snapshot, err := s.snapshotCalendarReply(ctx, job.UserID, job.ID)
	if err != nil {
		return nil, err
	}
	if snapshot.job != job || snapshot.proof.Account != control.AccountID() || snapshot.send.AccountID != control.AccountID() {
		return nil, storage.ErrCalendarEventChanged
	}
	proof := snapshot.proof
	source, err := s.SnapshotCalendarSource(ctx, job.UserID, proof.Source)
	if err != nil {
		return nil, err
	}
	identity, err := calendarReplySourceIdentity(source.Source())
	if err != nil {
		return nil, err
	}
	principal, err := calendarReplyPrincipal(source.service)
	if err != nil {
		return nil, err
	}
	calendar, err := calendarReplyConfiguration(source.service)
	if err != nil {
		return nil, err
	}
	if identity != proof.SourceIdentity || source.Source().AccountID != proof.Account || source.Source().Provider != storage.CalendarSourceProviderCalDAV ||
		(proof.CalendarPrincipal != [32]byte{} && principal != proof.CalendarPrincipal) ||
		(proof.CalendarPrincipal == [32]byte{} && calendar != proof.CalendarIdentity) {
		return nil, ErrAccountServicesChanged
	}
	event, err := s.SnapshotCalendarEvent(ctx, job.UserID, proof.Event)
	if err != nil {
		return nil, err
	}
	currentSource, _ := event.event.Fingerprints()
	if currentSource != source.source.Fingerprint() || event.service.connection != source.service.connection || event.service.calendar != source.service.calendar || event.service.smtp != source.service.smtp {
		return nil, ErrAccountServicesChanged
	}
	var payload struct {
		Event                       storage.CalendarEvent
		Scope, SelfEmail, Organizer string
	}
	if json.Unmarshal([]byte(job.Payload), &payload) != nil || payload.Scope != proof.Claim.Scope || payload.Event.ID != proof.Event ||
		event.Event().ICalUID != payload.Event.ICalUID || event.Event().RemoteID != payload.Event.RemoteID || event.Event().SeriesRemoteID != payload.Event.SeriesRemoteID ||
		!strings.EqualFold(payload.SelfEmail, source.service.Identity().EmailAddress) || !strings.EqualFold(snapshot.send.EnvelopeFrom, payload.SelfEmail) ||
		len(snapshot.send.EnvelopeRecipients) != 1 || snapshot.send.EnvelopeRecipients[0] != payload.Organizer || !source.service.SMTPConfigured() {
		return nil, storage.ErrCalendarEventChanged
	}
	snapshot.source, snapshot.event = source, event
	if err := s.ValidateCalendarReply(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// The handler calls this only after the native provider read confirms the
// original resource, version, UID, organizer, attendee and response scope.
// Storage checks the original attempt and nonce again after its writer wait.
func (s *UserAccountStore) RequeueCalendarReply(ctx context.Context, control *UserCalendarReplyControlSnapshot, snapshot *UserCalendarReplySnapshot, confirmed bool) error {
	if control == nil || control.repository != s || control.control == nil || snapshot == nil || snapshot.repository != s || snapshot.event == nil || snapshot.source == nil || snapshot.job != control.Job() || snapshot.proof.Account != control.AccountID() {
		return storage.ErrCalendarEventChanged
	}
	if err := s.checkServiceSnapshot(snapshot.event.service); err != nil {
		return err
	}
	if err := s.ValidateCalendarReply(ctx, snapshot); err != nil {
		return err
	}
	proof := snapshot.proof
	proof.SourceState, proof.EventState = snapshot.event.event.Fingerprints()
	service := snapshot.event.service
	proof.Connection, proof.Calendar, proof.SMTP = service.connection, service.calendar, service.smtp
	var err error
	proof.CalendarIdentity, err = calendarReplyConfiguration(service)
	if err != nil {
		return err
	}
	proof.CalendarPrincipal, err = calendarReplyPrincipal(service)
	if err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(snapshot.job.Payload), &payload); err != nil {
		return err
	}
	payload["UserAuthority"], err = json.Marshal(proof)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	guard := func(tx *sql.Tx, id string) error {
		if id != proof.Account {
			return storage.ErrAccountRoute
		}
		return s.serviceGuardWithSMTP(ctx, service, false, true)(tx)
	}
	return s.WithAccountForUser(ctx, proof.Owner, proof.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.RequeueUserCalendarReply(ctx, control.control, snapshot.event.event, proof.Claim, string(encoded), confirmed, guard)
	})
}
