package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// A CalDAV meeting resource holds one event, or a series: the event with the
// repeat rule plus one event per date changed on its own (RECURRENCE-ID).
// Gofer writes the scheduling messages itself, one event per message.

// calendarMeetingMaster returns the single event, or the series' own event.
func calendarMeetingMaster(cal *ical.Calendar) *ical.Component {
	for _, event := range cal.Events() {
		if event.Props.Get("RECURRENCE-ID") == nil {
			return event.Component
		}
	}
	return nil
}

// calendarMeetingDateKey identifies a date of a series the way the cache does.
func calendarMeetingDateKey(component *ical.Component) string {
	rid := component.Props.Get("RECURRENCE-ID")
	if rid == nil {
		return ""
	}
	key, err := calendarRecurrenceKey(rid, time.UTC)
	if err != nil {
		return "\x00invalid"
	}
	return key
}

// calendarMeetingComponent returns the event for a date of the series, or the
// series' own event for an empty key. Nil when that date has no event of its own.
func calendarMeetingComponent(cal *ical.Calendar, key string) *ical.Component {
	for _, event := range cal.Events() {
		if calendarMeetingDateKey(event.Component) == key {
			return event.Component
		}
	}
	return nil
}

// calendarMeetingExcluded reports whether the series no longer has that date.
func calendarMeetingExcluded(master *ical.Component, key string) bool {
	for _, prop := range master.Props["EXDATE"] {
		for _, value := range strings.Split(prop.Value, ",") {
			one := prop
			one.Value = value
			if candidate, err := calendarRecurrenceKey(&one, time.UTC); err == nil && candidate == key {
				return true
			}
		}
	}
	return false
}

// calendarMeetingMessageSource is the calendar a single scheduling message is
// built from: the time zones and just one event of the resource.
func calendarMeetingMessageSource(cal *ical.Calendar, component *ical.Component) *ical.Calendar {
	message := &ical.Calendar{Component: ical.NewComponent(ical.CompCalendar)}
	for name, props := range cal.Props {
		message.Props[name] = props
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			message.Children = append(message.Children, cloneCalendarComponent(child))
		}
	}
	message.Children = append(message.Children, cloneCalendarComponent(component))
	return message
}

// calendarMeetingDateEvent builds a date's own event from the series' event,
// for a date that has none yet: same details, at that date's time.
func calendarMeetingDateEvent(master *ical.Component, rid *ical.Prop) (*ical.Component, error) {
	date := cloneCalendarComponent(master)
	for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
		delete(date.Props, name)
	}
	date.Props.Set(rid)
	plain := ical.Event{Component: cloneCalendarComponent(date)}
	start, err := plain.DateTimeStart(time.UTC)
	if err != nil {
		return nil, err
	}
	end, err := plain.DateTimeEnd(time.UTC)
	if err != nil {
		return nil, err
	}
	at, err := rid.DateTime(time.UTC)
	if err != nil {
		return nil, err
	}
	startProp := *rid
	startProp.Name = "DTSTART"
	startProp.Params = cloneCalendarParams(rid.Params)
	date.Props.Set(&startProp)
	delete(date.Props, "DURATION")
	endProp := ical.NewProp("DTEND")
	if rid.ValueType() == ical.ValueDate {
		endProp.SetDate(at.AddDate(0, 0, int(end.Sub(start).Hours()/24)))
	} else {
		zone := time.UTC
		if id := rid.Params.Get("TZID"); id != "" {
			if location, err := time.LoadLocation(id); err == nil {
				zone = location
			}
		}
		endProp.SetDateTime(at.Add(end.Sub(start)).In(zone))
	}
	date.Props.Set(endProp)
	return date, nil
}

func cloneCalendarParams(params ical.Params) ical.Params {
	copy := make(ical.Params)
	for key, values := range params {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}

// calendarMeetingSeriesRule returns the event's repeat rule as the cache stores it.
func calendarMeetingSeriesRule(master *ical.Component) json.RawMessage {
	if rule := master.Props.Get("RRULE"); rule != nil {
		raw, _ := json.Marshal([]string{"RRULE:" + rule.Value})
		return raw
	}
	return nil
}

// calendarMeetingAllGuests lists everyone invited to any date of the meeting.
func calendarMeetingAllGuests(cal *ical.Calendar, organizer string) ([]calendar.GuestDraft, error) {
	seen := map[string]bool{}
	var guests []calendar.GuestDraft
	for _, event := range cal.Events() {
		for _, person := range event.Props["ATTENDEE"] {
			email := calendarReplyAddress(person.Value)
			if email == "" {
				return nil, calendarUpdateUnsupported("This meeting has an unsupported participant.")
			}
			if seen[email] || strings.EqualFold(email, organizer) {
				continue
			}
			seen[email] = true
			guests = append(guests, calendar.GuestDraft{Email: email, Name: person.Params.Get("CN")})
		}
	}
	sort.Slice(guests, func(i, j int) bool { return guests[i].Email < guests[j].Email })
	return guests, nil
}

// calendarMeetingComponentGuests lists the guests of one event of the meeting.
func calendarMeetingComponentGuests(component *ical.Component, organizer string) []calendar.GuestDraft {
	var guests []calendar.GuestDraft
	for _, person := range component.Props["ATTENDEE"] {
		if email := calendarReplyAddress(person.Value); email != "" && !strings.EqualFold(email, organizer) {
			guests = append(guests, calendar.GuestDraft{Email: email, Name: person.Params.Get("CN")})
		}
	}
	return guests
}

// calendarMeetingExdates returns a series' removed dates, in a stable order.
func calendarMeetingExdates(master *ical.Component) []string {
	var keys []string
	for _, prop := range master.Props["EXDATE"] {
		for _, value := range strings.Split(prop.Value, ",") {
			one := prop
			one.Value = value
			key, err := calendarRecurrenceKey(&one, time.UTC)
			if err != nil {
				key = "\x00invalid"
			}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// calendarMeetingCancelSource is the series' own event, listing everyone
// invited to any of its dates, so one cancellation reaches them all.
func calendarMeetingCancelSource(cal *ical.Calendar, master *ical.Component) *ical.Calendar {
	message := calendarMeetingMessageSource(cal, master)
	event := message.Events()[0]
	listed := map[string]bool{}
	for _, person := range event.Props["ATTENDEE"] {
		listed[calendarReplyAddress(person.Value)] = true
	}
	for _, other := range cal.Events() {
		for _, person := range other.Props["ATTENDEE"] {
			if email := calendarReplyAddress(person.Value); email != "" && !listed[email] {
				listed[email] = true
				copy := person
				copy.Params = cloneCalendarParams(person.Params)
				event.Props.Add(&copy)
			}
		}
	}
	return message
}

// calendarCalDAVMeetingSeriesEvent reads a series meeting the organizer owns,
// to edit or delete it as a whole, however its dates were changed.
func calendarCalDAVMeetingSeriesEvent(cal *ical.Calendar, headers http.Header, endpoint, organizer string) (calendar.RemoteEvent, error) {
	if err := calendarMeetingCalDAVShape(cal, headers, organizer); err != nil {
		return calendar.RemoteEvent{}, err
	}
	master := calendarMeetingMaster(cal)
	if master.Props.Get("RRULE") == nil {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported(calendarSeriesUnsupported)
	}
	if start := master.Props.Get("DTSTART"); start != nil && start.ValueType() != ical.ValueDate && start.Params.Get("TZID") == "" && !strings.HasSuffix(start.Value, "Z") {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("This series uses floating times. Edit it in your calendar provider to preserve its timezone behavior.")
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("The provider did not supply a safe series version.")
	}
	event, err := calendarMeetingNormalized(cal, headers, endpoint, organizer)
	if err != nil {
		return event, err
	}
	_, err = calendarSeriesDraft(event)
	return event, err
}

// updateCalDAVMeetingOccurrence changes one date of a series meeting. A nil
// draft removes the date. Guests of that date get an invitation for just that
// date, and guests removed from it, or invited to a removed date, a
// cancellation for it. The rest of the series is kept as it is.
func (h *Handler) updateCalDAVMeetingOccurrence(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, existing storage.CalendarEvent, draft *calendar.EventDraft) (calendar.RemoteEvent, error) {
	fail := func(err error) (calendar.RemoteEvent, error) {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	if err := calendarOccurrenceExistingRestriction(existing); err != nil {
		return fail(err)
	}
	if !calendarUpdateValidETag(existing.ETag, false) {
		return fail(errCalendarUpdateUnsupported)
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, storage.CalendarEvent{RemoteID: existing.SeriesRemoteID})
	if err != nil {
		return fail(err)
	}
	collection, _ := url.Parse(source.RemoteID)
	resource, _ := url.Parse(endpoint)
	if !strings.HasPrefix(resource.Path, strings.TrimSuffix(collection.Path, "/")+"/") || strings.HasSuffix(resource.Path, "/") || path.Clean(resource.Path) != resource.Path {
		return fail(calendarUpdateUnsupported("The series must be a resource inside this calendar."))
	}
	if draft != nil {
		if draft.Recurrence != nil {
			return fail(errCalendarUpdateUnsupported)
		}
		if _, err := calendarUpdateDraftLocation(*draft); err != nil {
			return fail(err)
		}
	}
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil {
		return fail(err)
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
	if err != nil {
		return fail(err)
	}
	if headers.Get("ETag") != existing.ETag {
		return fail(errCalendarUpdateConflict)
	}
	if err := calendarMeetingCalDAVShape(current, headers, account.Email); err != nil {
		return fail(err)
	}
	master, override, rid, err := calendarCalDAVOccurrenceResource(current, headers, existing, true)
	if err != nil {
		return fail(err)
	}
	if err := calendarCalDAVVerifyOccurrence(ctx, source, credentials.username, credentials.password, existing, true); err != nil {
		return fail(err)
	}
	key, err := calendarRecurrenceKey(rid, time.UTC)
	if err != nil {
		return fail(err)
	}
	agent, email, name, err := h.calendarMeetingAgent(ctx, source, credentials, master.Props.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT"))
	if err != nil {
		return fail(err)
	}
	// What the date's guests had before this change, for the cancellation.
	before := override
	if before == nil {
		if before, err = calendarMeetingDateEvent(master, rid); err != nil {
			return fail(err)
		}
	} else {
		before = cloneCalendarComponent(before)
	}
	oldGuests := calendarMeetingComponentGuests(before, email)
	var invited, removed []calendar.GuestDraft
	var request *ical.Component
	if draft == nil {
		sequence, err := calendarMeetingComponentSequence(master, true)
		if err != nil {
			return fail(err)
		}
		master.Props.SetText("SEQUENCE", sequence)
		exdate := *rid
		exdate.Name = "EXDATE"
		exdate.Params = cloneCalendarParams(rid.Params)
		master.Props.Add(&exdate)
		if override != nil {
			children := current.Children[:0]
			for _, child := range current.Children {
				if child != override {
					children = append(children, child)
				}
			}
			current.Children = children
		}
		before.Props.SetText("SEQUENCE", sequence)
		removed = oldGuests
	} else {
		if override == nil {
			override = cloneCalendarComponent(master)
			for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
				delete(override.Props, name)
			}
			override.Props.Set(rid)
			current.Children = append(current.Children, override)
		}
		changed := *draft
		changed.OrganizerEmail, changed.OrganizerName, changed.ScheduleAgent = email, name, agent
		if !changed.GuestsSet {
			changed.Guests = oldGuests
		}
		for _, guest := range changed.Guests {
			if strings.EqualFold(guest.Email, email) {
				return fail(fmt.Errorf("you are already the organizer"))
			}
		}
		raw, err := calendarCreateICS(changed)
		if err != nil {
			return fail(err)
		}
		edited, err := calendarUpdateDecodeICS([]byte(raw))
		if err != nil {
			return fail(err)
		}
		for _, field := range []string{"SUMMARY", "LOCATION", "DTSTART", "DTEND", "X-GOFER-TIMEZONE"} {
			calendarCopyICSProp(override.Props, edited.Events()[0].Props, field)
		}
		calendarUpdateDescriptionProps(override.Props, edited.Events()[0].Props, existing.Description, *draft)
		delete(override.Props, "DURATION")
		if draft.ReminderSet && !calendarSameReminder(draft.ReminderMinutes, existing.ReminderMinutes) {
			calDAVSetReminder(override, draft.ReminderMinutes)
		}
		if draft.ShowAs != "" && !calendarSameShowAs(draft.ShowAs, existing.ShowAs) {
			calDAVSetShowAs(override, draft.ShowAs)
		}
		if draft.PrivateSet && draft.Private != existing.Private {
			calDAVSetPrivate(override, draft.Private)
		}
		// Keep each remaining guest's reply; add new guests as Gofer invites them.
		var people []ical.Prop
		for _, person := range override.Props["ATTENDEE"] {
			if calendarReplyAddress(person.Value) == email {
				people = append(people, person)
			}
		}
		for _, guest := range changed.Guests {
			var person *ical.Prop
			for _, candidate := range append(append([]ical.Prop{}, override.Props["ATTENDEE"]...), edited.Events()[0].Props["ATTENDEE"]...) {
				if calendarReplyAddress(candidate.Value) == guest.Email {
					p := candidate
					p.Params = cloneCalendarParams(candidate.Params)
					person = &p
					break
				}
			}
			if person == nil {
				return fail(fmt.Errorf("could not prepare guest"))
			}
			people = append(people, *person)
		}
		for i := range people {
			people[i].Params.Set("SCHEDULE-AGENT", agent)
			delete(people[i].Params, "SCHEDULE-STATUS")
		}
		override.Props["ATTENDEE"] = people
		override.Props.Get("ORGANIZER").Params.Set("SCHEDULE-AGENT", agent)
		delete(override.Props.Get("ORGANIZER").Params, "SCHEDULE-STATUS")
		// The date's revision must pass both its own and the series'.
		dateSequence, err := calendarMeetingComponentSequence(override, false)
		if err != nil {
			return fail(err)
		}
		seriesSequence, err := calendarMeetingComponentSequence(master, false)
		if err != nil {
			return fail(err)
		}
		next, _ := strconv.Atoi(dateSequence)
		if series, _ := strconv.Atoi(seriesSequence); series > next {
			next = series
		}
		override.Props.SetText("SEQUENCE", strconv.Itoa(next+1))
		before.Props.SetText("SEQUENCE", strconv.Itoa(next+1))
		override.Props.SetDateTime("DTSTAMP", time.Now().UTC())
		override.Props.SetDateTime("LAST-MODIFIED", time.Now().UTC())
		invited = changed.Guests
		wanted := map[string]bool{}
		for _, guest := range invited {
			wanted[guest.Email] = true
		}
		for _, guest := range oldGuests {
			if !wanted[guest.Email] {
				removed = append(removed, guest)
			}
		}
		request = override
	}
	if agent == "CLIENT" {
		notification := existing.ETag + "\x00" + key
		if draft != nil {
			notification += "\x00" + calendarDraftHash(*draft)
		}
		if request != nil {
			if err := h.queueCalendarNotification(ctx, source, endpoint, notification, "REQUEST", calendarMeetingMessageSource(current, request), current, invited, false); err != nil {
				return fail(err)
			}
		}
		if err := h.queueCalendarNotification(ctx, source, endpoint, notification, "CANCEL", calendarMeetingMessageSource(current, before), current, removed, false); err != nil {
			return fail(err)
		}
	}
	body, err := encodeCalendarReply(current)
	if err != nil {
		return fail(err)
	}
	req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, credentials.username, credentials.password, strings.NewReader(body))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	req.Header.Set("If-Match", existing.ETag)
	if tag := headers.Get("Schedule-Tag"); tag != "" {
		if !calendarUpdateValidETag(tag, false) {
			return fail(fmt.Errorf("unsafe scheduling version"))
		}
		req.Header.Set("If-Schedule-Tag-Match", tag)
	}
	response, err := calendarProviderDo(client, req)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	status, putETag := response.StatusCode, response.Header.Get("ETag")
	response.Body.Close()
	if status < 200 || status >= 300 {
		return calendar.RemoteEvent{}, calendarUpdateHTTPError(calendarCreateProviderError{status})
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the date's change (HTTP %d)", status)
	}
	h.wakeCalendarNotificationWorker(ctx, source)
	confirmed, confirmedHeaders, err := calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the date's change: %v", err)
	}
	etag := confirmedHeaders.Get("ETag")
	if !calendarUpdateValidETag(etag, false) || (calendarUpdateValidETag(putETag, false) && putETag != etag) {
		return calendar.RemoteEvent{}, fmt.Errorf("the meeting changed before its change could be confirmed")
	}
	if err := calendarMeetingCalDAVShape(confirmed, confirmedHeaders, email); err != nil || !calendarMeetingResourceMatches(current, confirmed) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the saved series and its dates")
	}
	if draft == nil {
		return calendar.RemoteEvent{RemoteID: existing.RemoteID, SeriesRemoteID: existing.SeriesRemoteID, ETag: etag, Deleted: true}, nil
	}
	saved := calendarMeetingComponent(confirmed, key)
	if saved == nil {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not return the saved date")
	}
	remote, err := normalizeCalDAVEvent(ical.Event{Component: saved}, endpoint, etag, time.UTC)
	if err != nil || remote.RemoteID != existing.RemoteID || !calendarUpdateMatchesDraft(remote, *draft) || !calendarMeetingGuestsMatch(remote.Attendees, invited, email) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the date's changes")
	}
	remote.ResponseStatus = "organizer"
	remote.StartTimeZone, remote.EndTimeZone = draft.TimeZone, draft.TimeZone
	return remote, nil
}

// calendarMeetingTimingChanged reports whether a series edit moves its dates:
// a new start, end, time zone or repeat rule. Its changed and removed dates
// are identified by their original time, so they can't follow such a change.
func calendarMeetingTimingChanged(before, after *ical.Component) bool {
	span := func(component *ical.Component) (time.Time, time.Time, bool, string, error) {
		event := ical.Event{Component: component}
		start, err := event.DateTimeStart(time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, false, "", err
		}
		end, err := event.DateTimeEnd(time.UTC)
		prop := component.Props.Get("DTSTART")
		return start, end, prop.ValueType() == ical.ValueDate, prop.Params.Get("TZID"), err
	}
	aStart, aEnd, aDay, aZone, errA := span(before)
	bStart, bEnd, bDay, bZone, errB := span(after)
	rule := func(component *ical.Component) string {
		if prop := component.Props.Get("RRULE"); prop != nil {
			return prop.Value
		}
		return ""
	}
	return errA != nil || errB != nil || !aStart.Equal(bStart) || !aEnd.Equal(bEnd) || aDay != bDay || aZone != bZone || rule(before) != rule(after)
}

// calendarMeetingCarryDates brings a series edit to the dates changed one by
// one. A title, place, description or guest change reaches every date, except
// that a date keeps what it had changed on its own; removed dates stay
// removed. A time or repeat change can't keep those dates: with the
// organizer's confirmation (reset) they follow the series again, removed
// dates included.
func calendarMeetingCarryDates(current *ical.Calendar, before, master *ical.Component, reset bool, agent, organizer, sequence string) error {
	if calendarMeetingTimingChanged(before, master) {
		if !reset {
			return errCalendarSeriesDatesReset
		}
		children := current.Children[:0]
		for _, child := range current.Children {
			if child.Name != ical.CompEvent || child == master {
				children = append(children, child)
			}
		}
		current.Children = children
		delete(master.Props, "EXDATE")
		return nil
	}
	same := func(a, b *ical.Component, name string) bool {
		x, y := a.Props[name], b.Props[name]
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Value != y[i].Value {
				return false
			}
		}
		return true
	}
	copyField := func(date *ical.Component, name string) {
		delete(date.Props, name)
		for _, prop := range master.Props[name] {
			copy := prop
			copy.Params = cloneCalendarParams(prop.Params)
			date.Props.Add(&copy)
		}
	}
	guests := func(component *ical.Component) map[string]bool {
		set := map[string]bool{}
		for _, person := range component.Props["ATTENDEE"] {
			set[calendarReplyAddress(person.Value)] = true
		}
		return set
	}
	wasInvited, isInvited := guests(before), guests(master)
	next, _ := strconv.Atoi(sequence)
	for _, event := range current.Events() {
		date := event.Component
		if date == master {
			continue
		}
		for _, name := range []string{"SUMMARY", "LOCATION"} {
			if same(date, before, name) {
				copyField(date, name)
			}
		}
		if same(date, before, "DESCRIPTION") && same(date, before, "X-ALT-DESC") {
			copyField(date, "DESCRIPTION")
			copyField(date, "X-ALT-DESC")
		}
		// Guests who leave the series leave its dates; new guests join them.
		var people []ical.Prop
		listed := map[string]bool{}
		for _, person := range date.Props["ATTENDEE"] {
			email := calendarReplyAddress(person.Value)
			if wasInvited[email] && !isInvited[email] {
				continue
			}
			listed[email] = true
			people = append(people, person)
		}
		for _, person := range master.Props["ATTENDEE"] {
			if email := calendarReplyAddress(person.Value); !wasInvited[email] && !listed[email] {
				copy := person
				copy.Params = cloneCalendarParams(person.Params)
				people = append(people, copy)
			}
		}
		for i := range people {
			people[i].Params.Set("SCHEDULE-AGENT", agent)
			delete(people[i].Params, "SCHEDULE-STATUS")
		}
		date.Props["ATTENDEE"] = people
		if prop := date.Props.Get("ORGANIZER"); prop != nil && calendarReplyAddress(prop.Value) == organizer {
			prop.Params.Set("SCHEDULE-AGENT", agent)
			delete(prop.Params, "SCHEDULE-STATUS")
		}
		// Each date's revision must pass the series'.
		if own, err := calendarMeetingComponentSequence(date, false); err == nil {
			if value, _ := strconv.Atoi(own); value > next {
				next = value
			}
		}
	}
	for _, event := range current.Events() {
		if event.Component != master {
			event.Props.SetText("SEQUENCE", strconv.Itoa(next))
		}
	}
	if next > 0 {
		master.Props.SetText("SEQUENCE", strconv.Itoa(next))
	}
	return nil
}

// calendarCalDAVSeriesOwnEvent returns a series appointment as just its own
// event, without its removed dates, after checking each date changed on its
// own: the same UID, one plain RECURRENCE-ID, no repeat rule of its own, and
// otherwise the checks of an ordinary appointment. A single event is returned
// as it is.
func calendarCalDAVSeriesOwnEvent(cal *ical.Calendar, headers http.Header) (*ical.Calendar, error) {
	master := calendarMeetingMaster(cal)
	if master == nil || (len(cal.Events()) == 1 && master.Props.Get("EXDATE") == nil) {
		return cal, nil
	}
	uid, err := master.Props.Text("UID")
	if err != nil || uid == "" || master.Props.Get("RRULE") == nil {
		return nil, calendarUpdateUnsupported(calendarSeriesUnsupported)
	}
	seen := map[string]bool{}
	for _, event := range cal.Events() {
		if event.Component == master {
			continue
		}
		key := calendarMeetingDateKey(event.Component)
		value, err := event.Props.Text("UID")
		if err != nil || value != uid || len(event.Props["UID"]) != 1 || len(event.Props["RECURRENCE-ID"]) != 1 || strings.HasPrefix(key, "\x00") || seen[key] {
			return nil, calendarUpdateUnsupported("The series has unsupported or ambiguous exceptions.")
		}
		seen[key] = true
		check := cloneCalendarComponent(event.Component)
		delete(check.Props, "RECURRENCE-ID")
		delete(check.Props, "STATUS") // A cancelled date may stay as it is.
		one := ical.NewCalendar()
		one.Children = []*ical.Component{check}
		if reason := calendarUpdateCalDAVRestriction(one, headers); reason != "" {
			return nil, calendarUpdateUnsupported(reason)
		}
	}
	own := &ical.Calendar{Component: ical.NewComponent(ical.CompCalendar)}
	for name, props := range cal.Props {
		own.Props[name] = props
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			own.Children = append(own.Children, cloneCalendarComponent(child))
		}
	}
	plain := cloneCalendarComponent(master)
	delete(plain.Props, "EXDATE")
	own.Children = append(own.Children, plain)
	return own, nil
}
