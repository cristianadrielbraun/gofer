package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// Select exactly one scheduling agent, never both DAV scheduling and SMTP.
func (h *Handler) calendarMeetingAgent(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, previous ...string) (string, string, string, error) {
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil || calendarReplyAddress("mailto:"+account.Email) == "" {
		return "", "", "", fmt.Errorf("a valid organizer account is required")
	}
	organizer := strings.ToLower(account.Email)
	oldAgent := ""
	if len(previous) > 0 {
		oldAgent = previous[0]
	}
	if oldAgent == "NONE" {
		return "", "", "", calendarUpdateUnsupported("Guest delivery is disabled for this meeting.")
	}
	auto, addresses, err := calDAVResponseCapabilities(ctx, source, credentials.username, credentials.password)
	if err != nil {
		return "", "", "", err
	}
	if auto && oldAgent != "CLIENT" {
		for _, address := range addresses {
			if strings.EqualFold(address, organizer) {
				return "SERVER", organizer, account.Name, nil
			}
		}
		return "", "", "", calendarUpdateUnsupported("The calendar server's scheduling identity does not match this account's email address.")
	}
	if !auto && oldAgent == "SERVER" {
		return "", "", "", calendarUpdateUnsupported("Automatic scheduling is no longer available; refresh this account before changing the meeting.")
	}
	if h.accountStore == nil {
		return "", "", "", fmt.Errorf("configure SMTP before inviting guests")
	}
	cfg, err := h.accountStore.GetConfig(ctx, source.AccountID)
	if err != nil || cfg.SMTPHost == "" || cfg.SMTPPort <= 0 {
		return "", "", "", calendarReplyConfigurationError("This calendar server needs invitations by email. Configure SMTP for this account first.")
	}
	return "CLIENT", organizer, account.Name, nil
}

func calendarMeetingCalDAVShape(cal *ical.Calendar, headers http.Header, organizer string) error {
	if len(cal.Events()) != 1 || cal.Props.Get("METHOD") != nil {
		return calendarUpdateUnsupported("Only single, non-recurring meetings are supported.")
	}
	props := cal.Events()[0].Props
	if len(props["ORGANIZER"]) != 1 || calendarReplyAddress(props.Get("ORGANIZER").Value) != strings.ToLower(organizer) {
		return calendarUpdateUnsupported("Only the organizer can change this meeting.")
	}
	for _, name := range []string{"ORGANIZER", "ATTENDEE"} {
		for _, person := range props[name] {
			if calendarReplyAddress(person.Value) == "" {
				return calendarUpdateUnsupported("This meeting has an unsupported participant.")
			}
			for _, param := range []string{"SENT-BY", "DELEGATED-TO", "DELEGATED-FROM", "MEMBER"} {
				if person.Params.Get(param) != "" {
					return calendarUpdateUnsupported("Delegated meetings are not supported yet.")
				}
			}
			if kind := person.Params.Get("CUTYPE"); kind != "" && kind != "INDIVIDUAL" {
				return calendarUpdateUnsupported("Room and group invitations are not supported yet.")
			}
		}
	}
	copy := &ical.Calendar{Component: cloneCalendarComponent(cal.Component)}
	delete(copy.Events()[0].Props, "ORGANIZER")
	delete(copy.Events()[0].Props, "ATTENDEE")
	h := headers.Clone()
	h.Del("Schedule-Tag")
	if reason := calendarUpdateCalDAVRestriction(copy, h); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	return nil
}

func calendarMeetingSequence(cal *ical.Calendar, increment bool) (string, error) {
	sequence := 0
	if prop := cal.Events()[0].Props.Get("SEQUENCE"); prop != nil {
		var err error
		sequence, err = strconv.Atoi(prop.Value)
		if err != nil || sequence < 0 || sequence >= 2147483647 {
			return "", calendarUpdateUnsupported("The meeting has an unsupported sequence.")
		}
	}
	if increment {
		sequence++
	}
	return strconv.Itoa(sequence), nil
}

func calendarMeetingNormalized(cal *ical.Calendar, headers http.Header, endpoint, organizer string) (calendar.RemoteEvent, error) {
	if err := calendarMeetingCalDAVShape(cal, headers, organizer); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if !calendarUpdateValidETag(headers.Get("ETag"), false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Refresh the meeting to retrieve a safe event version.")
	}
	event, err := normalizeCalDAVEvent(cal.Events()[0], endpoint, headers.Get("ETag"), time.UTC)
	if err == nil {
		_, err = calendarMeetingGuests(event.Attendees)
		event.ResponseStatus = "organizer"
	}
	return event, err
}

func (h *Handler) createCalDAVMeeting(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	agent, email, name, err := h.calendarMeetingAgent(ctx, source, credentials)
	if err != nil {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	draft.ScheduleAgent, draft.OrganizerEmail, draft.OrganizerName = agent, email, name
	body, err := calendarCreateICS(draft)
	if err != nil {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	endpoint := strings.TrimRight(source.RemoteID, "/") + "/" + draft.RequestID + ".ics"
	if _, err := calDAVResponseEndpoint(source, endpoint); err != nil {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	if agent == "CLIENT" {
		cal, err := calendarUpdateDecodeICS([]byte(body))
		if err != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
		}
		if err := h.queueCalendarNotification(ctx, source, endpoint, draft.RequestID, "REQUEST", cal, cal, draft.Guests, false); err != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
		}
	}
	event, err := createCalDAVCalendarEvent(ctx, source, credentials.username, credentials.password, draft)
	h.signalOutgoingWorker()
	return event, err
}

func (h *Handler) readCalDAVMeeting(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, existing storage.CalendarEvent) (*ical.Calendar, http.Header, calendar.RemoteEvent, error) {
	if !calendarUpdateValidETag(existing.ETag, false) {
		return nil, nil, calendar.RemoteEvent{}, calendarUpdateUnsupported("Refresh the meeting to retrieve a safe event version.")
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, existing)
	if err != nil {
		return nil, nil, calendar.RemoteEvent{}, err
	}
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil {
		return nil, nil, calendar.RemoteEvent{}, err
	}
	cal, headers, err := calendarUpdateCalDAVGet(ctx, calendarDeleteClient(calDAVHTTPTransport), endpoint, credentials.username, credentials.password)
	if err != nil {
		return nil, nil, calendar.RemoteEvent{}, err
	}
	if headers.Get("ETag") != existing.ETag {
		return nil, nil, calendar.RemoteEvent{}, errCalendarUpdateConflict
	}
	var event calendar.RemoteEvent
	if len(cal.Events()) == 1 && cal.Events()[0].Props.Get("ORGANIZER") == nil && cal.Events()[0].Props.Get("ATTENDEE") == nil {
		if reason := calendarUpdateCalDAVRestriction(cal, headers); reason != "" {
			return nil, nil, event, calendarUpdateUnsupported(reason)
		}
		event, err = normalizeCalDAVEvent(cal.Events()[0], endpoint, headers.Get("ETag"), time.UTC)
	} else {
		event, err = calendarMeetingNormalized(cal, headers, endpoint, account.Email)
	}
	if err == nil && existing.ICalUID != "" && event.ICalUID != existing.ICalUID {
		err = errCalendarUpdateConflict
	}
	return cal, headers, event, err
}

func (h *Handler) updateCalDAVMeeting(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	fail := func(err error) (calendar.RemoteEvent, error) {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	if draft.Recurrence != nil || existing.SeriesRemoteID != "" {
		return fail(calendarUpdateUnsupported("Recurring meetings with guests are not supported yet."))
	}
	current, headers, before, err := h.readCalDAVMeeting(ctx, source, credentials, existing)
	// A guestless appointment may become a meeting; its organizer is still
	// checked against the linked account, never against submitted form values.
	if err != nil {
		return fail(err)
	}
	oldAgent := ""
	if organizer := current.Events()[0].Props.Get("ORGANIZER"); organizer != nil {
		oldAgent = organizer.Params.Get("SCHEDULE-AGENT")
	}
	agent, email, name, err := h.calendarMeetingAgent(ctx, source, credentials, oldAgent)
	if err != nil {
		return fail(err)
	}
	draft.OrganizerEmail, draft.OrganizerName, draft.ScheduleAgent = email, name, agent
	oldGuests, err := calendarMeetingGuests(before.Attendees)
	if err != nil {
		return fail(err)
	}
	if !draft.GuestsSet {
		draft.Guests = filterCalendarOrganizerGuests(oldGuests, email)
	}
	for _, guest := range draft.Guests {
		if strings.EqualFold(guest.Email, email) {
			return fail(fmt.Errorf("you are already the organizer"))
		}
	}
	replacement, err := calendarCreateICS(draft)
	if err != nil {
		return fail(err)
	}
	edited, err := calendarUpdateDecodeICS([]byte(replacement))
	if err != nil {
		return fail(err)
	}
	old := &ical.Calendar{Component: cloneCalendarComponent(current.Component)}
	props := current.Events()[0].Props
	for _, field := range []string{"SUMMARY", "DESCRIPTION", "LOCATION", "DTSTART", "DTEND", "X-GOFER-TIMEZONE"} {
		if field == "DESCRIPTION" && draft.Description == calendarDescriptionText(existing.Description) {
			continue
		}
		props.Set(edited.Events()[0].Props.Get(field))
	}
	delete(props, "DURATION")
	var people []ical.Prop
	for _, person := range old.Events()[0].Props["ATTENDEE"] {
		if calendarReplyAddress(person.Value) == email {
			person.Params.Set("SCHEDULE-AGENT", agent)
			delete(person.Params, "SCHEDULE-STATUS")
			people = append(people, person)
		}
	}
	for _, guest := range draft.Guests {
		var person *ical.Prop
		for _, previous := range old.Events()[0].Props["ATTENDEE"] {
			if calendarReplyAddress(previous.Value) == guest.Email {
				p := previous
				person = &p
				break
			}
		}
		if person == nil {
			for _, added := range edited.Events()[0].Props["ATTENDEE"] {
				if calendarReplyAddress(added.Value) == guest.Email {
					p := added
					person = &p
					break
				}
			}
		}
		if person == nil {
			return fail(fmt.Errorf("could not prepare guest"))
		}
		person.Params.Set("SCHEDULE-AGENT", agent)
		delete(person.Params, "SCHEDULE-STATUS")
		people = append(people, *person)
	}
	props["ATTENDEE"] = people
	if props.Get("ORGANIZER") == nil {
		props.Set(&ical.Prop{Name: "ORGANIZER", Value: "mailto:" + email, Params: ical.Params{}})
		props.Get("ORGANIZER").Params.Set("CN", name)
	}
	props.Get("ORGANIZER").Params.Set("SCHEDULE-AGENT", agent)
	delete(props.Get("ORGANIZER").Params, "SCHEDULE-STATUS")
	sequence, err := calendarMeetingSequence(current, true)
	if err != nil {
		return fail(err)
	}
	props.SetText("SEQUENCE", sequence)
	props.SetDateTime("DTSTAMP", time.Now().UTC())
	props.SetDateTime("LAST-MODIFIED", time.Now().UTC())
	body, err := encodeCalendarReply(current)
	if err != nil {
		return fail(err)
	}
	key := existing.ETag + "\x00" + calendarDraftHash(draft)
	if agent == "CLIENT" {
		if err := h.queueCalendarNotification(ctx, source, existing.RemoteID, key, "REQUEST", current, current, draft.Guests, false); err != nil {
			return fail(err)
		}
		wanted := map[string]bool{}
		for _, guest := range draft.Guests {
			wanted[guest.Email] = true
		}
		var removed []calendar.GuestDraft
		for _, guest := range oldGuests {
			if !wanted[guest.Email] && !strings.EqualFold(guest.Email, email) {
				removed = append(removed, guest)
			}
		}
		old.Events()[0].Props.SetText("SEQUENCE", sequence)
		if err := h.queueCalendarNotification(ctx, source, existing.RemoteID, key, "CANCEL", old, current, removed, false); err != nil {
			return fail(err)
		}
	}
	req, err := newCardDAVRequest(ctx, http.MethodPut, existing.RemoteID, credentials.username, credentials.password, strings.NewReader(body))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	req.Header.Set("If-Match", headers.Get("ETag"))
	if tag := headers.Get("Schedule-Tag"); tag != "" {
		if !calendarUpdateValidETag(tag, false) {
			return fail(fmt.Errorf("unsafe scheduling version"))
		}
		req.Header.Set("If-Schedule-Tag-Match", tag)
	}
	res, err := calendarDeleteClient(calDAVHTTPTransport).Do(req)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	status := res.StatusCode
	res.Body.Close()
	if status < 200 || status >= 300 {
		return calendar.RemoteEvent{}, calendarUpdateHTTPError(calendarCreateProviderError{status})
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the update")
	}
	confirmed, responseHeaders, err := calendarUpdateCalDAVGet(ctx, calendarDeleteClient(calDAVHTTPTransport), existing.RemoteID, credentials.username, credentials.password)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("could not confirm meeting update: %v", err)
	}
	updated, err := calendarMeetingNormalized(confirmed, responseHeaders, existing.RemoteID, email)
	if err != nil || updated.ICalUID != before.ICalUID || !calendarUpdateMatchesDraft(updated, draft) || !calendarMeetingGuestsMatch(updated.Attendees, draft.Guests, email) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the saved meeting")
	}
	h.signalOutgoingWorker()
	return updated, nil
}

func (h *Handler) deleteCalDAVMeeting(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, existing storage.CalendarEvent) error {
	cal, headers, before, err := h.readCalDAVMeeting(ctx, source, credentials, existing)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	agent, _, _, err := h.calendarMeetingAgent(ctx, source, credentials, cal.Events()[0].Props.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT"))
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	sequence, err := calendarMeetingSequence(cal, true)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	cal.Events()[0].Props.SetText("SEQUENCE", sequence)
	if agent == "CLIENT" {
		guests, err := calendarMeetingGuests(before.Attendees)
		if err != nil {
			return calendarDeletePreflightError{err}
		}
		if err := h.queueCalendarNotification(ctx, source, existing.RemoteID, existing.ETag, "CANCEL", cal, nil, guests, true); err != nil {
			return calendarDeletePreflightError{err}
		}
	}
	req, err := newCardDAVRequest(ctx, http.MethodDelete, existing.RemoteID, credentials.username, credentials.password, nil)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	req.Header.Set("If-Match", headers.Get("ETag"))
	if tag := headers.Get("Schedule-Tag"); tag != "" {
		if !calendarUpdateValidETag(tag, false) {
			return calendarDeletePreflightError{fmt.Errorf("unsafe scheduling version")}
		}
		req.Header.Set("If-Schedule-Tag-Match", tag)
	}
	err = calendarDeleteHTTP(calendarDeleteClient(calDAVHTTPTransport), req, true)
	h.signalOutgoingWorker()
	return err
}

// Compare scheduling content, ignoring server timestamps and guest replies.
func calendarMeetingResourceMatches(expected, actual *ical.Calendar) bool {
	if len(expected.Events()) != 1 || len(actual.Events()) != 1 {
		return false
	}
	left, right := expected.Events()[0].Props, actual.Events()[0].Props
	for _, name := range []string{"UID", "SEQUENCE", "STATUS"} {
		a, b := left.Get(name), right.Get(name)
		if (a == nil) != (b == nil) || (a != nil && a.Value != b.Value) {
			return false
		}
	}
	if left.Get("ORGANIZER") == nil || right.Get("ORGANIZER") == nil || calendarReplyAddress(left.Get("ORGANIZER").Value) != calendarReplyAddress(right.Get("ORGANIZER").Value) {
		return false
	}
	a, err := normalizeCalDAVEvent(expected.Events()[0], "", "", time.UTC)
	if err != nil {
		return false
	}
	b, err := normalizeCalDAVEvent(actual.Events()[0], "", "", time.UTC)
	if err != nil {
		return false
	}
	guests, err := calendarMeetingGuests(a.Attendees)
	if err != nil {
		return false
	}
	draft := calendar.EventDraft{Summary: a.Summary, Description: a.Description, Location: a.Location, AllDay: a.AllDay, StartAt: a.StartAt, EndAt: a.EndAt, StartDate: a.StartDate, EndDate: a.EndDate}
	if !calendarUpdateMatchesDraft(b, draft) {
		return false
	}
	// Include the organizer's attendee entry when a provider stores one.
	return calendarMeetingGuestsMatch(b.Attendees, filterCalendarOrganizerGuests(guests, a.OrganizerEmail), a.OrganizerEmail)
}

func filterCalendarOrganizerGuests(guests []calendar.GuestDraft, organizer string) []calendar.GuestDraft {
	var result []calendar.GuestDraft
	for _, guest := range guests {
		if !strings.EqualFold(guest.Email, organizer) {
			result = append(result, guest)
		}
	}
	return result
}

func calendarNotificationMissing(err error) bool {
	var provider calendarCreateProviderError
	return errors.As(err, &provider) && (provider.Status == 404 || provider.Status == 410)
}
