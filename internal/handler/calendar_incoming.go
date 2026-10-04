package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
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
		state, note, reason := "complete", "", "applied"
		if err != nil {
			note = err.Error()
			state = "retry"
			reason = "retry"
			if errors.Is(err, errCalendarIncomingIgnored) || errors.Is(err, message.ErrCalendarReplyAuthentication) {
				state = "ignored"
				reason = "not_applied"
			}
			if errors.Is(err, message.ErrCalendarReplyAuthentication) {
				reason = "unverified"
			}
			if errors.Is(err, message.ErrCalendarReplyAuthenticationTemporary) {
				reason = "verification_pending"
			}
			if errors.Is(err, errCalendarIncomingServerManaged) {
				reason = "server_managed"
			}
		}
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
		file, openErr := os.Open(info.RawPath)
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
	event, err := h.db.FindCalendarIncomingEvent(ctx, candidate.UserID, candidate.AccountID, reply.UID)
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
	existing, err := h.db.FindCalendarIncomingEvent(ctx, candidate.UserID, candidate.AccountID, reply.UID)
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
		if s.ID == source.ID && s.AccountID == candidate.AccountID && s.Provider == storage.CalendarSourceProviderCalDAV && calendarSourceWritable(s) {
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
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		return err
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, existing)
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
	props := current.Events()[0].Props
	organizer := props.Get("ORGANIZER")
	if before.ICalUID != reply.UID || organizer == nil {
		return errCalendarIncomingIgnored
	}
	if organizer.Params.Get("SCHEDULE-AGENT") != "CLIENT" {
		return errCalendarIncomingServerManaged
	}
	sequence, err := calendarMeetingSequence(current, false)
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
	allowed, err := h.db.ReserveCalendarIncomingResponse(ctx, existing, reply.Attendee, reply.Sequence, reply.Stamp, reply.Status)
	if err != nil {
		return err
	}
	if !allowed {
		return errCalendarIncomingIgnored
	}
	confirmed, responseHeaders := current, headers
	if props["ATTENDEE"][index].Params.Get("PARTSTAT") != reply.Status {
		// Merge only PARTSTAT. Preserve private properties, other guests, alarms,
		// the organizer's SEQUENCE/DTSTAMP, and the selected scheduling agent.
		desired := &ical.Calendar{Component: cloneCalendarComponent(current.Component)}
		desired.Events()[0].Props["ATTENDEE"][index].Params.Set("PARTSTAT", reply.Status)
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
		res, err := client.Do(req)
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
	confirmedProps := confirmed.Events()[0].Props
	confirmedSequence, err := calendarMeetingSequence(confirmed, false)
	if err != nil || updated.ICalUID != reply.UID || confirmedSequence != strconv.Itoa(reply.Sequence) || confirmedProps.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT") != "CLIENT" {
		return fmt.Errorf("calendar reply identity or revision was not confirmed")
	}
	matched := 0
	for _, person := range confirmedProps["ATTENDEE"] {
		if calendarReplyAddress(person.Value) == reply.Attendee && person.Params.Get("PARTSTAT") == reply.Status {
			matched++
		}
	}
	if matched != 1 {
		return fmt.Errorf("calendar server did not confirm the guest response")
	}
	if err := h.db.CompleteCalendarIncomingResponse(ctx, existing, calendarStorageEvent(existing.UserID, existing.SourceID, updated)); err != nil {
		return err
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: existing.UserID, Payload: map[string]any{"source_id": source.ID, "event_id": existing.ID}})
	}
	return nil
}
