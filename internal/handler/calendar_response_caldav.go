package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

type calDAVResponseTarget struct {
	Calendar                        *ical.Calendar
	Selected                        *ical.Component
	Username, Password, ScheduleTag string
	ServerScheduling                bool
}

// Calendar addresses are URIs, not display-name mailbox strings or arbitrary
// URLs. Email fallback never derives a sending identity from a DAV username.
func calendarReplyAddress(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "mailto") || u.RawQuery != "" || u.Fragment != "" || u.Host != "" || u.Opaque == "" {
		return ""
	}
	a, err := mail.ParseAddress(u.Opaque)
	if err != nil || a.Name != "" || a.Address != u.Opaque || strings.ContainsAny(a.Address, "\r\n") {
		return ""
	}
	return strings.ToLower(a.Address)
}

// OPTIONS advertises RFC 6638; the authenticated principal supplies the
// identities on whose behalf the server can schedule. All discovery is
// same-origin HTTPS, with redirects refused for the capability request.
func calDAVResponseCapabilities(ctx context.Context, source storage.CalendarSource, username, password string) (bool, []string, error) {
	req, err := newCardDAVRequest(ctx, "OPTIONS", source.RemoteID, username, password, nil)
	if err != nil {
		return false, nil, err
	}
	res, err := calendarProviderDo(calendarDeleteClient(calDAVHTTPTransport), req)
	if err != nil {
		return false, nil, err
	}
	res.Body.Close()
	if res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotImplemented {
		return false, nil, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, nil, calendarCreateProviderError{res.StatusCode}
	}
	auto := false
	for _, line := range res.Header.Values("DAV") {
		for _, value := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(value), "calendar-auto-schedule") {
				auto = true
			}
		}
	}
	if !auto {
		return false, nil, nil
	}
	principalResult, err := calDAVRequest(ctx, "PROPFIND", source.RemoteID, username, password, "0", `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`, 30*time.Second)
	if err != nil {
		return false, nil, err
	}
	if len(principalResult.Responses) != 1 {
		return false, nil, fmt.Errorf("ambiguous calendar principal")
	}
	pr := principalResult.Responses[0]
	if pr.Status != "" && !strings.Contains(pr.Status, " 200 ") {
		return false, nil, fmt.Errorf("calendar principal unavailable")
	}
	resolvedPrincipalResource, err := resolveCalDAVHref(source.RemoteID, pr.Href)
	if err != nil || resolvedPrincipalResource != source.RemoteID {
		return false, nil, fmt.Errorf("unexpected calendar principal resource")
	}
	principal := ""
	for _, stat := range pr.PropStats {
		if strings.Contains(stat.Status, " 404 ") {
			continue
		}
		if !strings.Contains(stat.Status, " 200 ") {
			return false, nil, fmt.Errorf("calendar principal unavailable")
		}
		if principal != "" && principal != stat.Prop.CurrentUserPrincipal.Href {
			return false, nil, fmt.Errorf("ambiguous calendar principal")
		}
		principal = strings.TrimSpace(stat.Prop.CurrentUserPrincipal.Href)
	}
	if principal == "" {
		return false, nil, nil
	}
	endpoint, err := resolveCalDAVHref(source.RemoteID, principal)
	if err != nil {
		return false, nil, err
	}
	multi, err := calDAVRequest(ctx, "PROPFIND", endpoint, username, password, "0", `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-user-address-set/></d:prop></d:propfind>`, 30*time.Second)
	if err != nil {
		return false, nil, err
	}
	if len(multi.Responses) != 1 {
		return false, nil, fmt.Errorf("ambiguous calendar principal")
	}
	r := multi.Responses[0]
	if r.Status != "" && !strings.Contains(r.Status, " 200 ") {
		return false, nil, fmt.Errorf("calendar identity unavailable")
	}
	resolved, err := resolveCalDAVHref(endpoint, r.Href)
	if err != nil || resolved != endpoint {
		return false, nil, fmt.Errorf("unexpected calendar principal")
	}
	var addresses []string
	for _, stat := range r.PropStats {
		if strings.Contains(stat.Status, " 404 ") {
			continue
		}
		if !strings.Contains(stat.Status, " 200 ") {
			return false, nil, fmt.Errorf("calendar identity unavailable")
		}
		for _, href := range stat.Prop.CalendarUserAddresses.Hrefs {
			if address := calendarReplyAddress(href); address != "" {
				addresses = append(addresses, address)
			}
		}
	}
	return len(addresses) > 0, addresses, nil
}

func calDAVResponseSelf(event *ical.Component, identities []string) (*ical.Prop, string, error) {
	if len(event.Props["ORGANIZER"]) != 1 {
		return nil, "", errCalendarNotInvitation
	}
	organizer := calendarReplyAddress(event.Props.Get("ORGANIZER").Value)
	if organizer == "" {
		return nil, "", errCalendarNotInvitation
	}
	var self *ical.Prop
	for _, attendee := range event.Props["ATTENDEE"] {
		address := calendarReplyAddress(attendee.Value)
		match := false
		for _, identity := range identities {
			if address != "" && address == identity {
				match = true
			}
		}
		if !match {
			continue
		}
		if self != nil || address == organizer || attendee.Params.Get("SENT-BY") != "" || attendee.Params.Get("DELEGATED-TO") != "" || attendee.Params.Get("DELEGATED-FROM") != "" {
			return nil, "", errCalendarNotInvitation
		}
		copy := attendee
		self = &copy
	}
	if self == nil {
		return nil, "", errCalendarNotInvitation
	}
	status := strings.ToLower(self.Params.Get("PARTSTAT"))
	if status == "" || status == "needs-action" {
		status = "needsAction"
	}
	if calendarResponseStatus(status) == "" {
		return nil, "", errCalendarNotInvitation
	}
	return self, status, nil
}

func readCalDAVResponseTarget(ctx context.Context, source storage.CalendarSource, cached storage.CalendarEvent, scope, username, password, mailbox string) (calendarResponseTarget, error) {
	target := calendarResponseTarget{Scope: scope}
	resource := cached.RemoteID
	if cached.SeriesRemoteID != "" {
		resource = cached.SeriesRemoteID
	}
	endpoint, err := calDAVResponseEndpoint(source, resource)
	if err != nil {
		return target, err
	}
	cal, headers, err := calendarUpdateCalDAVGet(ctx, calendarDeleteClient(calDAVHTTPTransport), endpoint, username, password)
	if err != nil {
		return target, err
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) || etag != cached.ETag || cal.Props.Get("METHOD") != nil || cached.ICalUID == "" {
		return target, errCalendarUpdateConflict
	}
	var master, selected *ical.Component
	seen := map[string]bool{}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			continue
		}
		uid, err := child.Props.Text("UID")
		if child.Name != ical.CompEvent || err != nil || uid != cached.ICalUID || len(child.Props["UID"]) != 1 || len(child.Props["DTSTART"]) != 1 || len(child.Props["SEQUENCE"]) > 1 {
			return target, errCalendarUpdateConflict
		}
		if seq := child.Props.Get("SEQUENCE"); seq != nil {
			n, err := strconv.Atoi(seq.Value)
			if err != nil || n < 0 {
				return target, errCalendarUpdateConflict
			}
		}
		key := "master"
		if rid := child.Props.Get("RECURRENCE-ID"); rid != nil {
			key, err = calendarRecurrenceKey(rid, time.UTC)
			if err != nil || len(child.Props["RECURRENCE-ID"]) != 1 || child.Props.Get("RRULE") != nil || child.Props.Get("RDATE") != nil {
				return target, errCalendarUpdateConflict
			}
			if endpoint+"#recurrence="+url.QueryEscape(key) == cached.RemoteID {
				selected = child
			}
		} else {
			master = child
		}
		if seen[key] {
			return target, errCalendarUpdateConflict
		}
		seen[key] = true
	}
	if master == nil {
		return target, errCalendarUpdateConflict
	}
	recurring := master.Props.Get("RRULE") != nil || master.Props.Get("RDATE") != nil
	if (scope == "event" && (recurring || len(seen) != 1)) || (scope != "event" && !recurring) {
		return target, errCalendarUpdateConflict
	}
	if cached.SeriesRemoteID != "" {
		expanded, err := calendarCalDAVReadOccurrence(ctx, source, username, password, cached)
		if err != nil {
			return target, err
		}
		target.OccurrenceVersion = etag
		if scope == "occurrence" && selected == nil {
			// Use the provider's expanded timing, retaining alarms and private
			// metadata from the master. Never reuse the master's original dates.
			selected = cloneCalendarComponent(master)
			for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE", "DTSTART", "DTEND", "DURATION", "RECURRENCE-ID"} {
				delete(selected.Props, name)
			}
			for _, name := range []string{"DTSTART", "DTEND", "DURATION", "RECURRENCE-ID"} {
				selected.Props[name] = expanded.Props[name]
			}
			if selected.Props.Get("RECURRENCE-ID") == nil {
				rid := *expanded.Props.Get("DTSTART")
				rid.Name = "RECURRENCE-ID"
				selected.Props.Set(&rid)
			}
			key, err := calendarRecurrenceKey(selected.Props.Get("RECURRENCE-ID"), time.UTC)
			if err != nil || endpoint+"#recurrence="+url.QueryEscape(key) != cached.RemoteID {
				return target, errCalendarUpdateConflict
			}
			cal.Children = append(cal.Children, selected)
		}
	}
	if scope != "occurrence" {
		selected = master
	}
	if selected == nil {
		return target, errCalendarUpdateConflict
	}
	selectedStatus, _ := selected.Props.Text("STATUS")
	if strings.EqualFold(selectedStatus, "CANCELLED") {
		return target, errCalendarUpdateConflict
	}
	auto, identities, err := calDAVResponseCapabilities(ctx, source, username, password)
	if err != nil {
		return target, err
	}
	if !auto {
		identities = []string{strings.ToLower(mailbox)}
	}
	self, status, err := calDAVResponseSelf(selected, identities)
	if err != nil {
		return target, err
	}
	selfEmail := calendarReplyAddress(self.Value)
	organizer := calendarReplyAddress(selected.Props.Get("ORGANIZER").Value)
	// All affected instances must agree about organizer and attendee identity.
	for _, child := range cal.Events() {
		if scope != "series" && child.Component != selected {
			continue
		}
		if calDAVReplyCanceled(child.Component) {
			continue
		}
		_, partstat, err := calDAVResponseSelf(child.Component, []string{selfEmail})
		if err != nil || calendarReplyAddress(child.Props.Get("ORGANIZER").Value) != organizer {
			return target, errCalendarNotInvitation
		}
		if partstat != status && scope == "series" {
			status = "needsAction"
		}
		switch strings.ToUpper(child.Props.Get("ORGANIZER").Params.Get("SCHEDULE-AGENT")) {
		case "", "SERVER":
		case "CLIENT":
			auto = false
		default:
			return target, fmt.Errorf("this invitation disables scheduling replies")
		}
	}
	if !auto && (mailbox == "" || !strings.EqualFold(mailbox, selfEmail)) {
		return target, calendarReplyConfigurationError("Email replies require this account's sending address to match the invited attendee.")
	}
	remote, err := calDAVResponseEvent(selected, endpoint, etag)
	if err != nil {
		return target, err
	}
	remote.ResponseStatus = status
	if scope == "occurrence" && remote.RemoteID != cached.RemoteID {
		return target, errCalendarUpdateConflict
	}
	target.Event, target.Endpoint, target.HTTPETag, target.SelfEmail = remote, endpoint, etag, selfEmail
	target.CalDAV = &calDAVResponseTarget{Calendar: cal, Selected: selected, Username: username, Password: password, ScheduleTag: headers.Get("Schedule-Tag"), ServerScheduling: auto}
	if target.CalDAV.ScheduleTag != "" && !calendarUpdateValidETag(target.CalDAV.ScheduleTag, false) {
		return calendarResponseTarget{}, errCalendarUpdateConflict
	}
	return target, nil
}

func calDAVResponseEndpoint(source storage.CalendarSource, resource string) (string, error) {
	endpoint, err := calendarUpdateCalDAVEndpoint(source, storage.CalendarEvent{RemoteID: resource})
	if err != nil {
		return "", err
	}
	collection, _ := url.Parse(source.RemoteID)
	u, _ := url.Parse(endpoint)
	if !strings.HasPrefix(u.Path, strings.TrimSuffix(collection.Path, "/")+"/") || strings.HasSuffix(u.Path, "/") || path.Clean(u.Path) != u.Path {
		return "", errCalendarUpdateConflict
	}
	return endpoint, nil
}

func encodeCalendarReply(cal *ical.Calendar) (string, error) {
	var buffer bytes.Buffer
	err := ical.NewEncoder(&buffer).Encode(cal)
	return buffer.String(), err
}

// Keep the stored resource intact. The emailed REPLY deliberately contains only
// the replying attendee and the protocol fields, never other guests or alarms.
func prepareCalDAVResponse(target calendarResponseTarget, response string) (*ical.Calendar, string, error) {
	cal := &ical.Calendar{Component: cloneCalendarComponent(target.CalDAV.Calendar.Component)}
	reply := ical.NewCalendar()
	reply.Props.SetText("VERSION", "2.0")
	reply.Props.SetText("PRODID", "-//Gofer//Calendar//EN")
	reply.Props.SetText("METHOD", "REPLY")
	zones := make(map[string]bool)
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			if id := child.Props.Get("TZID"); id != nil {
				zones[id.Value] = true
			}
		}
	}
	for i, child := range target.CalDAV.Calendar.Children {
		if child.Name == ical.CompTimezone {
			reply.Children = append(reply.Children, cloneCalendarComponent(child))
			continue
		}
		if target.Scope != "series" && child != target.CalDAV.Selected {
			continue
		}
		if calDAVReplyCanceled(child) {
			continue
		}
		stored := cal.Children[i]
		attendees := stored.Props["ATTENDEE"]
		var replying ical.Prop
		for j := range attendees {
			if calendarReplyAddress(attendees[j].Value) != target.SelfEmail {
				continue
			}
			attendees[j].Params.Set("PARTSTAT", strings.ToUpper(response))
			attendees[j].Params.Set("RSVP", "FALSE")
			replying = attendees[j]
		}
		organizer := stored.Props.Get("ORGANIZER")
		delete(organizer.Params, "SCHEDULE-STATUS")
		if !target.CalDAV.ServerScheduling {
			organizer.Params.Set("SCHEDULE-AGENT", "CLIENT")
		}
		stored.Props.SetDateTime("DTSTAMP", time.Now().UTC())
		one := ical.NewComponent(ical.CompEvent)
		for _, name := range []string{"UID", "SEQUENCE", "DTSTAMP", "RECURRENCE-ID", "DTSTART", "DTEND", "DURATION", "SUMMARY"} {
			one.Props[name] = stored.Props[name]
		}
		one = cloneCalendarComponent(one)
		// Some servers use timezone-by-reference and omit VTIMEZONE. iMIP
		// needs self-contained dates: use equivalent UTC instants in that
		// case, while leaving the original stored resource untouched.
		for _, name := range []string{"DTSTART", "DTEND", "RECURRENCE-ID"} {
			prop := one.Props.Get(name)
			if prop == nil || prop.Params.Get("TZID") == "" || zones[prop.Params.Get("TZID")] {
				continue
			}
			at, err := prop.DateTime(time.UTC)
			if err != nil {
				return nil, "", err
			}
			prop.SetDateTime(at.UTC())
			delete(prop.Params, "TZID")
		}
		person := ical.NewProp("ATTENDEE")
		person.Value = replying.Value
		person.Params.Set("PARTSTAT", strings.ToUpper(response))
		one.Props.Set(person)
		org := ical.NewProp("ORGANIZER")
		org.Value = organizer.Value
		one.Props.Set(org)
		reply.Children = append(reply.Children, one)
	}
	body, err := encodeCalendarReply(reply)
	return cal, body, err
}

func calDAVResponseResourceMatches(expected, actual *ical.Calendar) bool {
	strip := func(cal *ical.Calendar) *ical.Calendar {
		copy := &ical.Calendar{Component: cloneCalendarComponent(cal.Component)}
		for _, event := range copy.Events() {
			for name, props := range event.Props {
				for i := range props {
					delete(props[i].Params, "SCHEDULE-STATUS")
				}
				event.Props[name] = props
			}
		}
		return copy
	}
	return calendarOccurrenceResourceMatches(strip(expected), strip(actual))
}

// Never fall back to email after starting this write: a timeout may mean the
// server has already notified the organizer. The durable reservation stays put.
func putCalDAVResponse(ctx context.Context, target calendarResponseTarget, desired *ical.Calendar, response string) (calendarResponseResult, error) {
	body, err := encodeCalendarReply(desired)
	if err != nil {
		return calendarResponseResult{}, calendarDeletePreflightError{err}
	}
	req, err := newCardDAVRequest(ctx, http.MethodPut, target.Endpoint, target.CalDAV.Username, target.CalDAV.Password, strings.NewReader(body))
	if err != nil {
		return calendarResponseResult{}, calendarDeletePreflightError{err}
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	// The full representation must not overwrite concurrent attendee changes.
	// Use ETag even with a scheduling tag; stale resources are re-read by users.
	req.Header.Set("If-Match", target.HTTPETag)
	if target.CalDAV.ScheduleTag != "" {
		req.Header.Set("If-Schedule-Tag-Match", target.CalDAV.ScheduleTag)
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	res, err := calendarProviderDo(client, req)
	if err != nil {
		return calendarResponseResult{}, err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return calendarResponseResult{}, calendarUpdateHTTPError(calendarCreateProviderError{res.StatusCode})
		}
		return calendarResponseResult{}, fmt.Errorf("unexpected calendar response status")
	}
	saved, headers, err := calendarUpdateCalDAVGet(ctx, client, target.Endpoint, target.CalDAV.Username, target.CalDAV.Password)
	if err != nil || !calendarUpdateValidETag(headers.Get("ETag"), false) || !calDAVResponseResourceMatches(desired, saved) {
		return calendarResponseResult{}, fmt.Errorf("could not confirm the calendar response")
	}
	result := calendarResponseResult{Event: target.Event}
	result.Event.ETag, result.Event.ResponseStatus = headers.Get("ETag"), response
	for _, event := range saved.Events() {
		if calDAVReplyCanceled(event.Component) {
			continue
		}
		remote, err := calDAVResponseEvent(event.Component, target.Endpoint, headers.Get("ETag"))
		if err != nil {
			return calendarResponseResult{}, err
		}
		if target.Scope != "series" && remote.RemoteID != target.Event.RemoteID {
			continue
		}
		if target.Scope != "series" || remote.SeriesRemoteID == "" {
			remote.ResponseStatus = response
			result.Event = remote
		}
		if target.CalDAV.ServerScheduling {
			status := event.Props.Get("ORGANIZER").Params.Get("SCHEDULE-STATUS")
			for _, code := range strings.Split(status, ",") {
				code = strings.TrimSpace(code)
				if strings.HasPrefix(code, "3.") || strings.HasPrefix(code, "4.") || strings.HasPrefix(code, "5.") {
					return calendarResponseResult{}, fmt.Errorf("calendar saved, but the server could not deliver the reply; check your provider before retrying")
				}
				if code != "1.1" && code != "1.2" && !strings.HasPrefix(code, "2.") {
					result.Pending = true
				}
			}
		}
	}
	return result, nil
}

func calDAVReplyCanceled(component *ical.Component) bool {
	status, _ := component.Props.Text("STATUS")
	return strings.EqualFold(status, "CANCELLED")
}

// The sync normalizer intentionally rejects unexpanded recurrences. A response
// preflight reads full resources, so normalize a copy without changing the
// stored recurrence or loosening the sync boundary.
func calDAVResponseEvent(component *ical.Component, endpoint, etag string) (calendar.RemoteEvent, error) {
	copy := cloneCalendarComponent(component)
	var rules []string
	for _, name := range []string{"RRULE", "EXRULE", "RDATE", "EXDATE"} {
		for _, prop := range copy.Props[name] {
			rules = append(rules, name+":"+prop.Value)
		}
		delete(copy.Props, name)
	}
	event, err := normalizeCalDAVEvent(ical.Event{Component: copy}, endpoint, etag, time.UTC)
	if len(rules) > 0 {
		event.Recurrence, _ = json.Marshal(rules)
	}
	return event, err
}
