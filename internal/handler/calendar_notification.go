package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/google/uuid"
)

func calendarNotificationICS(source *ical.Calendar, method string, guests []calendar.GuestDraft) (string, error) {
	if method != "REQUEST" && method != "CANCEL" || len(source.Events()) != 1 {
		return "", fmt.Errorf("invalid scheduling message")
	}
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Gofer//Calendar//EN")
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("METHOD", method)
	for _, child := range source.Children {
		if child.Name == ical.CompTimezone {
			cal.Children = append(cal.Children, cloneCalendarComponent(child))
		}
	}
	event := cloneCalendarComponent(source.Events()[0].Component)
	allowed := map[string]bool{}
	for _, name := range []string{"UID", "DTSTAMP", "SEQUENCE", "DTSTART", "DTEND", "DURATION", "SUMMARY", "DESCRIPTION", "X-ALT-DESC", "LOCATION", "STATUS", "ORGANIZER", "ATTENDEE", "TRANSP", "CLASS", "URL"} {
		allowed[name] = true
	}
	for name := range event.Props {
		if !allowed[name] {
			delete(event.Props, name)
		}
	}
	event.Children = nil // Personal alarms and private metadata stay on DAV.
	for name, props := range event.Props {
		for i := range props {
			for param := range props[i].Params {
				if strings.HasPrefix(param, "SCHEDULE-") {
					delete(props[i].Params, param)
				}
			}
		}
		event.Props[name] = props
	}
	event.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	if method == "CANCEL" {
		event.Props.SetText("STATUS", "CANCELLED")
		wanted := map[string]bool{}
		for _, guest := range guests {
			wanted[guest.Email] = true
		}
		var people []ical.Prop
		for _, person := range event.Props["ATTENDEE"] {
			if wanted[calendarReplyAddress(person.Value)] {
				people = append(people, person)
			}
		}
		event.Props["ATTENDEE"] = people
	}
	cal.Children = append(cal.Children, event)
	return encodeCalendarReply(cal)
}

// Queue before writing DAV. Source serialization plus a fresh pre-SMTP read
// makes this durable across a lost provider response or a process restart.
func (h *Handler) queueCalendarNotification(ctx context.Context, source storage.CalendarSource, endpoint, key, method string, cal, expected *ical.Calendar, guests []calendar.GuestDraft, deleted bool) error {
	if p, ok := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); ok {
		if p == nil || p.h != h {
			return storage.ErrCalendarSourceChanged
		}
		return p.queueNotification(ctx, source, endpoint, key, method, cal, expected, guests, deleted)
	}
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil {
		return err
	}
	guests = filterCalendarOrganizerGuests(guests, account.Email)
	if len(guests) == 0 {
		return nil
	}
	var recipients []string
	for _, guest := range guests {
		if calendarReplyAddress("mailto:"+guest.Email) == "" {
			return fmt.Errorf("invalid guest")
		}
		recipients = append(recipients, guest.Email)
	}
	sort.Strings(recipients)
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(source.ID+"\x00"+endpoint+"\x00"+key+"\x00"+method+"\x00"+strings.Join(recipients, ","))).String()
	if previous, err := h.db.GetOutgoingSend(ctx, id); err == nil {
		if previous.AccountID != source.AccountID {
			return fmt.Errorf("notification identity changed")
		}
		if previous.Status == storage.OutgoingSendFailed {
			_, err = h.db.RetryOutgoingSend(ctx, source.UserID, id, false)
			return err
		}
		if previous.Status == storage.OutgoingSendCanceled {
			return fmt.Errorf("this meeting notification was canceled; reopen the event before saving")
		}
		return nil // Never replay sent/ambiguous deliveries.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	body, err := calendarNotificationICS(cal, method, guests)
	if err != nil {
		return err
	}
	var desired string
	if expected != nil {
		desired, err = encodeCalendarReply(expected)
		if err != nil {
			return err
		}
	}
	summary, _ := cal.Events()[0].Props.Text("SUMMARY")
	label := "Invitation"
	if method == "CANCEL" {
		label = "Canceled"
	}
	msg := &message.OutgoingMessage{FromName: account.Name, FromEmail: account.Email, Subject: label + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(summary), TextBody: label + ": " + summary,
		MessageID: message.NewMessageID(), Date: time.Now().UTC(), CalendarNotification: &message.CalendarNotification{UserID: source.UserID, SourceID: source.ID, ResourceID: endpoint, Method: method, Calendar: body, ExpectedCalendar: desired, Deleted: deleted}}
	for _, guest := range guests {
		msg.To = append(msg.To, &mail.Address{Name: guest.Name, Address: guest.Email})
	}
	raw, err := buildOutgoingMIME(storage.OutgoingTransportSMTP, msg)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(snapshotOutgoingMessage(msg))
	if err != nil {
		return err
	}
	_, err = h.db.QueueOutgoingSend(ctx, storage.QueueOutgoingSendInput{ID: id, AccountID: source.AccountID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: account.Email, EnvelopeRecipients: recipients, MIMEData: raw, MessageJSON: snapshot})
	return err
}

func (h *Handler) beforeCalendarNotificationSend(ctx context.Context, send storage.OutgoingSend, msg *message.OutgoingMessage) (func(), error) {
	noop := func() {}
	note := msg.CalendarNotification
	if note == nil || (note.Method != "REQUEST" && note.Method != "CANCEL") || (note.Deleted && note.Method != "CANCEL") {
		return noop, fmt.Errorf("invalid meeting notification")
	}
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: note.SourceID, UserID: note.UserID})
	if err != nil {
		return noop, markOutgoingSendRetryable(fmt.Errorf("calendar is busy"))
	}
	p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest)
	var source storage.CalendarSource
	if owned {
		if p == nil || p.h != h || p.notification == nil || p.source == nil {
			return unlock, storage.ErrCalendarSourceChanged
		}
		if err := p.validate(ctx); err != nil {
			return unlock, err
		}
		source = p.source.Source()
		if source.UserID != note.UserID || source.ID != note.SourceID {
			return unlock, storage.ErrCalendarSourceChanged
		}
	} else {
		sources, err := h.db.ListSelectedCalendarSources(ctx, note.UserID)
		if err != nil {
			return unlock, markOutgoingSendRetryable(err)
		}
		for _, candidate := range sources {
			if candidate.ID == note.SourceID {
				source = candidate
				break
			}
		}
	}
	if source.ID == "" || source.Provider != storage.CalendarSourceProviderCalDAV || source.AccountID != send.AccountID || !calendarSourceWritable(source) {
		return unlock, fmt.Errorf("meeting notification access changed")
	}
	var account models.Account
	if owned {
		identity := p.service().Identity()
		account = models.Account{ID: source.AccountID, Email: identity.EmailAddress, Name: identity.DisplayName}
	} else {
		account, err = h.calendarReplyAccount(ctx, source)
	}
	if err != nil || !strings.EqualFold(send.EnvelopeFrom, account.Email) || !strings.EqualFold(msg.FromEmail, account.Email) {
		return unlock, fmt.Errorf("meeting sending identity changed")
	}
	endpoint, err := calDAVResponseEndpoint(source, note.ResourceID)
	if err != nil {
		return unlock, err
	}
	icalMessage, err := calendarUpdateDecodeICS([]byte(note.Calendar))
	if err != nil || len(icalMessage.Events()) != 1 || icalMessage.Props.Get("METHOD") == nil || icalMessage.Props.Get("METHOD").Value != note.Method || icalMessage.Events()[0].Props.Get("ORGANIZER") == nil || calendarReplyAddress(icalMessage.Events()[0].Props.Get("ORGANIZER").Value) != strings.ToLower(account.Email) {
		return unlock, fmt.Errorf("invalid meeting organizer notification")
	}
	allowed := map[string]bool{}
	for _, person := range icalMessage.Events()[0].Props["ATTENDEE"] {
		allowed[calendarReplyAddress(person.Value)] = true
	}
	if len(send.EnvelopeRecipients) == 0 {
		return unlock, fmt.Errorf("no meeting recipients")
	}
	for _, recipient := range send.EnvelopeRecipients {
		if !allowed[strings.ToLower(recipient)] {
			return unlock, fmt.Errorf("meeting recipients changed")
		}
	}
	var credentials calendarCredentials
	if owned {
		credentials, credentials.err = p.actionCredentials()
	} else {
		credentials = h.calendarCredentialsForSource(ctx, note.UserID, source)
	}
	if credentials.err != nil {
		return unlock, credentials.err
	}
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		return unlock, err
	}
	current, _, err := calendarUpdateCalDAVGet(ctx, calendarDeleteClient(calDAVHTTPTransport), endpoint, credentials.username, credentials.password)
	if note.Deleted {
		if calendarNotificationMissing(err) {
			return unlock, nil
		}
		return unlock, markOutgoingSendRetryable(fmt.Errorf("meeting cancellation is waiting for confirmed deletion"))
	}
	if err != nil {
		return unlock, markOutgoingSendRetryable(fmt.Errorf("saved meeting could not be verified: %w", err))
	}
	desired, err := calendarUpdateDecodeICS([]byte(note.ExpectedCalendar))
	if err != nil {
		return unlock, fmt.Errorf("invalid expected meeting")
	}
	matches := calendarMeetingResourceMatches(desired, current)
	if note.Method == "CANCEL" {
		matches = calendarNotificationRemovedGuestsMatch(desired, current, send.EnvelopeRecipients)
	}
	if !matches {
		return unlock, markOutgoingSendRetryable(fmt.Errorf("notification is waiting for the matching saved meeting; refresh the calendar"))
	}
	if current.Events()[0].Props.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT") != "CLIENT" {
		return unlock, fmt.Errorf("the calendar server now handles guest notifications")
	}
	return unlock, nil
}

// A later organizer update must not suppress a pending removal cancellation.
// Conversely, re-adding that guest must prevent a stale cancellation email.
func calendarNotificationRemovedGuestsMatch(expected, actual *ical.Calendar, recipients []string) bool {
	if len(expected.Events()) != 1 || len(actual.Events()) != 1 {
		return false
	}
	left, right := expected.Events()[0].Props, actual.Events()[0].Props
	if left.Get("UID") == nil || right.Get("UID") == nil || left.Get("UID").Value != right.Get("UID").Value || left.Get("ORGANIZER") == nil || right.Get("ORGANIZER") == nil {
		return false
	}
	organizer := calendarReplyAddress(left.Get("ORGANIZER").Value)
	if err := calendarMeetingCalDAVShape(actual, http.Header{}, organizer); err != nil {
		return false
	}
	a, err := calendarMeetingSequence(expected, false)
	if err != nil {
		return false
	}
	b, err := calendarMeetingSequence(actual, false)
	if err != nil {
		return false
	}
	oldSequence, _ := strconv.Atoi(a)
	newSequence, _ := strconv.Atoi(b)
	if newSequence < oldSequence {
		return false
	}
	for _, person := range right["ATTENDEE"] {
		for _, recipient := range recipients {
			if calendarReplyAddress(person.Value) == strings.ToLower(recipient) {
				return false
			}
		}
	}
	return true
}
