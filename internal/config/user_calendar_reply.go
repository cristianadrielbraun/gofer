package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// This persisted record contains fingerprints and an exact reservation nonce;
// it cannot supply credentials or turn a legacy queued reply into a new grant.
type userCalendarReplyAuthority struct {
	Format                                                    int
	Owner, Account, Source, Event                             string
	SourceState, EventState, Connection, Calendar, SMTP       [32]byte
	PayloadState, SendState, CalendarIdentity, SourceIdentity [32]byte
	CalendarPrincipal                                         [32]byte
	Claim                                                     storage.CalendarResponseClaimIdentity
}

func calendarReplyConfiguration(service *AccountServiceSnapshot) ([32]byte, error) {
	base, username, useAccount := service.CalDAVSettings()
	password := service.calendarPassword
	if useAccount {
		username = strings.TrimSpace(service.Identity().Username)
		if username == "" {
			username = service.Identity().EmailAddress
		}
		password = service.password
	}
	identity := service.Identity()
	return serviceStateHash([]any{"user-calendar-reply-calendar-v1", identity.Provider, identity.ProviderAccountID,
		identity.EmailAddress, base, username, useAccount, password})
}

// An explicit user retry can repair credentials, but cannot silently change
// the original Calendar account, endpoint, sender or effective DAV username.
func calendarReplyPrincipal(service *AccountServiceSnapshot) ([32]byte, error) {
	base, username, useAccount := service.CalDAVSettings()
	identity := service.Identity()
	if useAccount {
		username = strings.TrimSpace(identity.Username)
		if username == "" {
			username = identity.EmailAddress
		}
	}
	return serviceStateHash([]any{"user-calendar-reply-principal-v1", identity.Provider,
		identity.ProviderAccountID, identity.EmailAddress, base, username, useAccount})
}

// Display metadata may change after email acceptance. Persist the selected
// collection and access identity independently of its cached name/color.
func calendarReplySourceIdentity(source storage.CalendarSource) ([32]byte, error) {
	return serviceStateHash([]any{"user-calendar-reply-source-v1", source.ID, source.UserID,
		source.AccountID, source.Provider, source.RemoteID, source.AccessRole, source.IsSelected, source.IsDeleted})
}

func calendarReplyAuthority(claim *UserCalendarResponseClaim) userCalendarReplyAuthority {
	snapshot, service := claim.event, claim.event.service
	sourceState, eventState := snapshot.event.Fingerprints()
	return userCalendarReplyAuthority{Format: 1, Owner: service.owner, Account: service.id, Source: snapshot.Source().ID, Event: snapshot.Event().ID, SourceState: sourceState, EventState: eventState, Connection: service.connection, Calendar: service.calendar, SMTP: service.smtp, Claim: claim.claim.Identity()}
}

func (s *UserAccountStore) QueueCalendarReply(ctx context.Context, claim *UserCalendarResponseClaim, job storage.CalendarReplyJob, input storage.QueueOutgoingSendInput) (string, error) {
	if claim == nil || claim.repository != s || claim.event == nil || claim.claim == nil || claim.event.Source().Provider != storage.CalendarSourceProviderCalDAV {
		return "", storage.ErrCalendarEventChanged
	}
	snapshot := claim.event
	if err := s.checkServiceSnapshot(snapshot.service); err != nil {
		return "", err
	}
	if !snapshot.service.SMTPConfigured() || !strings.EqualFold(input.EnvelopeFrom, snapshot.service.Identity().EmailAddress) {
		return "", ErrAccountServicesChanged
	}
	// Freeze caller-owned slices before routing/cache/writer waits.
	input.EnvelopeRecipients = append([]string(nil), input.EnvelopeRecipients...)
	input.MIMEData = append([]byte(nil), input.MIMEData...)
	input.MessageJSON = append([]byte(nil), input.MessageJSON...)
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(job.Payload), &payload) != nil || payload == nil {
		return "", storage.ErrCalendarEventChanged
	}
	delete(payload, "UserAuthority")
	var details struct {
		Event                       storage.CalendarEvent
		Target                      struct{ RemoteID, ETag string }
		Scope, SelfEmail, Organizer string
	}
	if json.Unmarshal([]byte(job.Payload), &details) != nil || details.Scope != claim.claim.Identity().Scope || details.Target.RemoteID != job.RemoteID || details.Target.ETag != job.Version || !strings.EqualFold(details.SelfEmail, input.EnvelopeFrom) || len(input.EnvelopeRecipients) != 1 || details.Organizer != input.EnvelopeRecipients[0] {
		return "", storage.ErrCalendarEventChanged
	}
	original, err := serviceStateHash(snapshot.Event())
	if err != nil {
		return "", err
	}
	submitted, err := serviceStateHash(details.Event)
	if err != nil {
		return "", err
	}
	if original != submitted {
		return "", storage.ErrCalendarEventChanged
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	proof := calendarReplyAuthority(claim)
	proof.SourceIdentity, err = calendarReplySourceIdentity(snapshot.Source())
	if err != nil {
		return "", err
	}
	proof.CalendarIdentity, err = calendarReplyConfiguration(snapshot.service)
	if err != nil {
		return "", err
	}
	proof.CalendarPrincipal, err = calendarReplyPrincipal(snapshot.service)
	if err != nil {
		return "", err
	}
	proof.PayloadState, err = serviceStateHash(string(canonical))
	if err != nil {
		return "", err
	}
	proof.SendState, err = storage.CalendarReplySendFingerprint(storage.OutgoingSend{AccountID: input.AccountID, Transport: input.Transport, EnvelopeFrom: input.EnvelopeFrom, EnvelopeRecipients: input.EnvelopeRecipients, MIMEData: input.MIMEData, MessageJSON: input.MessageJSON})
	if err != nil {
		return "", err
	}
	authority, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	payload["UserAuthority"] = authority
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	job.Payload = string(encoded)
	guard := func(tx *sql.Tx, id string) error {
		if id != snapshot.service.id {
			return storage.ErrAccountRoute
		}
		return s.serviceGuardWithSMTP(ctx, snapshot.service, false, true)(tx)
	}
	var id string
	err = s.WithAccountForUser(ctx, snapshot.service.owner, snapshot.service.id, func(_ *AccountStore, db *storage.DB) error {
		var err error
		id, err = db.QueueUserCalendarReply(ctx, claim.claim, job, input, guard)
		return err
	})
	return id, err
}

type UserCalendarReplySnapshot struct {
	repository *UserAccountStore
	job        storage.CalendarReplyJob
	send       storage.OutgoingSend
	proof      userCalendarReplyAuthority
	event      *UserCalendarEventSnapshot
	source     *UserCalendarSourceSnapshot
}

func (s *UserCalendarReplySnapshot) Job() storage.CalendarReplyJob       { return s.job }
func (s *UserCalendarReplySnapshot) Event() *UserCalendarEventSnapshot   { return s.event }
func (s *UserCalendarReplySnapshot) Source() *UserCalendarSourceSnapshot { return s.source }

// Resume only a proof read from this owner's saved job. Event-state validation
// is needed before email; after accepted delivery only source/config authority
// is retained, so cache refreshes cannot cause an email to be sent again.
func (s *UserAccountStore) snapshotCalendarReply(ctx context.Context, owner, id string) (*UserCalendarReplySnapshot, error) {
	var job storage.CalendarReplyJob
	var send storage.OutgoingSend
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		job, err = db.GetCalendarReply(ctx, owner, id)
		if err != nil {
			return err
		}
		send, err = db.GetOutgoingSend(ctx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	var payload struct{ UserAuthority userCalendarReplyAuthority }
	if json.Unmarshal([]byte(job.Payload), &payload) != nil {
		return nil, storage.ErrCalendarEventChanged
	}
	proof := payload.UserAuthority
	if job.State != "pending" {
		return nil, storage.ErrCalendarEventChanged
	}
	var originalPayload map[string]json.RawMessage
	if json.Unmarshal([]byte(job.Payload), &originalPayload) != nil {
		return nil, storage.ErrCalendarEventChanged
	}
	delete(originalPayload, "UserAuthority")
	canonical, err := json.Marshal(originalPayload)
	if err != nil {
		return nil, err
	}
	state, err := serviceStateHash(string(canonical))
	if err != nil {
		return nil, err
	}
	if state != proof.PayloadState {
		return nil, storage.ErrCalendarEventChanged
	}
	if proof.Format != 1 || proof.Owner != owner || proof.Source != job.SourceID || proof.Account == "" || proof.Event == "" || proof.Claim.ID == "" || proof.Claim.RemoteID != job.RemoteID || proof.Claim.Version != job.Version || proof.Claim.Response != job.Response {
		return nil, storage.ErrCalendarEventChanged
	}
	return &UserCalendarReplySnapshot{repository: s, job: job, send: send, proof: proof}, nil
}

// Capture the immutable job and the exact claimed attempt before protocol work.
// This permits recording a definite preflight rejection even if its event or
// settings have changed. It does not authorize any SMTP or Calendar request.
func (s *UserAccountStore) SnapshotCalendarReplyDelivery(ctx context.Context, owner, id string) (*UserCalendarReplySnapshot, error) {
	snapshot, err := s.snapshotCalendarReply(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if snapshot.send.Status != storage.OutgoingSendSending {
		return nil, storage.ErrCalendarEventChanged
	}
	if err := s.ValidateCalendarReplyDelivery(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) RestoreCalendarReply(ctx context.Context, owner, id string, beforeEmail bool) (*UserCalendarReplySnapshot, error) {
	snapshot, err := s.snapshotCalendarReply(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	job, proof := snapshot.job, snapshot.proof
	if (beforeEmail && job.SendStatus != storage.OutgoingSendPending && job.SendStatus != storage.OutgoingSendSending && job.SendStatus != storage.OutgoingSendFailed) || (!beforeEmail && job.SendStatus != storage.OutgoingSendSent) {
		return nil, storage.ErrCalendarEventChanged
	}
	source, err := s.SnapshotCalendarSource(ctx, owner, proof.Source)
	if err != nil {
		return nil, err
	}
	calendarIdentity, err := calendarReplyConfiguration(source.service)
	if err != nil {
		return nil, err
	}
	sourceIdentity, err := calendarReplySourceIdentity(source.Source())
	if err != nil {
		return nil, err
	}
	if source.Source().AccountID != proof.Account || source.Source().Provider != storage.CalendarSourceProviderCalDAV || sourceIdentity != proof.SourceIdentity || calendarIdentity != proof.CalendarIdentity ||
		(beforeEmail && (source.source.Fingerprint() != proof.SourceState || source.service.connection != proof.Connection || source.service.calendar != proof.Calendar || source.service.smtp != proof.SMTP)) {
		return nil, ErrAccountServicesChanged
	}
	var event *UserCalendarEventSnapshot
	if beforeEmail {
		event, err = s.SnapshotCalendarEvent(ctx, owner, proof.Event)
		if err != nil {
			return nil, err
		}
		sourceState, eventState := event.event.Fingerprints()
		if sourceState != proof.SourceState || eventState != proof.EventState || event.service.connection != proof.Connection || event.service.calendar != proof.Calendar || event.service.smtp != proof.SMTP {
			return nil, storage.ErrCalendarEventChanged
		}
	}
	snapshot.event, snapshot.source = event, source
	if err := s.ValidateCalendarReply(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Original job/source/configuration and nonce are checked after reacquisition.
func (s *UserAccountStore) ValidateCalendarReply(ctx context.Context, snapshot *UserCalendarReplySnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.source == nil {
		return storage.ErrCalendarEventChanged
	}
	proof, source := snapshot.proof, snapshot.source
	var event *storage.UserCalendarEventSnapshot
	if snapshot.event != nil {
		event = snapshot.event.event
	}
	return s.WithAccountForUser(ctx, proof.Owner, proof.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarReplyProof(ctx, snapshot.job, proof.Claim, source.source, event, snapshot.send, proof.SendState, func(tx *sql.Tx, id string) error {
			if id != proof.Account {
				return storage.ErrAccountRoute
			}
			return s.serviceGuardWithSMTP(ctx, source.service, false, snapshot.event != nil)(tx)
		})
	})
}

func (s *UserAccountStore) ValidateCalendarReplyDelivery(ctx context.Context, snapshot *UserCalendarReplySnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.send.Status != storage.OutgoingSendSending {
		return storage.ErrCalendarEventChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarReplyDelivery(ctx, snapshot.job, p.Claim, snapshot.send, p.SendState, s.calendarControlGuard(ctx, p.Owner))
	})
}

// Record the result of exactly this attempt. Configuration and event changes
// after remote acceptance must not erase that acceptance or make mail eligible
// for another automatic send. Lifecycle, immutable job/MIME and nonce still gate
// the commit after the writer wait and after the mutation.
func (s *UserAccountStore) FinishCalendarReplySend(ctx context.Context, snapshot *UserCalendarReplySnapshot, result storage.CalendarReplySendResult) error {
	if snapshot == nil || snapshot.repository != s || snapshot.send.Status != storage.OutgoingSendSending {
		return storage.ErrCalendarEventChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.FinishUserCalendarReplySend(ctx, snapshot.job, p.Claim, snapshot.send, p.SendState, result, s.calendarControlGuard(ctx, p.Owner))
	})
}

// Return a detached copy: delivery callers cannot mutate the private attempt.
func (s *UserCalendarReplySnapshot) Send() storage.OutgoingSend {
	send := s.send
	send.MIMEData = append([]byte(nil), send.MIMEData...)
	send.MessageJSON = append([]byte(nil), send.MessageJSON...)
	send.EnvelopeRecipients = append([]string(nil), send.EnvelopeRecipients...)
	return send
}
