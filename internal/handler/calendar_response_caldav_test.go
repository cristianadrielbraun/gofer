package handler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func replyCalendar(events string) string {
	return "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Fixture//EN\n" + events + "END:VCALENDAR\n"
}
func replyEvent(dates, extra string) string {
	return "BEGIN:VEVENT\nUID:invite-uid\nDTSTAMP:20260901T000000Z\nSEQUENCE:7\n" + dates + "\nSUMMARY:Meeting\nORGANIZER:mailto:host@example.com\nATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:me@example.com\nATTENDEE;PARTSTAT=ACCEPTED:mailto:other@example.com\nX-PRIVATE:keep\n" + extra + "\nEND:VEVENT\n"
}

type replyDAVFixture struct {
	source               storage.CalendarSource
	event                storage.CalendarEvent
	server               *httptest.Server
	body, expanded, mode string
	writes               int
}

func newReplyDAVFixture(t *testing.T, mode, scope, dates string) *replyDAVFixture {
	t.Helper()
	f := &replyDAVFixture{mode: mode}
	master := "DTSTART:20261002T070000Z\nDTEND:20261002T080000Z"
	instance := "RECURRENCE-ID:20261009T070000Z\nDTSTART:20261009T070000Z\nDTEND:20261009T080000Z"
	key := "2026-10-09T07:00:00Z"
	if dates == "all-day" {
		master = "DTSTART;VALUE=DATE:20261002\nDTEND;VALUE=DATE:20261004"
		instance = "RECURRENCE-ID;VALUE=DATE:20261009\nDTSTART;VALUE=DATE:20261009\nDTEND;VALUE=DATE:20261011"
		key = "2026-10-09"
	}
	if dates == "dst" {
		master = "DTSTART;TZID=Europe/Prague:20261002T090000\nDTEND;TZID=Europe/Prague:20261002T100000"
		instance = "RECURRENCE-ID:20261030T080000Z\nDTSTART:20261030T080000Z\nDTEND:20261030T090000Z"
		key = "2026-10-30T08:00:00Z"
	}
	if dates == "first" {
		instance = master
		key = "2026-10-02T07:00:00Z"
	}
	if dates == "moved" {
		instance = strings.ReplaceAll(instance, "DTSTART:20261009T070000Z", "DTSTART:20261010T110000Z")
		instance = strings.ReplaceAll(instance, "DTEND:20261009T080000Z", "DTEND:20261010T120000Z")
	}
	extra := "BEGIN:VALARM\nACTION:DISPLAY\nTRIGGER:-PT15M\nDESCRIPTION:Private reminder\nEND:VALARM"
	if scope != "event" {
		extra = "RRULE:FREQ=WEEKLY\n" + extra
	}
	f.body = replyCalendar(replyEvent(master, extra))
	f.expanded = replyCalendar(replyEvent(instance, ""))
	if scope != "event" {
		sibling := replyEvent("RECURRENCE-ID:20261016T070000Z\nDTSTART:20261016T110000Z\nDTEND:20261016T120000Z", "X-SIBLING:keep")
		if dates == "moved" {
			sibling += replyEvent(instance, "X-EXCEPTION:keep")
		}
		f.body = strings.Replace(f.body, "END:VCALENDAR", sibling+"END:VCALENDAR", 1)
	}
	switch mode {
	case "client":
		f.body = strings.ReplaceAll(f.body, "ORGANIZER:", "ORGANIZER;SCHEDULE-AGENT=CLIENT:")
	case "none":
		f.body = strings.ReplaceAll(f.body, "ORGANIZER:", "ORGANIZER;SCHEDULE-AGENT=NONE:")
	case "foreign":
		f.body = strings.ReplaceAll(f.body, "mailto:me@example.com", "mailto:foreign@example.com")
	case "organizer":
		f.body = strings.ReplaceAll(f.body, "mailto:host@example.com", "mailto:me@example.com")
	case "duplicate":
		f.body = strings.ReplaceAll(f.body, "X-PRIVATE:keep", "ATTENDEE:mailto:me@example.com\nX-PRIVATE:keep")
	case "delegated":
		f.body = strings.ReplaceAll(f.body, "RSVP=TRUE:", "RSVP=TRUE;DELEGATED-TO=\"mailto:other@example.com\":")
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != "pass" {
			t.Error("missing DAV auth")
		}
		switch r.Method {
		case "OPTIONS":
			if mode == "options-fail" {
				w.WriteHeader(500)
				return
			}
			if mode == "options-unsupported" {
				w.WriteHeader(405)
				return
			}
			if mode != "fallback" {
				w.Header().Set("DAV", "1, calendar-access, calendar-auto-schedule")
			}
			w.WriteHeader(200)
		case "PROPFIND":
			props := `<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>`
			if mode == "cross-principal" {
				props = `<d:current-user-principal><d:href>https://foreign.invalid/principal/</d:href></d:current-user-principal>`
			}
			if r.URL.Path == "/principal/" {
				props = `<c:calendar-user-address-set><d:href>mailto:me@example.com</d:href></c:calendar-user-address-set>`
				if mode == "no-addresses" {
					props = ""
				}
			}
			w.WriteHeader(207)
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, props)
		case "REPORT":
			var escaped bytes.Buffer
			_ = xml.EscapeText(&escaped, []byte(f.expanded))
			etag := `&quot;v1&quot;`
			if mode == "report-race" {
				etag = `&quot;changed&quot;`
			}
			w.WriteHeader(207)
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/cal/event.ics</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, etag, escaped.String())
		case "GET":
			etag := `"v1"`
			if f.writes > 0 || mode == "stale" {
				etag = `"v2"`
			}
			w.Header().Set("ETag", etag)
			w.Header().Set("Schedule-Tag", `"schedule1"`)
			if f.writes > 0 && mode == "unconfirmed" {
				io.WriteString(w, "invalid")
				return
			}
			body := f.body
			if f.writes > 0 && mode != "fallback" && mode != "client" && mode != "no-addresses" && mode != "options-unsupported" {
				cal, err := calendarUpdateDecodeICS([]byte(body))
				if err != nil {
					t.Error(err)
					return
				}
				code := "1.2"
				if mode == "pending" {
					code = "1.0"
				}
				if mode == "delivery-failed" {
					code = "5.1"
				}
				for _, e := range cal.Events() {
					e.Props.Get("ORGANIZER").Params.Set("SCHEDULE-STATUS", code)
				}
				body, _ = encodeCalendarReply(cal)
			}
			io.WriteString(w, body)
		case "PUT":
			f.writes++
			if r.URL.Path != "/cal/event.ics" || r.Header.Get("If-Match") != `"v1"` || r.Header.Get("If-Schedule-Tag-Match") != `"schedule1"` {
				t.Error("unsafe conditional DAV write")
			}
			if mode == "race" {
				w.WriteHeader(412)
				return
			}
			if mode == "denied" {
				w.WriteHeader(403)
				return
			}
			data, _ := io.ReadAll(r.Body)
			f.body = string(data)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(f.server.Close)
	previous := calDAVHTTPTransport
	calDAVHTTPTransport = f.server.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = previous })
	f.source = storage.CalendarSource{UserID: "one", ID: "one-source", AccountID: "one-account", Provider: "caldav", RemoteID: f.server.URL + "/cal/", AccessRole: "owner", IsSelected: true}
	cal, _ := calendarUpdateDecodeICS([]byte(f.body))
	remote, err := calDAVResponseEvent(cal.Events()[0].Component, f.server.URL+"/cal/event.ics", `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if scope != "event" {
		expanded, _ := calendarUpdateDecodeICS([]byte(f.expanded))
		remote, err = normalizeCalDAVEvent(expanded.Events()[0], f.server.URL+"/cal/event.ics", `"v1"`, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		remote.RemoteID = f.server.URL + "/cal/event.ics#recurrence=" + url.QueryEscape(key)
		remote.SeriesRemoteID = f.server.URL + "/cal/event.ics"
	}
	f.event = calendarStorageEvent("one", "one-source", remote)
	f.event.ID = "edit-event"
	return f
}

func TestCalDAVResponsePaths(t *testing.T) {
	for _, scope := range []string{"event", "occurrence", "series"} {
		for _, mode := range []string{"server", "fallback", "client", "no-addresses", "options-unsupported"} {
			for _, answer := range []string{"accepted", "tentative", "declined"} {
				t.Run(scope+"/"+mode+"/"+answer, func(t *testing.T) {
					f := newReplyDAVFixture(t, mode, scope, "timed")
					target, err := readCalDAVResponseTarget(t.Context(), f.source, f.event, scope, "user", "pass", "me@example.com")
					if err != nil {
						t.Fatal(err)
					}
					if target.CalDAV.ServerScheduling != (mode == "server") {
						t.Fatal("wrong delivery route")
					}
					desired, reply, err := prepareCalDAVResponse(target, answer)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(reply, "other@example.com") || strings.Contains(reply, "X-PRIVATE") || strings.Contains(reply, "VALARM") {
						t.Fatal("email leaks guests or private metadata")
					}
					ics, err := calendarUpdateDecodeICS([]byte(reply))
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range ics.Events() {
						if len(event.Props["ATTENDEE"]) != 1 || event.Props.Get("ATTENDEE").Params.Get("PARTSTAT") != strings.ToUpper(answer) || event.Props.Get("SEQUENCE").Value != "7" {
							t.Fatal("invalid iTIP REPLY")
						}
					}
					result, err := putCalDAVResponse(t.Context(), target, desired, answer)
					if err != nil {
						t.Fatal(err)
					}
					if f.writes != 1 || result.Pending || result.Event.ResponseStatus != answer || result.Event.ETag != `"v2"` {
						t.Fatalf("bad result: %+v", result)
					}
					if !strings.Contains(f.body, "X-PRIVATE:keep") || !strings.Contains(f.body, "BEGIN:VALARM") {
						t.Fatal("resource metadata lost")
					}
				})
			}
		}
	}
}

func TestCalDAVResponseOccurrenceDates(t *testing.T) {
	for _, kind := range []string{"timed", "all-day", "dst", "first", "moved"} {
		t.Run(kind, func(t *testing.T) {
			f := newReplyDAVFixture(t, "server", "occurrence", kind)
			target, err := readCalDAVResponseTarget(t.Context(), f.source, f.event, "occurrence", "user", "pass", "me@example.com")
			if err != nil {
				t.Fatal(err)
			}
			desired, _, err := prepareCalDAVResponse(target, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			result, err := putCalDAVResponse(t.Context(), target, desired, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			if result.Event.RemoteID != f.event.RemoteID || result.Event.StartDate != f.event.StartDate || (f.event.StartAt != nil && !result.Event.StartAt.Equal(*f.event.StartAt)) {
				t.Fatal("occurrence moved or lost original identity")
			}
			for _, e := range desired.Events() {
				key, _ := calendarRecurrenceKey(e.Props.Get("RECURRENCE-ID"), time.UTC)
				if key != "" && strings.Contains(f.event.RemoteID, url.QueryEscape(key)) {
					continue
				}
				_, status, err := calDAVResponseSelf(e.Component, []string{"me@example.com"})
				if err != nil || status != "needsAction" {
					t.Fatal("another occurrence was changed")
				}
			}
		})
	}
}

func TestCalDAVResponseGuards(t *testing.T) {
	for _, mode := range []string{"stale", "foreign", "organizer", "duplicate", "delegated", "none", "options-fail", "cross-principal", "report-race", "race", "denied", "unconfirmed", "pending", "delivery-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newReplyDAVFixture(t, mode, "occurrence", "timed")
			target, err := readCalDAVResponseTarget(t.Context(), f.source, f.event, "occurrence", "user", "pass", "me@example.com")
			post := mode == "race" || mode == "denied" || mode == "unconfirmed" || mode == "pending" || mode == "delivery-failed"
			if !post {
				if err == nil || f.writes != 0 {
					t.Fatal("unsafe invitation accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			desired, _, err := prepareCalDAVResponse(target, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			result, err := putCalDAVResponse(t.Context(), target, desired, "accepted")
			if f.writes != 1 || (mode == "pending" && (err != nil || !result.Pending)) || (mode != "pending" && err == nil) {
				t.Fatalf("unconfirmed reply accepted: %+v %v", result, err)
			}
		})
	}
}

func TestCalDAVReplyIdentityAndTimezoneFallback(t *testing.T) {
	for _, mode := range []string{"server", "fallback", "client"} {
		t.Run(mode, func(t *testing.T) {
			f := newReplyDAVFixture(t, mode, "series", "dst")
			target, err := readCalDAVResponseTarget(t.Context(), f.source, f.event, "series", "user", "pass", "different@example.com")
			if mode != "server" {
				if err == nil {
					t.Fatal("email fallback impersonated another attendee")
				}
				return
			}
			if err != nil {
				t.Fatal("server must allow a verified calendar alias", err)
			}
			desired, reply, err := prepareCalDAVResponse(target, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(reply, "TZID=Europe/Prague") || !strings.Contains(reply, "DTSTART:20261002T070000Z") {
				t.Fatal("iMIP reply requires self-contained timezone data")
			}
			if desired.Events()[0].Props.Get("DTSTART").Params.Get("TZID") != "Europe/Prague" {
				t.Fatal("reply serialization changed stored timezone")
			}
		})
	}
}
