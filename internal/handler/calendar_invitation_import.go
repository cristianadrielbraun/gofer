package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	stdmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-message/mail"
	"github.com/google/uuid"
)

// Invitations from email to a CalDAV calendar. Google and Outlook put an
// invitation in the invited account's calendar themselves; a CalDAV server
// doesn't, so Gofer does: an invitation its organizer's domain signed is added
// by itself (see calendar_invitation_auto.go), and any other the invited
// person adds from the email. Either way it is answered there as usual. A later
// invitation for it updates it, and a cancellation removes it. Gofer only acts
// on events organized by someone else.

// calendarInvitationRefusal says why Gofer won't add, update or remove an
// invitation's event; it is shown to the person as it is.
type calendarInvitationRefusal string

func (e calendarInvitationRefusal) Error() string { return string(e) }

// mailInvitation is the scheduling message in an email for one event UID.
type mailInvitation struct {
	method, organizer string
	forwardedBy       string         // Who sent it on, when not its organizer.
	calendar          *ical.Calendar // The message's time zones and its events for the UID.
}

// readMailInvitation finds the REQUEST or CANCEL for uid in an email, and
// checks it invites (or cancels for) the account. It must come from its
// organizer, unless forwarded is allowed.
func readMailInvitation(raw []byte, uid, account string, forwarded bool) (invitation mailInvitation, err error) {
	defer func() {
		if recover() != nil {
			err = calendarInvitationRefusal("the invitation could not be read")
		}
	}()
	if len(raw) > message.CalendarIncomingMaxSize {
		return invitation, calendarInvitationRefusal("the email is too large")
	}
	header, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil || len(header.Header["From"]) != 1 {
		return invitation, calendarInvitationRefusal("the email has no single sender")
	}
	from, err := stdmail.ParseAddress(header.Header.Get("From"))
	if err != nil {
		return invitation, calendarInvitationRefusal("the email has no single sender")
	}
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return invitation, calendarInvitationRefusal("the invitation could not be read")
	}
	defer reader.Close()
	for count := 0; count < 64; count++ {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		var kind, filename string
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			kind, _, _ = h.ContentType()
		case *mail.AttachmentHeader:
			kind, _, _ = h.ContentType()
			filename, _ = h.Filename()
		}
		if kind != "text/calendar" && kind != "application/ics" && kind != "application/icalendar" && !strings.HasSuffix(strings.ToLower(filename), ".ics") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part.Body, (256<<10)+1))
		if err != nil || len(data) > 256<<10 {
			continue
		}
		decoder := ical.NewDecoder(bytes.NewReader(data))
		cal, err := decoder.Decode()
		if err != nil {
			continue
		}
		if _, err := decoder.Decode(); err != io.EOF {
			continue
		}
		method, _ := cal.Props.Text("METHOD")
		method = strings.ToUpper(method)
		if method != "REQUEST" && method != "CANCEL" {
			continue
		}
		selected := &ical.Calendar{Component: ical.NewComponent(ical.CompCalendar)}
		selected.Props.SetText("PRODID", "-//Gofer//Calendar//EN")
		selected.Props.SetText("VERSION", "2.0")
		for _, child := range cal.Children {
			if child.Name == ical.CompTimezone {
				selected.Children = append(selected.Children, cloneCalendarComponent(child))
			}
		}
		for _, event := range cal.Events() {
			if value, err := event.Props.Text("UID"); err == nil && value == uid && len(event.Props["UID"]) == 1 {
				selected.Children = append(selected.Children, cloneCalendarComponent(event.Component))
			}
		}
		if len(selected.Events()) == 0 {
			continue
		}
		calendarInvitationZones(selected)
		invitation = mailInvitation{method: method, calendar: selected}
		return invitation, calendarInvitationCheck(&invitation, strings.ToLower(from.Address), strings.ToLower(account), forwarded)
	}
	return invitation, calendarInvitationRefusal("the email has no invitation for this event")
}

// calendarInvitationZones names Windows time zones, as Outlook writes them,
// by their IANA names, which Gofer and every calendar read; the zones' own
// rules stay with them. Unknown zones are left as they are.
func calendarInvitationZones(cal *ical.Calendar) {
	renamed := map[string]string{}
	for _, event := range cal.Events() {
		for name, props := range event.Props {
			for i := range props {
				zone := props[i].Params.Get("TZID")
				if zone == "" {
					continue
				}
				if _, err := time.LoadLocation(zone); err == nil {
					continue
				}
				if location := outlookCalendarTimeLocation(zone); location != time.UTC {
					props[i].Params.Set("TZID", location.String())
					renamed[zone] = location.String()
				}
			}
			event.Props[name] = props
		}
	}
	named := map[string]bool{}
	children := cal.Children[:0]
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			zone, _ := child.Props.Text("TZID")
			if name, ok := renamed[zone]; ok {
				zone = name
				child.Props.SetText("TZID", name)
			}
			if named[zone] {
				continue
			}
			named[zone] = true
		}
		children = append(children, child)
	}
	cal.Children = children
}

// Every event must have one organizer, the email's sender unless forwarded
// invitations are allowed, list the account as a guest, and otherwise be an
// ordinary event or series date.
func calendarInvitationCheck(invitation *mailInvitation, from, account string, forwarded bool) error {
	cal := invitation.calendar
	seen := map[string]bool{}
	organizer := ""
	for _, event := range cal.Events() {
		props := event.Props
		address := ""
		if len(props["ORGANIZER"]) == 1 {
			address = calendarReplyAddress(props.Get("ORGANIZER").Value)
		}
		if address == "" || (organizer != "" && address != organizer) {
			return calendarInvitationRefusal("the invitation has no single organizer")
		}
		organizer = address
		if address != from && !forwarded {
			return calendarInvitationRefusal("the invitation isn't from its organizer")
		}
		if address == account {
			return calendarInvitationRefusal("you organize this event")
		}
		invited := false
		for _, person := range props["ATTENDEE"] {
			email := calendarReplyAddress(person.Value)
			if email == "" {
				return calendarInvitationRefusal("the invitation has an unsupported guest")
			}
			invited = invited || email == account
		}
		if !invited {
			return calendarInvitationRefusal("this account isn't invited")
		}
		key := calendarMeetingDateKey(event.Component)
		if strings.HasPrefix(key, "\x00") || seen[key] || props.Get("RDATE") != nil || props.Get("EXRULE") != nil || (key != "" && (props.Get("RRULE") != nil || props.Get("EXDATE") != nil)) {
			return calendarInvitationRefusal("the invitation repeats in a way Gofer can't add")
		}
		seen[key] = true
		if _, err := calendarMeetingComponentSequence(event.Component, false); err != nil {
			return calendarInvitationRefusal("the invitation has an invalid revision")
		}
		if invitation.method == "REQUEST" {
			plain := cloneCalendarComponent(event.Component)
			delete(plain.Props, "RRULE")
			delete(plain.Props, "EXDATE")
			if _, err := normalizeCalDAVEvent(ical.Event{Component: plain}, "", "", time.UTC); err != nil {
				return calendarInvitationRefusal("the invitation's date can't be read")
			}
		}
	}
	invitation.organizer = organizer
	if organizer != from {
		invitation.forwardedBy = from
	}
	return nil
}

// calendarInvitationResource is the invitation as a calendar resource: no
// METHOD, and none of the organizer's scheduling details.
func calendarInvitationResource(invitation mailInvitation) *ical.Calendar {
	resource := &ical.Calendar{Component: cloneCalendarComponent(invitation.calendar.Component)}
	delete(resource.Props, "METHOD")
	for _, event := range resource.Events() {
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
	}
	return resource
}

// applyCalDAVInvitation adds the invitation to the calendar (existing is
// empty), or updates or removes the event it is for. It returns the
// resource written.
func applyCalDAVInvitation(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, invitation mailInvitation, existing storage.CalendarEvent) (string, error) {
	if !storage.CalendarSourceMailServer(source.Provider) || !calendarSourceWritable(source) {
		return "", calendarInvitationRefusal("choose a writable CalDAV calendar")
	}
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		return "", err
	}
	// Anyone can forward an invitation; only its organizer can change it.
	if invitation.forwardedBy != "" && existing.ID != "" {
		return "", calendarInvitationRefusal("only the organizer can change an event that's already in your calendar")
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	put := func(endpoint string, cal *ical.Calendar, match string) error {
		body, err := encodeCalendarReply(cal)
		if err != nil {
			return err
		}
		req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, credentials.username, credentials.password, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		if match == "" {
			req.Header.Set("If-None-Match", "*")
		} else {
			req.Header.Set("If-Match", match)
		}
		response, err := calendarProviderDo(client, req)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return calendarUpdateHTTPError(calendarCreateProviderError{response.StatusCode})
		}
		return nil
	}
	if existing.ID == "" {
		if invitation.method != "REQUEST" {
			return "", calendarInvitationRefusal("this cancellation is for an event that isn't in your calendar")
		}
		endpoint := strings.TrimRight(source.RemoteID, "/") + "/" + uuid.NewString() + ".ics"
		if _, err := calDAVResponseEndpoint(source, endpoint); err != nil {
			return "", err
		}
		return endpoint, put(endpoint, calendarInvitationResource(invitation), "")
	}
	resource := existing
	if existing.SeriesRemoteID != "" {
		resource = storage.CalendarEvent{RemoteID: existing.SeriesRemoteID}
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, resource)
	if err != nil {
		return "", err
	}
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
	if err != nil {
		return "", err
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) {
		return "", calendarUpdateUnsupported("Refresh the calendar to retrieve a safe event version.")
	}
	// Only the invitation's own event, from the same organizer, is changed.
	uid, _ := invitation.calendar.Events()[0].Props.Text("UID")
	for _, event := range current.Events() {
		value, _ := event.Props.Text("UID")
		organizer := event.Props.Get("ORGANIZER")
		if value != uid || organizer == nil || calendarReplyAddress(organizer.Value) != invitation.organizer {
			return "", calendarInvitationRefusal("the event in your calendar isn't this invitation's")
		}
	}
	// An older message can't undo a newer one.
	for _, event := range invitation.calendar.Events() {
		key := calendarMeetingDateKey(event.Component)
		saved := calendarMeetingComponent(current, key)
		if saved == nil {
			saved = calendarMeetingMaster(current)
		}
		if saved == nil {
			continue
		}
		newer, _ := calendarMeetingComponentSequence(event.Component, false)
		older, err := calendarMeetingComponentSequence(saved, false)
		a, _ := strconv.Atoi(newer)
		b, _ := strconv.Atoi(older)
		if err != nil || a < b {
			return "", calendarInvitationRefusal("your calendar already has a newer version of this event")
		}
	}
	whole := calendarMeetingMaster(invitation.calendar) != nil
	if invitation.method == "CANCEL" && whole {
		req, err := newCardDAVRequest(ctx, http.MethodDelete, endpoint, credentials.username, credentials.password, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("If-Match", etag)
		return endpoint, calendarDeleteHTTP(client, req, true)
	}
	updated := &ical.Calendar{Component: cloneCalendarComponent(current.Component)}
	if invitation.method == "REQUEST" && whole {
		// A new version of the whole event or series replaces it.
		updated = calendarInvitationResource(invitation)
	} else {
		master := calendarMeetingMaster(updated)
		for _, event := range calendarInvitationResource(invitation).Events() {
			key := calendarMeetingDateKey(event.Component)
			children := updated.Children[:0]
			for _, child := range updated.Children {
				if child.Name != ical.CompEvent || calendarMeetingDateKey(child) != key {
					children = append(children, child)
				}
			}
			updated.Children = children
			if invitation.method == "REQUEST" {
				updated.Children = append(updated.Children, cloneCalendarComponent(event.Component))
			} else if master != nil && !calendarMeetingExcluded(master, key) {
				exdate := *event.Props.Get("RECURRENCE-ID")
				exdate.Name = "EXDATE"
				exdate.Params = cloneCalendarParams(exdate.Params)
				master.Props.Add(&exdate)
			}
		}
	}
	return endpoint, put(endpoint, updated, etag)
}

// mailCalendarInvitationOffer says what an email's card can do with its
// invitation in a CalDAV calendar of the invited account: add it, update the
// event it is for, or remove that event. Nil when the event is in another
// kind of calendar, which handles invitations itself, or is the account's own.
// A forwarded invitation can only be added.
func mailCalendarInvitationOffer(item mailCalendarEvent, candidates []storage.CalendarEvent, sources []storage.CalendarSource, accountID, account string) *views.MailCalendarInvitation {
	if item.Organizer == "" || strings.EqualFold(item.Organizer, account) || (item.Method != "REQUEST" && item.Method != "CANCEL") {
		return nil
	}
	var writable []storage.CalendarSource
	for _, source := range sources {
		if source.AccountID == accountID && storage.CalendarSourceMailServer(source.Provider) && calendarSourceWritable(source) {
			writable = append(writable, source)
		}
	}
	var existing []storage.CalendarEvent
	for _, event := range candidates {
		if event.IsDeleted {
			continue
		}
		if !storage.CalendarSourceMailServer(event.SourceProvider) || calendarCachedOrganizer(event) {
			return nil
		}
		existing = append(existing, event)
	}
	if len(writable) == 0 {
		return nil
	}
	forwarded := item.From != "" && !strings.EqualFold(item.From, item.Organizer)
	switch {
	case item.Method == "REQUEST" && item.Invited && !item.Cancelled && len(existing) == 0:
		note := "Add it to " + writable[0].Name + " to see it there and answer it."
		if forwarded {
			note = "Forwarded by " + item.From + ". " + note
		}
		return &views.MailCalendarInvitation{Label: "Add to calendar", Note: note, UID: item.UID, SourceID: writable[0].ID}
	case forwarded:
		return nil
	case item.Method == "REQUEST" && item.Invited && !item.Cancelled && mailCalendarInvitationChanged(item, existing):
		return &views.MailCalendarInvitation{Label: "Update in calendar", Note: "This invitation has changes that aren't in your calendar yet.", UID: item.UID}
	case item.Method == "CANCEL" && len(existing) > 0:
		return &views.MailCalendarInvitation{Label: "Remove from calendar", Note: "The organizer cancelled this event.", UID: item.UID}
	}
	return nil
}

// mailCalendarInvitationChanged reports whether an invitation is newer than,
// and differs from, the cached event or the date of a series it is for. An
// older email, or one whose age can't be told, never replaces the calendar.
func mailCalendarInvitationChanged(item mailCalendarEvent, existing []storage.CalendarEvent) bool {
	event := existing[0]
	if item.Modified == nil {
		return false
	}
	for _, candidate := range existing {
		if candidate.ProviderUpdatedAt == nil || !item.Modified.After(*candidate.ProviderUpdatedAt) {
			return false
		}
	}
	if item.Recurrence != nil {
		found := false
		for _, candidate := range existing {
			if candidate.StartAt != nil && candidate.StartAt.Equal(*item.Recurrence) || candidate.AllDay && candidate.StartDate == item.Recurrence.Format("2006-01-02") {
				event, found = candidate, true
			}
		}
		if !found {
			return true
		}
	}
	if strings.TrimSpace(item.Summary) != event.Summary || strings.TrimSpace(item.Location) != event.Location {
		return true
	}
	if item.Recurring && item.Recurrence == nil {
		return false // A series' dates aren't compared one by one.
	}
	if item.AllDay {
		return item.StartDate != event.StartDate || item.EndDate != event.EndDate
	}
	return item.Start == nil || item.End == nil || event.StartAt == nil || event.EndAt == nil || !item.Start.Equal(*event.StartAt) || !item.End.Equal(*event.EndAt)
}

// mailCalendarInvitationTarget picks the calendar and cached event an
// invitation goes to: the event's own calendar when it's already there.
func mailCalendarInvitationTarget(candidates []storage.CalendarEvent, sources []storage.CalendarSource, accountID, sourceID string) (storage.CalendarSource, storage.CalendarEvent, error) {
	var existing storage.CalendarEvent
	for _, event := range candidates {
		if event.IsDeleted {
			continue
		}
		if !storage.CalendarSourceMailServer(event.SourceProvider) || calendarCachedOrganizer(event) {
			return storage.CalendarSource{}, existing, calendarInvitationRefusal("this event isn't one Gofer can add from email")
		}
		resource := event.SeriesRemoteID
		if resource == "" {
			resource = event.RemoteID
		}
		if existing.ID != "" {
			previous := existing.SeriesRemoteID
			if previous == "" {
				previous = existing.RemoteID
			}
			if previous != resource || existing.SourceID != event.SourceID {
				return storage.CalendarSource{}, existing, calendarInvitationRefusal("this event is in more than one calendar")
			}
			continue
		}
		existing = event
	}
	if existing.ID != "" {
		sourceID = existing.SourceID
	}
	for _, source := range sources {
		if source.ID == sourceID && source.AccountID == accountID && storage.CalendarSourceMailServer(source.Provider) && calendarSourceWritable(source) {
			return source, existing, nil
		}
	}
	return storage.CalendarSource{}, existing, calendarInvitationRefusal("choose a writable calendar of this account")
}
