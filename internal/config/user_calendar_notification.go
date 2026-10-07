package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/mail"
	"net/url"
	"path"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

type userCalendarNotificationAuthority struct {
	Format                                                             int
	Owner, Account, Source                                             string
	SourceIdentity, CalendarIdentity, CalendarPrincipal, SMTP, Content [32]byte
}

// Authority is private queue metadata; it never appears in the SMTP MIME.
// Canonicalize only the snapshot JSON, preserving all fields other than proof.
func calendarNotificationContent(send storage.OutgoingSend) ([32]byte, map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var payload, note map[string]json.RawMessage
	if json.Unmarshal(send.MessageJSON, &payload) != nil || payload == nil || json.Unmarshal(payload["calendar_notification"], &note) != nil || note == nil {
		return [32]byte{}, nil, nil, storage.ErrCalendarSourceChanged
	}
	delete(note, "UserAuthority")
	encoded, err := json.Marshal(note)
	if err != nil {
		return [32]byte{}, nil, nil, err
	}
	payload["calendar_notification"] = encoded
	send.MessageJSON, err = json.Marshal(payload)
	if err != nil {
		return [32]byte{}, nil, nil, err
	}
	state, err := storage.CalendarReplySendFingerprint(send)
	return state, payload, note, err
}

func notificationWithinSource(source storage.CalendarSource, resource string) bool {
	base, err := url.Parse(source.RemoteID)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return false
	}
	endpoint, err := url.Parse(resource)
	return err == nil && endpoint.Scheme == base.Scheme && endpoint.Host == base.Host && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" && endpoint.Opaque == "" && path.Clean(endpoint.Path) == endpoint.Path && strings.HasPrefix(endpoint.Path, strings.TrimSuffix(base.Path, "/")+"/") && !strings.HasSuffix(endpoint.Path, "/")
}

func notificationSourceWritable(source storage.CalendarSource) bool {
	role := strings.ToLower(strings.TrimSpace(source.AccessRole))
	return source.Provider == storage.CalendarSourceProviderCalDAV && (role == "" || role == "unknown" || role == "owner" || role == "writer")
}

func (s *UserAccountStore) notificationGuard(ctx context.Context, source *UserCalendarSourceSnapshot) func(*sql.Tx, string) error {
	return func(tx *sql.Tx, id string) error {
		if id != source.service.id {
			return storage.ErrAccountRoute
		}
		return s.serviceGuardWithSMTP(ctx, source.service, false, true)(tx)
	}
}

func (s *UserAccountStore) QueueCalendarNotification(ctx context.Context, source *UserCalendarSourceSnapshot, input storage.QueueOutgoingSendInput, event ...*UserCalendarEventSnapshot) (storage.OutgoingSend, error) {
	if source == nil || source.repository != s || source.source == nil || source.service == nil || source.Source().Provider != storage.CalendarSourceProviderCalDAV {
		return storage.OutgoingSend{}, storage.ErrCalendarSourceChanged
	}
	service, collection := source.service, source.Source()
	var events []*storage.UserCalendarEventSnapshot
	if len(event) > 1 {
		return storage.OutgoingSend{}, storage.ErrCalendarEventChanged
	}
	if len(event) == 1 {
		original := event[0]
		if original == nil || original.repository != s || original.event == nil || original.service == nil || original.service.owner != service.owner || original.service.id != service.id || original.service.connection != service.connection || original.service.calendar != service.calendar || original.service.smtp != service.smtp {
			return storage.OutgoingSend{}, ErrAccountServicesChanged
		}
		events = append(events, original.event)
	}
	identity := service.Identity()
	if identity.Provider != "imap" || identity.AuthMethod != "plain" || !service.SMTPConfigured() || input.AccountID != service.id || !strings.EqualFold(input.EnvelopeFrom, identity.EmailAddress) || !notificationSourceWritable(collection) {
		return storage.OutgoingSend{}, ErrAccountServicesChanged
	}
	input.MIMEData = append([]byte(nil), input.MIMEData...)
	input.MessageJSON = append([]byte(nil), input.MessageJSON...)
	input.EnvelopeRecipients = append([]string(nil), input.EnvelopeRecipients...)
	send := storage.OutgoingSend{AccountID: input.AccountID, Transport: input.Transport, EnvelopeFrom: input.EnvelopeFrom, EnvelopeRecipients: input.EnvelopeRecipients, MIMEData: input.MIMEData, MessageJSON: input.MessageJSON}
	content, payload, note, err := calendarNotificationContent(send)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	var details struct {
		UserID, SourceID, ResourceID, Method, Calendar, ExpectedCalendar string
		Deleted                                                          bool
	}
	if json.Unmarshal(payload["calendar_notification"], &details) != nil || details.UserID != service.owner || details.SourceID != collection.ID || !notificationWithinSource(collection, details.ResourceID) || (details.Method != "REQUEST" && details.Method != "CANCEL") || (details.Deleted && details.Method != "CANCEL") || details.Calendar == "" || (!details.Deleted && details.ExpectedCalendar == "") {
		return storage.OutgoingSend{}, storage.ErrCalendarSourceChanged
	}
	proof := userCalendarNotificationAuthority{Format: 1, Owner: service.owner, Account: service.id, Source: collection.ID, SMTP: service.smtp, Content: content}
	proof.SourceIdentity, err = calendarReplySourceIdentity(collection)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	proof.CalendarIdentity, err = calendarReplyConfiguration(service)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	proof.CalendarPrincipal, err = calendarReplyPrincipal(service)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	note["UserAuthority"], err = json.Marshal(proof)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	payload["calendar_notification"], err = json.Marshal(note)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	input.MessageJSON, err = json.Marshal(payload)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	var queued storage.OutgoingSend
	err = s.WithAccountForUser(ctx, service.owner, service.id, func(_ *AccountStore, db *storage.DB) error {
		var err error
		queued, err = db.QueueUserCalendarNotification(ctx, source.source, input, s.notificationGuard(ctx, source), events...)
		return err
	})
	return queued, err
}

type UserCalendarNotificationSnapshot struct {
	repository *UserAccountStore
	send       storage.OutgoingSend
	state      [32]byte
	proof      userCalendarNotificationAuthority
	source     *UserCalendarSourceSnapshot
}

func (s *UserCalendarNotificationSnapshot) Source() *UserCalendarSourceSnapshot { return s.source }
func (s *UserCalendarNotificationSnapshot) Send() storage.OutgoingSend {
	send := s.send
	send.MIMEData = append([]byte(nil), send.MIMEData...)
	send.MessageJSON = append([]byte(nil), send.MessageJSON...)
	send.EnvelopeRecipients = append([]string(nil), send.EnvelopeRecipients...)
	return send
}

// Copy the claimed bytes before checking current Calendar settings. Definite
// preflight failures remain publishable, even when settings have been changed.
func (s *UserAccountStore) snapshotCalendarNotification(ctx context.Context, owner, id string) (*UserCalendarNotificationSnapshot, error) {
	snapshot := &UserCalendarNotificationSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.send, err = db.GetOutgoingSend(ctx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		CalendarNotification *struct {
			UserAuthority userCalendarNotificationAuthority
		} `json:"calendar_notification"`
	}
	if json.Unmarshal(snapshot.send.MessageJSON, &payload) != nil || payload.CalendarNotification == nil {
		return nil, storage.ErrCalendarSourceChanged
	}
	proof := payload.CalendarNotification.UserAuthority
	content, _, _, err := calendarNotificationContent(snapshot.send)
	if err != nil {
		return nil, err
	}
	if proof.Format != 1 || proof.Owner != owner || proof.Account != snapshot.send.AccountID || proof.Source == "" || proof.Content != content {
		return nil, storage.ErrCalendarSourceChanged
	}
	snapshot.proof = proof
	snapshot.state, err = storage.CalendarReplySendFingerprint(snapshot.send)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) SnapshotCalendarNotificationDelivery(ctx context.Context, owner, id string) (*UserCalendarNotificationSnapshot, error) {
	snapshot, err := s.snapshotCalendarNotification(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if err := s.ValidateCalendarNotificationDelivery(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Explicit retries may repair credentials and SMTP settings for the original
// Calendar principal. The original MIME, sender, UID, recipients and resource
// remain unchanged; ambiguous delivery always requires user confirmation.
func (s *UserAccountStore) RetryCalendarNotification(ctx context.Context, owner, id string, confirmAmbiguous bool) (storage.OutgoingSend, error) {
	snapshot, err := s.snapshotCalendarNotification(ctx, owner, id)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	return s.retryCalendarNotification(ctx, snapshot, confirmAmbiguous, nil)
}

// A control snapshot freezes the exact saved content across an HTTP/source/writer
// wait. It supplies no current provider authority and cannot authorize delivery.
func (s *UserAccountStore) SnapshotCalendarNotificationControl(ctx context.Context, owner, id string) (*UserCalendarNotificationSnapshot, error) {
	return s.snapshotCalendarNotification(ctx, owner, id)
}

func (s *UserAccountStore) RetryCalendarNotificationForEvent(ctx context.Context, snapshot *UserCalendarNotificationSnapshot, event *UserCalendarEventSnapshot) (storage.OutgoingSend, error) {
	if snapshot == nil || snapshot.repository != s || event == nil || event.repository != s || event.event == nil || event.service == nil || snapshot.send.Status != storage.OutgoingSendFailed {
		return storage.OutgoingSend{}, storage.ErrCalendarEventChanged
	}
	proof := snapshot.proof
	if event.service.owner != proof.Owner || event.service.id != proof.Account || event.Source().ID != proof.Source {
		return storage.OutgoingSend{}, storage.ErrCalendarEventChanged
	}
	var payload struct {
		CalendarNotification struct{ UserID, SourceID, ResourceID, Method, Calendar string } `json:"calendar_notification"`
	}
	if json.Unmarshal(snapshot.send.MessageJSON, &payload) != nil || payload.CalendarNotification.UserID != proof.Owner || payload.CalendarNotification.SourceID != proof.Source || payload.CalendarNotification.ResourceID != event.Event().RemoteID || !notificationMatchesCalendarEvent(payload.CalendarNotification.Calendar, payload.CalendarNotification.Method, event.Event(), event.service.Identity().EmailAddress) {
		return storage.OutgoingSend{}, storage.ErrCalendarEventChanged
	}
	return s.retryCalendarNotification(ctx, snapshot, false, event)
}

// Verify the frozen notification's UID and organizer independently of the view
// filter. A caller cannot attach a different meeting to a valid control snapshot.
func notificationMatchesCalendarEvent(raw, method string, event storage.CalendarEvent, sender string) (matches bool) {
	defer func() {
		if recover() != nil {
			matches = false
		}
	}()
	if method != "REQUEST" && method != "CANCEL" {
		return false
	}
	decoder := ical.NewDecoder(strings.NewReader(raw))
	cal, err := decoder.Decode()
	if err != nil || len(cal.Events()) != 1 {
		return false
	}
	if _, err := decoder.Decode(); err != io.EOF {
		return false
	}
	if cal.Props.Get("METHOD") == nil || cal.Props.Get("METHOD").Value != method {
		return false
	}
	props := cal.Events()[0].Props
	uid, err := props.Text("UID")
	if err != nil || uid == "" || uid != event.ICalUID || len(props["ORGANIZER"]) != 1 {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(props.Get("ORGANIZER").Value))
	if err != nil || !strings.EqualFold(u.Scheme, "mailto") || u.RawQuery != "" || u.Fragment != "" || u.Host != "" || u.Opaque == "" {
		return false
	}
	address, err := mail.ParseAddress(u.Opaque)
	return err == nil && address.Name == "" && address.Address == u.Opaque && !strings.ContainsAny(address.Address, "\r\n") && strings.EqualFold(address.Address, sender) && strings.EqualFold(address.Address, event.AccountEmail)
}

func (s *UserAccountStore) retryCalendarNotification(ctx context.Context, snapshot *UserCalendarNotificationSnapshot, confirmAmbiguous bool, event *UserCalendarEventSnapshot) (storage.OutgoingSend, error) {
	if snapshot == nil || snapshot.repository != s {
		return storage.OutgoingSend{}, storage.ErrCalendarSourceChanged
	}
	owner := snapshot.proof.Owner
	proof := snapshot.proof
	source, err := s.SnapshotCalendarSource(ctx, owner, proof.Source)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	identity, err := calendarReplySourceIdentity(source.Source())
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	principal, err := calendarReplyPrincipal(source.service)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	if source.Source().AccountID != proof.Account || source.Source().Provider != storage.CalendarSourceProviderCalDAV || !notificationSourceWritable(source.Source()) || source.service.Identity().Provider != "imap" || source.service.Identity().AuthMethod != "plain" || !source.service.SMTPConfigured() || identity != proof.SourceIdentity || principal != proof.CalendarPrincipal {
		return storage.OutgoingSend{}, ErrAccountServicesChanged
	}
	proof.CalendarIdentity, err = calendarReplyConfiguration(source.service)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	proof.SMTP = source.service.smtp
	_, payload, note, err := calendarNotificationContent(snapshot.send)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	note["UserAuthority"], err = json.Marshal(proof)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	payload["calendar_notification"], err = json.Marshal(note)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	messageJSON, err := json.Marshal(payload)
	if err != nil {
		return storage.OutgoingSend{}, err
	}
	var events []*storage.UserCalendarEventSnapshot
	if event != nil {
		if event.service.connection != source.service.connection || event.service.calendar != source.service.calendar || event.service.smtp != source.service.smtp {
			return storage.OutgoingSend{}, ErrAccountServicesChanged
		}
		events = append(events, event.event)
	}
	var actual storage.OutgoingSend
	err = s.WithAccountForUser(ctx, owner, proof.Account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		actual, err = db.RetryUserCalendarNotification(ctx, source.source, snapshot.send, snapshot.state, messageJSON, confirmAmbiguous, s.notificationGuard(ctx, source), events...)
		return err
	})
	return actual, err
}

func (s *UserAccountStore) RestoreCalendarNotification(ctx context.Context, snapshot *UserCalendarNotificationSnapshot) error {
	if snapshot == nil || snapshot.repository != s {
		return storage.ErrCalendarSourceChanged
	}
	proof := snapshot.proof
	source, err := s.SnapshotCalendarSource(ctx, proof.Owner, proof.Source)
	if err != nil {
		return err
	}
	identity, err := calendarReplySourceIdentity(source.Source())
	if err != nil {
		return err
	}
	calendar, err := calendarReplyConfiguration(source.service)
	if err != nil {
		return err
	}
	principal, err := calendarReplyPrincipal(source.service)
	if err != nil {
		return err
	}
	if source.Source().AccountID != proof.Account || source.Source().Provider != storage.CalendarSourceProviderCalDAV || source.service.Identity().Provider != "imap" || source.service.Identity().AuthMethod != "plain" || identity != proof.SourceIdentity || calendar != proof.CalendarIdentity || principal != proof.CalendarPrincipal || source.service.smtp != proof.SMTP {
		return ErrAccountServicesChanged
	}
	snapshot.source = source
	return s.ValidateCalendarNotification(ctx, snapshot)
}

func (s *UserAccountStore) ValidateCalendarNotification(ctx context.Context, snapshot *UserCalendarNotificationSnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.source == nil {
		return storage.ErrCalendarSourceChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarNotificationSource(ctx, snapshot.source.source, snapshot.send, snapshot.state, s.notificationGuard(ctx, snapshot.source))
	})
}

func (s *UserAccountStore) ValidateCalendarNotificationDelivery(ctx context.Context, snapshot *UserCalendarNotificationSnapshot) error {
	if snapshot == nil || snapshot.repository != s {
		return storage.ErrCalendarSourceChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarNotificationAttempt(ctx, p.Owner, snapshot.send, snapshot.state, s.calendarControlGuard(ctx, p.Owner))
	})
}

func (s *UserAccountStore) FinishCalendarNotificationSend(ctx context.Context, snapshot *UserCalendarNotificationSnapshot, result storage.CalendarReplySendResult) error {
	if snapshot == nil || snapshot.repository != s {
		return storage.ErrCalendarSourceChanged
	}
	p := snapshot.proof
	return s.WithAccountForUser(ctx, p.Owner, p.Account, func(_ *AccountStore, db *storage.DB) error {
		return db.FinishUserCalendarNotificationSend(ctx, p.Owner, snapshot.send, snapshot.state, result, s.calendarControlGuard(ctx, p.Owner))
	})
}
