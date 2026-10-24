package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	ical "github.com/emersion/go-ical"
)

var errCalendarIncomingIgnored = errors.New("calendar reply cannot be applied")
var errCalendarIncomingServerManaged = fmt.Errorf("%w: replies are managed by the calendar server", errCalendarIncomingIgnored)

func (h *Handler) runCalendarIncomingTick(ctx context.Context) {
	if h.accountStore == nil {
		return
	}
	messages, err := h.db.ListCalendarIncomingMessages(ctx, 12)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("calendar incoming replies: %v", err)
		}
		return
	}
	for _, candidate := range messages {
		if ctx.Err() != nil {
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := h.processCalendarIncomingMessage(jobCtx, candidate)
		cancel()
		state, note, reason := calendarIncomingOutcome(err)
		finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := h.db.FinishCalendarIncomingDelivery(finishCtx, candidate, state, note, reason); err != nil {
			log.Printf("calendar incoming reply %d: could not record outcome: %v", candidate.ID, err)
		}
		finish()
	}
}

func (h *Handler) processCalendarIncomingMessage(ctx context.Context, candidate storage.CalendarIncomingMessage) error {
	info, err := h.db.GetMessageStorageInfoForUser(ctx, candidate.ID, candidate.UserID)
	if err != nil {
		return err
	}
	if info == nil || info.AccountID != candidate.AccountID {
		return errCalendarIncomingIgnored
	}
	var raw []byte
	if info.RawPath != "" {
		file, openErr := store.OpenMessageFile(info.RawPath)
		if openErr == nil {
			raw, err = io.ReadAll(io.LimitReader(file, message.CalendarIncomingMaxSize+1))
			file.Close()
			if err != nil {
				return err
			}
		}
	}
	if raw == nil {
		fetchInfo, err := h.db.GetMessageFetchInfoInternal(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if fetchInfo == nil || fetchInfo.AccountID != candidate.AccountID {
			return errCalendarIncomingIgnored
		}
		raw, err = h.fetchBodyRemote(ctx, candidate.ID, fetchInfo)
		if err != nil {
			return err
		}
	}
	reply, err := message.ExtractCalendarIncomingReply(raw)
	if err != nil || reply == nil || reply.From != candidate.From {
		return fmt.Errorf("%w: no supported reply from the invited guest", errCalendarIncomingIgnored)
	}
	// Associate only syntactically valid replies for this organizer and a
	// currently invited guest. This is diagnostic metadata, not authentication.
	event, err := h.db.FindCalendarIncomingEvent(ctx, candidate.UserID, candidate.AccountID, reply.UID, reply.RecurrenceID, reply.Attendee)
	if errors.Is(err, sql.ErrNoRows) {
		return errCalendarIncomingIgnored
	}
	if err != nil {
		return err
	}
	invited := false
	for _, person := range calendarEventParticipants(event.AttendeesJSON) {
		if strings.EqualFold(person.Email, reply.Attendee) {
			invited = true
			break
		}
	}
	if !invited || !strings.EqualFold(event.AccountEmail, reply.Organizer) || !strings.EqualFold(event.OrganizerEmail, reply.Organizer) {
		return errCalendarIncomingIgnored
	}
	if err := h.db.BeginCalendarIncomingDelivery(ctx, candidate, event, reply.Attendee); err != nil {
		return err
	}
	if err := message.VerifyCalendarIncomingSender(ctx, raw, reply.From, h.calendarIncomingLookupTXT); err != nil {
		return err
	}
	return h.applyCalendarIncomingReply(ctx, candidate, *reply)
}

func (h *Handler) applyCalendarIncomingReply(ctx context.Context, candidate storage.CalendarIncomingMessage, reply message.CalendarIncomingReply) error {
	existing, err := h.db.FindCalendarIncomingEvent(ctx, candidate.UserID, candidate.AccountID, reply.UID, reply.RecurrenceID, reply.Attendee)
	if errors.Is(err, sql.ErrNoRows) {
		return errCalendarIncomingIgnored
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(existing.AccountEmail, reply.Organizer) || reply.Attendee != candidate.From {
		return errCalendarIncomingIgnored
	}
	source := storage.CalendarSource{ID: existing.SourceID, UserID: existing.UserID}
	unlock, err := h.lockCalendarCreate(ctx, source)
	if err != nil {
		return err
	}
	defer unlock()
	// Recheck source and cache after waiting for sync/editor/SMTP work.
	sources, err := h.db.ListSelectedCalendarSources(ctx, candidate.UserID)
	if err != nil {
		return err
	}
	found := false
	for _, s := range sources {
		if s.ID == source.ID && s.AccountID == candidate.AccountID && storage.CalendarSourceMailServer(s.Provider) && calendarSourceWritable(s) {
			source, found = s, true
			break
		}
	}
	if !found {
		return errCalendarIncomingIgnored
	}
	existing, err = h.db.GetCalendarEvent(ctx, candidate.UserID, existing.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errCalendarIncomingIgnored
		}
		return err
	}
	credentials := h.calendarCredentialsForSource(ctx, candidate.UserID, source)
	if credentials.err != nil {
		return credentials.err
	}
	ctx = withJMAPCalendarDAV(ctx, source, credentials)
	err = applyCalendarIncomingCalDAV(ctx, source, existing, reply, credentials,
		func(ctx context.Context) (bool, error) {
			// A series' date is reserved apart from the whole series.
			reserved := existing
			reserved.ICalUID = storage.CalendarIncomingResponseKey(existing.ICalUID, reply.RecurrenceID)
			return h.db.ReserveCalendarIncomingResponse(ctx, reserved, reply.Attendee, reply.Sequence, reply.Stamp, reply.Status)
		}, func(ctx context.Context, updated storage.CalendarEvent) error {
			if err := h.db.CompleteCalendarIncomingResponse(ctx, existing, updated); err != nil {
				return err
			}
			if h.syncer != nil {
				h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: existing.UserID, Payload: map[string]any{"source_id": source.ID, "event_id": existing.ID}})
			}
			return nil
		})
	// A reply to a series changes many cached dates: refresh them.
	if err == nil && existing.SeriesRemoteID != "" {
		h.refreshCalendarEditedSeries(ctx, source, existing)
	}
	return err
}

// Both layouts share authentication-independent CalDAV revision checks and the
// minimal PARTSTAT merge. The caller supplies guarded reservation/publication.
// A reply for a series changes its own event, and one for a single date that
// date's event, which is created from the series when it has none yet.
func applyCalendarIncomingCalDAV(ctx context.Context, source storage.CalendarSource, existing storage.CalendarEvent, reply message.CalendarIncomingReply, credentials calendarCredentials, reserve func(context.Context) (bool, error), publish func(context.Context, storage.CalendarEvent) error) error {
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		return err
	}
	resource := existing
	if existing.SeriesRemoteID != "" {
		resource = storage.CalendarEvent{RemoteID: existing.SeriesRemoteID}
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, resource)
	if err != nil {
		return err
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
	if err != nil {
		if calendarNotificationMissing(err) {
			return errCalendarIncomingIgnored
		}
		return err
	}
	before, err := calendarMeetingNormalized(current, headers, endpoint, reply.Organizer)
	if err != nil {
		if errors.Is(err, errCalendarUpdateUnsupported) {
			return errCalendarIncomingIgnored
		}
		return err
	}
	master := calendarMeetingMaster(current)
	organizer := master.Props.Get("ORGANIZER")
	if before.ICalUID != reply.UID || organizer == nil || (reply.RecurrenceID != "" && master.Props.Get("RRULE") == nil) {
		return errCalendarIncomingIgnored
	}
	if organizer.Params.Get("SCHEDULE-AGENT") != "CLIENT" {
		return errCalendarIncomingServerManaged
	}
	target := calendarMeetingComponent(current, reply.RecurrenceID)
	created := false
	if target == nil {
		// A date with no event of its own: check with the server that it is a
		// date of the series, then add its event, as the series has it.
		if existing.RemoteID != existing.SeriesRemoteID+"#recurrence="+url.QueryEscape(reply.RecurrenceID) || existing.ETag != headers.Get("ETag") || calendarMeetingExcluded(master, reply.RecurrenceID) {
			return errCalendarIncomingIgnored
		}
		_, _, rid, err := calendarCalDAVOccurrenceResource(current, headers, existing, true)
		if err != nil {
			return errCalendarIncomingIgnored
		}
		if err := calendarCalDAVVerifyOccurrence(ctx, source, credentials.username, credentials.password, existing, true); err != nil {
			return errCalendarIncomingIgnored
		}
		if target, err = calendarMeetingDateEvent(master, rid); err != nil {
			return errCalendarIncomingIgnored
		}
		created = true
	}
	props := target.Props
	sequence, err := calendarMeetingComponentSequence(target, false)
	if err != nil {
		return errCalendarIncomingIgnored
	}
	if sequence != strconv.Itoa(reply.Sequence) {
		return errCalendarIncomingIgnored
	}
	if stamp := props.Get("DTSTAMP"); stamp != nil {
		revision, err := time.Parse("20060102T150405Z", stamp.Value)
		if err != nil || reply.Stamp.Before(revision) {
			return errCalendarIncomingIgnored
		}
	}
	index := -1
	for i, person := range props["ATTENDEE"] {
		if calendarReplyAddress(person.Value) == reply.Attendee {
			if index != -1 {
				return errCalendarIncomingIgnored
			}
			index = i
		}
	}
	if index == -1 {
		return errCalendarIncomingIgnored
	}
	allowed, err := reserve(ctx)
	if err != nil {
		return err
	}
	if !allowed {
		return errCalendarIncomingIgnored
	}
	confirmed, responseHeaders := current, headers
	if created || props["ATTENDEE"][index].Params.Get("PARTSTAT") != reply.Status {
		// Merge only PARTSTAT. Preserve private properties, other guests, alarms,
		// the organizer's SEQUENCE/DTSTAMP, and the selected scheduling agent.
		desired := &ical.Calendar{Component: cloneCalendarComponent(current.Component)}
		if created {
			desired.Children = append(desired.Children, cloneCalendarComponent(target))
		}
		person := &calendarMeetingComponent(desired, reply.RecurrenceID).Props["ATTENDEE"][index]
		person.Params.Set("PARTSTAT", reply.Status)
		body, err := encodeCalendarReply(desired)
		if err != nil {
			return err
		}
		req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, credentials.username, credentials.password, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		req.Header.Set("If-Match", headers.Get("ETag"))
		if tag := headers.Get("Schedule-Tag"); tag != "" {
			if !calendarUpdateValidETag(tag, false) {
				return errCalendarIncomingIgnored
			}
			req.Header.Set("If-Schedule-Tag-Match", tag)
		}
		res, err := calendarProviderDo(client, req)
		if err != nil {
			return err
		}
		status := res.StatusCode
		res.Body.Close()
		if status != http.StatusOK && status != http.StatusNoContent {
			return fmt.Errorf("calendar reply merge failed (HTTP %d)", status)
		}
		confirmed, responseHeaders, err = calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
		if err != nil {
			return err
		}
		if !calendarMeetingResourceMatches(desired, confirmed) {
			return fmt.Errorf("calendar reply merge was not confirmed")
		}
	}
	updated, err := calendarMeetingNormalized(confirmed, responseHeaders, endpoint, reply.Organizer)
	if err != nil {
		return err
	}
	saved := calendarMeetingComponent(confirmed, reply.RecurrenceID)
	confirmedMaster := calendarMeetingMaster(confirmed)
	if saved == nil || confirmedMaster == nil {
		return fmt.Errorf("calendar reply identity or revision was not confirmed")
	}
	confirmedSequence, err := calendarMeetingComponentSequence(saved, false)
	if err != nil || updated.ICalUID != reply.UID || confirmedSequence != strconv.Itoa(reply.Sequence) || confirmedMaster.Props.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT") != "CLIENT" {
		return fmt.Errorf("calendar reply identity or revision was not confirmed")
	}
	matched := 0
	for _, person := range saved.Props["ATTENDEE"] {
		if calendarReplyAddress(person.Value) == reply.Attendee && person.Params.Get("PARTSTAT") == reply.Status {
			matched++
		}
	}
	if matched != 1 {
		return fmt.Errorf("calendar server did not confirm the guest response")
	}
	if existing.SeriesRemoteID == "" {
		return publish(ctx, calendarStorageEvent(existing.UserID, existing.SourceID, updated))
	}
	// A series is cached as its dates: record the reply on the associated
	// date with the guests it changed; the caller then refreshes the series'
	// dates from the server.
	plain := cloneCalendarComponent(saved)
	delete(plain.Props, "RRULE")
	delete(plain.Props, "EXDATE")
	guests, err := normalizeCalDAVEvent(ical.Event{Component: plain}, endpoint, responseHeaders.Get("ETag"), time.UTC)
	if err != nil {
		return err
	}
	row := existing
	row.ETag, row.AttendeesJSON, row.ResponseStatus = responseHeaders.Get("ETag"), string(guests.Attendees), "organizer"
	return publish(ctx, row)
}
