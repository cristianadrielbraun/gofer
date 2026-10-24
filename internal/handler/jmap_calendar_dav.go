package handler

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// A JMAP calendar is written through the CalDAV code: its calendar and event
// ids are kept as CalDAV-style resource URLs on a host that never resolves,
// and jmapCalendarDAV answers the CalDAV requests for them over JMAP. An
// event is its iCalendar form there, and its version a hash of its JMAP
// form, which sync stores too.
const jmapCalendarDAVHost = "jmap.invalid"

const jmapCalendarDAVOrigin = "https://" + jmapCalendarDAVHost + "/"

// jmapCalendarDAVCollection is the resource URL of a JMAP calendar.
func jmapCalendarDAVCollection(calendarID string) string {
	return jmapCalendarDAVOrigin + url.PathEscape(calendarID) + "/"
}

// jmapCalendarDAVResource is the resource URL of an event in a calendar.
func jmapCalendarDAVResource(collection, eventID string) string {
	return collection + url.PathEscape(eventID) + ".ics"
}

// jmapCalendarDAVParse reads a calendar's id, and an event resource's name
// ("" for the calendar itself), from a resource URL.
func jmapCalendarDAVParse(raw string) (calendarID, name string, ok bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != jmapCalendarDAVHost || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	segments := strings.Split(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
	if len(segments) != 2 {
		return "", "", false
	}
	calendarID, err = url.PathUnescape(segments[0])
	if err != nil || calendarID == "" {
		return "", "", false
	}
	if segments[1] == "" {
		return calendarID, "", true
	}
	escaped, isEvent := strings.CutSuffix(segments[1], ".ics")
	if name, err = url.PathUnescape(escaped); err != nil || !isEvent || name == "" {
		return "", "", false
	}
	return calendarID, name, true
}

type jmapCalendarDAVKey struct{}

// jmapCalendarStore is what jmapCalendarDAV needs of a JMAP session; see
// mail.JMAPCalendarConnection.
type jmapCalendarStore interface {
	Event(ctx context.Context, id string) (jmap.CalendarEvent, error)
	ByUID(ctx context.Context, calendarID, uid string) (jmap.CalendarEvent, error)
	FromICS(ctx context.Context, data []byte) ([]jmap.CalendarEvent, error)
	Create(ctx context.Context, calendarID string, event jmap.CalendarEvent) (string, error)
	Update(ctx context.Context, id string, patch jmap.PatchObject) (bool, error)
	Delete(ctx context.Context, id string) (bool, error)
}

// jmapCalendarDAV answers CalDAV requests for one JMAP calendar during one
// write. Events it creates keep the resource name CalDAV gave them until
// the write is done.
type jmapCalendarDAV struct {
	cfg        *models.AccountConfig
	password   string
	calendarID string

	mu         sync.Mutex
	connection jmapCalendarStore
	created    map[string]string
	written    map[string]jmapCalendarDAVWrite
}

// jmapCalendarDAVWrite is the iCalendar an event was saved from, at the
// version saving it made, once the server was seen to keep every change.
type jmapCalendarDAVWrite struct {
	etag string
	data []byte
}

// withJMAPCalendarDAV lets the CalDAV code reach a JMAP calendar's events.
// Other calendars keep their context.
func withJMAPCalendarDAV(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) context.Context {
	if source.Provider != storage.CalendarSourceProviderJMAP || credentials.jmap == nil {
		return ctx
	}
	calendarID, name, ok := jmapCalendarDAVParse(source.RemoteID)
	if !ok || name != "" {
		return ctx
	}
	return context.WithValue(ctx, jmapCalendarDAVKey{}, &jmapCalendarDAV{cfg: credentials.jmap, password: credentials.password, calendarID: calendarID, created: map[string]string{}, written: map[string]jmapCalendarDAVWrite{}})
}

// jmapCalendarDAVFor is the JMAP calendar a request is for, nil when it goes
// to a real CalDAV server.
func jmapCalendarDAVFor(req *http.Request) *jmapCalendarDAV {
	if req.URL == nil || req.URL.Host != jmapCalendarDAVHost {
		return nil
	}
	dav, _ := req.Context().Value(jmapCalendarDAVKey{}).(*jmapCalendarDAV)
	return dav
}

// jmapCalendarDAVMissing answers a request for a JMAP calendar made without
// its connection, which is a bug rather than something to send anywhere.
func jmapCalendarDAVMissing(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("JMAP calendar request %s %s has no JMAP connection", req.Method, req.URL.Redacted())
}

func (d *jmapCalendarDAV) open(ctx context.Context) (jmapCalendarStore, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connection == nil {
		connection, err := mail.OpenJMAPCalendars(ctx, d.cfg, d.password)
		if err != nil {
			return nil, err
		}
		d.connection = connection
	}
	return d.connection, nil
}

// eventID is the JMAP id of the event a resource name stands for.
func (d *jmapCalendarDAV) eventID(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.created[name]; ok {
		return id
	}
	return name
}

// canonical gives a resource URL its event's JMAP id, for an event created
// under the name CalDAV chose.
func (d *jmapCalendarDAV) canonical(raw string) string {
	base, fragment, _ := strings.Cut(raw, "#")
	calendarID, name, ok := jmapCalendarDAVParse(base)
	if !ok || name == "" || calendarID != d.calendarID {
		return raw
	}
	resolved := jmapCalendarDAVResource(jmapCalendarDAVCollection(calendarID), d.eventID(name))
	if fragment != "" {
		resolved += "#" + fragment
	}
	return resolved
}

// roundTrip answers GET, PUT and DELETE on an event resource.
func (d *jmapCalendarDAV) roundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	calendarID, name, ok := jmapCalendarDAVParse(req.URL.String())
	if !ok || calendarID != d.calendarID {
		return nil, errors.New("calendar request is outside the selected JMAP calendar")
	}
	if name == "" {
		if req.Method == "REPORT" {
			return d.report(req)
		}
		return jmapCalendarDAVStatus(req, http.StatusMethodNotAllowed, ""), nil
	}
	connection, err := d.open(ctx)
	if err != nil {
		return nil, err
	}
	id := d.eventID(name)
	current, err := connection.Event(ctx, id)
	if err != nil {
		return nil, err
	}
	if current == nil && req.Method == http.MethodPut && req.Header.Get("If-None-Match") == "*" {
		// A create tried again after its reply was lost finds the event it
		// made by its UID, as CalDAV finds it by its resource name.
		if current, err = d.createdBefore(req, connection); err != nil {
			return nil, err
		}
		if current != nil {
			id = current.ID()
			d.mu.Lock()
			d.created[name] = id
			d.mu.Unlock()
		}
	}
	if current != nil && !jmapCalendarEventIn(current, calendarID) {
		current = nil
	}
	etag := ""
	if current != nil {
		etag = jmapCalendarEventETag(current)
	}
	switch req.Method {
	case http.MethodGet:
		if current == nil {
			return jmapCalendarDAVStatus(req, http.StatusNotFound, ""), nil
		}
		// An event this write saved reads back as it was written, as a
		// CalDAV server stores a resource, when the server kept every
		// change; the CalDAV code checks its write against that.
		d.mu.Lock()
		written, saved := d.written[id]
		d.mu.Unlock()
		data := written.data
		if !saved || written.etag != etag {
			if data, err = mail.JMAPEventICS(current); err != nil {
				return nil, err
			}
		}
		response := jmapCalendarDAVStatus(req, http.StatusOK, etag)
		response.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		response.Body, response.ContentLength = io.NopCloser(bytes.NewReader(data)), int64(len(data))
		return response, nil
	case http.MethodDelete:
		if current == nil {
			return jmapCalendarDAVStatus(req, http.StatusNotFound, ""), nil
		}
		if match := req.Header.Get("If-Match"); match != "" && match != etag {
			return jmapCalendarDAVStatus(req, http.StatusPreconditionFailed, ""), nil
		}
		if deleted, err := connection.Delete(ctx, id); err != nil || !deleted {
			if err == nil {
				return jmapCalendarDAVStatus(req, http.StatusNotFound, ""), nil
			}
			return nil, err
		}
		return jmapCalendarDAVStatus(req, http.StatusNoContent, ""), nil
	case http.MethodPut:
		return d.put(req, connection, name, id, current, etag)
	}
	return jmapCalendarDAVStatus(req, http.StatusMethodNotAllowed, ""), nil
}

// put saves an event's iCalendar form: a new event, or the changes to one
// that is there, as the properties that differ from what it was.
func (d *jmapCalendarDAV) put(req *http.Request, connection jmapCalendarStore, name, id string, current jmap.CalendarEvent, etag string) (*http.Response, error) {
	ctx := req.Context()
	if req.Header.Get("If-None-Match") == "*" && current != nil {
		return jmapCalendarDAVStatus(req, http.StatusPreconditionFailed, ""), nil
	}
	if match := req.Header.Get("If-Match"); match != "" && (current == nil || match != etag) {
		return jmapCalendarDAVStatus(req, http.StatusPreconditionFailed, ""), nil
	}
	if req.Body == nil {
		return jmapCalendarDAVStatus(req, http.StatusBadRequest, ""), nil
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, calDAVResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > calDAVResponseLimit {
		return jmapCalendarDAVStatus(req, http.StatusRequestEntityTooLarge, ""), nil
	}
	wanted, err := connection.FromICS(ctx, data)
	if err != nil {
		return nil, err
	}
	if len(wanted) != 1 {
		return jmapCalendarDAVStatus(req, http.StatusBadRequest, ""), nil
	}
	status := http.StatusNoContent
	var patch jmap.PatchObject
	if current == nil {
		id, err = connection.Create(ctx, d.calendarID, wanted[0])
		if err != nil {
			// A server refuses a second event with a UID it has; that one
			// is the resource, if its search finds it by now.
			if existing, findErr := connection.ByUID(ctx, d.calendarID, jscalendarUID(wanted[0])); findErr == nil && existing != nil {
				d.mu.Lock()
				d.created[name] = existing.ID()
				d.mu.Unlock()
				return jmapCalendarDAVStatus(req, http.StatusPreconditionFailed, ""), nil
			}
			return nil, err
		}
		d.mu.Lock()
		d.created[name] = id
		d.mu.Unlock()
		status = http.StatusCreated
	} else {
		if jscalendarUID(current) != jscalendarUID(wanted[0]) {
			return jmapCalendarDAVStatus(req, http.StatusBadRequest, ""), nil
		}
		// The event as it was is read back through the same conversion, so
		// only what the CalDAV code changed differs.
		before, err := mail.JMAPEventICS(current)
		if err != nil {
			return nil, err
		}
		baseline, err := connection.FromICS(ctx, before)
		if err != nil {
			return nil, err
		}
		if len(baseline) != 1 {
			return nil, errors.New("the JMAP server read the event as more than one")
		}
		patch = mail.JMAPEventPatch(baseline[0], wanted[0])
		if len(patch) > 0 {
			updated, err := connection.Update(ctx, id, patch)
			if err != nil {
				return nil, err
			}
			if !updated {
				return jmapCalendarDAVStatus(req, http.StatusPreconditionFailed, ""), nil
			}
		}
	}
	saved, err := connection.Event(ctx, id)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return jmapCalendarDAVStatus(req, http.StatusConflict, ""), nil
	}
	etag = jmapCalendarEventETag(saved)
	if jmapCalendarDAVKept(patch, saved) {
		d.mu.Lock()
		d.written[id] = jmapCalendarDAVWrite{etag: etag, data: data}
		d.mu.Unlock()
	}
	return jmapCalendarDAVStatus(req, status, etag), nil
}

// jmapCalendarDAVKept reports whether the server kept every property a patch
// set, as sent. The times things were updated are the server's to set, and
// the iCalendar details it keeps aside it need not send back in a changed
// occurrence.
func jmapCalendarDAVKept(patch jmap.PatchObject, saved jmap.CalendarEvent) bool {
	if patch == nil {
		return false
	}
	for key, value := range patch {
		if key == "updated" {
			continue
		}
		if stored, ok := saved[key]; value == nil && ok || value != nil && !reflect.DeepEqual(jmapCalendarDAVComparable(stored), jmapCalendarDAVComparable(value)) {
			return false
		}
	}
	return true
}

// report answers a calendar-multiget that expands events in a time range,
// as the CalDAV code reads one occurrence of a series.
func (d *jmapCalendarDAV) report(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return jmapCalendarDAVStatus(req, http.StatusBadRequest, ""), nil
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var query struct {
		XMLName xml.Name
		Expand  *struct {
			Start string `xml:"start,attr"`
			End   string `xml:"end,attr"`
		} `xml:"prop>calendar-data>expand"`
		Hrefs []string `xml:"href"`
	}
	if err := xml.Unmarshal(data, &query); err != nil || query.XMLName.Local != "calendar-multiget" || query.Expand == nil || len(query.Hrefs) == 0 {
		return jmapCalendarDAVStatus(req, http.StatusNotImplemented, ""), nil
	}
	start, startErr := time.Parse("20060102T150405Z", query.Expand.Start)
	end, endErr := time.Parse("20060102T150405Z", query.Expand.End)
	if startErr != nil || endErr != nil || !end.After(start) {
		return jmapCalendarDAVStatus(req, http.StatusBadRequest, ""), nil
	}
	connection, err := d.open(req.Context())
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
	for _, href := range query.Hrefs {
		body.WriteString("<d:response><d:href>")
		_ = xml.EscapeText(&body, []byte(href))
		body.WriteString("</d:href>")
		resource, err := req.URL.Parse(href)
		calendarID, name, ok := "", "", err == nil
		if ok {
			calendarID, name, ok = jmapCalendarDAVParse(resource.String())
		}
		var event jmap.CalendarEvent
		if ok && calendarID == d.calendarID && name != "" {
			if event, err = connection.Event(req.Context(), d.eventID(name)); err != nil {
				return nil, err
			}
		}
		if event == nil || !jmapCalendarEventIn(event, d.calendarID) {
			body.WriteString("<d:status>HTTP/1.1 404 Not Found</d:status></d:response>")
			continue
		}
		expanded, err := jmapCalendarExpand(event, resource.String(), start, end, time.UTC)
		if err != nil {
			return nil, err
		}
		cal := ical.NewCalendar()
		cal.Props.SetText(ical.PropProductID, "-//Gofer//JMAP Calendars//EN")
		cal.Props.SetText(ical.PropVersion, "2.0")
		for _, occurrence := range expanded {
			cal.Children = append(cal.Children, occurrence.component)
		}
		var ics bytes.Buffer
		if len(cal.Children) > 0 {
			if err := ical.NewEncoder(&ics).Encode(cal); err != nil {
				return nil, err
			}
		}
		body.WriteString("<d:propstat><d:prop><d:getetag>")
		_ = xml.EscapeText(&body, []byte(jmapCalendarEventETag(event)))
		body.WriteString("</d:getetag><c:calendar-data>")
		_ = xml.EscapeText(&body, ics.Bytes())
		body.WriteString("</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>")
	}
	body.WriteString("</d:multistatus>")
	response := jmapCalendarDAVStatus(req, http.StatusMultiStatus, "")
	response.Header.Set("Content-Type", "application/xml; charset=utf-8")
	response.Body, response.ContentLength = io.NopCloser(bytes.NewReader(body.Bytes())), int64(body.Len())
	return response, nil
}

// createdBefore is the event a create's iCalendar UID already names, if
// any. The request body is kept for the create itself.
func (d *jmapCalendarDAV) createdBefore(req *http.Request, connection jmapCalendarStore) (jmap.CalendarEvent, error) {
	if req.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, calDAVResponseLimit+1))
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	cal, err := calendarUpdateDecodeICS(data)
	if err != nil || len(cal.Events()) == 0 {
		return nil, nil
	}
	uid, _ := cal.Events()[0].Props.Text("UID")
	if uid == "" {
		return nil, nil
	}
	return connection.ByUID(req.Context(), d.calendarID, uid)
}

// jmapCalendarDAVComparable is a property value without the parts
// jmapCalendarDAVKept doesn't compare, at any depth.
func jmapCalendarDAVComparable(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			if key != "updated" && key != "iCalendar" {
				out[key] = jmapCalendarDAVComparable(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = jmapCalendarDAVComparable(item)
		}
		return out
	}
	return value
}

func jmapCalendarDAVStatus(req *http.Request, status int, etag string) *http.Response {
	response := &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: http.NoBody, Request: req,
	}
	if etag != "" {
		response.Header.Set("ETag", etag)
	}
	return response
}

// jmapCalendarEventIn reports whether an event is in a calendar.
func jmapCalendarEventIn(event jmap.CalendarEvent, calendarID string) bool {
	calendars, _ := event["calendarIds"].(map[string]any)
	in, _ := calendars[calendarID].(bool)
	return in
}

func jscalendarUID(event jmap.CalendarEvent) string {
	uid, _ := event["uid"].(string)
	return uid
}

// jmapCalendarDAVCanonical gives an event the CalDAV code created its JMAP
// id, in place of the resource name it was created under.
func jmapCalendarDAVCanonical(ctx context.Context, remote calendar.RemoteEvent) calendar.RemoteEvent {
	dav, _ := ctx.Value(jmapCalendarDAVKey{}).(*jmapCalendarDAV)
	if dav == nil {
		return remote
	}
	remote.RemoteID = dav.canonical(remote.RemoteID)
	if remote.SeriesRemoteID != "" {
		remote.SeriesRemoteID = dav.canonical(remote.SeriesRemoteID)
	}
	return remote
}
