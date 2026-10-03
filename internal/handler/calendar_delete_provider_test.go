package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarDeleteJSONProvidersConditionalAndSafe(t *testing.T) {
	for _, graph := range []bool{false, true} {
		for _, mode := range []string{"success", "changed", "wrong-id", "invalid-etag", "recurring", "guests", "invitation", "online", "race", "denied", "missing", "redirect", "accepted", "read-failure",
			"series-success", "series-changed", "series-wrong-id", "series-invalid-etag", "series-guests", "series-invitation", "series-online", "series-race", "series-denied", "series-missing", "series-redirect", "series-accepted", "series-read-failure", "series-occurrence", "series-single"} {
			t.Run(fmt.Sprintf("graph=%v/%s", graph, mode), func(t *testing.T) {
				series := strings.HasPrefix(mode, "series-")
				mode := strings.TrimPrefix(mode, "series-")
				existing := storage.CalendarEvent{RemoteID: "event", ETag: `"v1"`}
				current := map[string]any{"id": "event", "etag": `"v1"`, "attendees": []any{}, "recurrence": []any{}}
				if series {
					draft := calendarProviderDraft(t, false)
					draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 1, Count: 4}
					remote := calendarCreatedRemote(draft)
					remote.RemoteID, remote.ETag = existing.RemoteID, existing.ETag
					current["start"] = map[string]string{"dateTime": "2026-10-02T07:00:00Z", "timeZone": "Europe/Prague"}
					current["end"] = map[string]string{"dateTime": "2026-10-02T08:00:00Z", "timeZone": "Europe/Prague"}
					current["recurrence"] = []string{"RRULE:" + calendarRecurrenceRule(draft)}
					if graph {
						current["recurrence"] = calendarOutlookRecurrence(draft)
					}
					remote.Recurrence, _ = json.Marshal(current["recurrence"])
					existing = calendarStorageEvent("one", "source", remote)
				}
				if graph {
					existing.ETag = "v1"
					current["changeKey"], current["@odata.etag"], current["type"] = "v1", `W/"v1"`, "singleInstance"
					if series {
						current["type"] = "seriesMaster"
					}
				}
				switch mode {
				case "changed":
					current["etag"], current["changeKey"] = `"v2"`, "v2"
				case "wrong-id":
					current["id"] = "different-event"
				case "invalid-etag":
					current["etag"], current["@odata.etag"], existing.ETag = "*", "*", "*"
				case "recurring":
					current["recurrence"] = []string{"RRULE:FREQ=DAILY"}
					if graph {
						current["recurrence"] = map[string]any{"pattern": "daily"}
					}
				case "guests":
					current["attendees"] = []any{map[string]string{"email": "guest@example.com"}}
				case "invitation":
					current["organizer"], current["isOrganizer"] = map[string]bool{"self": false}, false
				case "online":
					current["hangoutLink"], current["isOnlineMeeting"] = "https://meet.example", true
				case "occurrence":
					current["recurringEventId"], current["seriesMasterId"], current["type"] = "parent", "parent", "occurrence"
				case "single":
					current["recurrence"], current["type"] = nil, "singleInstance"
				}
				gets, deletes, redirects := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirects++
						w.WriteHeader(204)
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/calendars/work/events/event") || r.Header.Get("Authorization") != "Bearer test-token" {
						t.Errorf("incorrect target/credentials: %s", r.URL.Path)
					}
					if r.Method == http.MethodGet {
						gets++
						if mode == "read-failure" {
							w.WriteHeader(503)
							return
						}
						_ = json.NewEncoder(w).Encode(current)
						return
					}
					if r.Method != http.MethodDelete {
						t.Errorf("unexpected method %s", r.Method)
						w.WriteHeader(405)
						return
					}
					deletes++
					expected := `"v1"`
					if graph {
						expected = `W/"v1"`
					}
					if gets != 1 || r.Header.Get("If-Match") != expected || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
						t.Error("missing read-before-delete or exact provider version")
					}
					if body, _ := io.ReadAll(r.Body); len(body) != 0 {
						t.Error("DELETE must not send a provider body")
					}
					switch mode {
					case "race":
						w.WriteHeader(412)
					case "denied":
						w.WriteHeader(403)
					case "missing":
						w.WriteHeader(404)
					case "redirect":
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(307)
					case "accepted":
						w.WriteHeader(202)
					default:
						w.WriteHeader(204)
					}
				}))
				defer server.Close()
				var err error
				if graph {
					old := outlookGraphBaseURL
					outlookGraphBaseURL = server.URL
					defer func() { outlookGraphBaseURL = old }()
					err = deleteOutlookCalendarEventScope(t.Context(), "test-token", "work", existing, series)
				} else {
					old := googleCalendarAPIBaseURL
					googleCalendarAPIBaseURL = server.URL
					defer func() { googleCalendarAPIBaseURL = old }()
					err = deleteGoogleCalendarEventScope(t.Context(), "test-token", "work", existing, series)
				}
				if redirects != 0 || deletes > 1 {
					t.Fatal("delete followed a redirect or retried")
				}
				switch mode {
				case "success":
					if err != nil || deletes != 1 {
						t.Fatalf("deletes=%d err=%v", deletes, err)
					}
				case "changed", "wrong-id":
					if !errors.Is(err, errCalendarUpdateConflict) || deletes != 0 {
						t.Fatalf("deletes=%d err=%v", deletes, err)
					}
				case "invalid-etag", "recurring", "guests", "invitation", "online", "occurrence", "single":
					if !errors.Is(err, errCalendarUpdateUnsupported) || deletes != 0 {
						t.Fatalf("deletes=%d err=%v", deletes, err)
					}
				case "race":
					if !errors.Is(err, errCalendarUpdateConflict) {
						t.Fatalf("expected conflict, got %v", err)
					}
				case "accepted":
					if err == nil || !calendarCreateUncertain(err) {
						t.Fatal("unconfirmed deletion reported success")
					}
				case "read-failure":
					var preflight calendarDeletePreflightError
					if !errors.As(err, &preflight) || deletes != 0 {
						t.Fatalf("deletes=%d err=%v", deletes, err)
					}
				default:
					if err == nil {
						t.Fatal("provider rejection reported success")
					}
				}
			})
		}
	}
}

func TestCalendarDeleteCalDAVSingleResourceOnly(t *testing.T) {
	for _, mode := range []string{"success", "ok", "changed", "wrong-uid", "recurring", "guests", "organizer", "multiple-events", "schedule-tag", "race", "denied", "redirect", "accepted", "multi-status", "collection", "outside-collection", "traversal", "read-failure",
		"series-success", "series-ok", "series-changed", "series-wrong-uid", "series-guests", "series-organizer", "series-multiple-events", "series-schedule-tag", "series-race", "series-denied", "series-redirect", "series-accepted", "series-multi-status", "series-collection", "series-outside-collection", "series-traversal", "series-read-failure", "series-single", "series-occurrence"} {
		t.Run(mode, func(t *testing.T) {
			series := strings.HasPrefix(mode, "series-")
			mode := strings.TrimPrefix(mode, "series-")
			draft := calendarProviderDraft(t, false)
			if series {
				draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 4}
			}
			raw, err := calendarCreateICS(draft)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "recurring":
				raw = strings.Replace(raw, "END:VEVENT", "RRULE:FREQ=DAILY\r\nEND:VEVENT", 1)
			case "guests":
				raw = strings.Replace(raw, "END:VEVENT", "ATTENDEE:mailto:guest@example.com\r\nEND:VEVENT", 1)
			case "organizer":
				raw = strings.Replace(raw, "END:VEVENT", "ORGANIZER:mailto:host@example.com\r\nEND:VEVENT", 1)
			case "multiple-events":
				raw = strings.Replace(raw, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:other\r\nDTSTART:20261003T090000Z\r\nDTEND:20261003T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
			case "single":
				raw = strings.Replace(raw, "RRULE:"+calendarRecurrenceRule(draft)+"\r\n", "", 1)
			case "occurrence":
				raw = strings.Replace(raw, "END:VEVENT", "RECURRENCE-ID:20261002T070000Z\r\nEND:VEVENT", 1)
			}
			gets, deletes, redirects := 0, 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirects++
					w.WriteHeader(204)
					return
				}
				username, password, ok := r.BasicAuth()
				if !ok || username != "cal-user" || password != "cal-password" || r.URL.Path != "/calendar/original.ics" {
					t.Errorf("incorrect DAV target/auth: %s", r.URL.Path)
				}
				if r.Method == http.MethodGet {
					gets++
					if mode == "read-failure" {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("ETag", `"v1"`)
					if mode == "changed" {
						w.Header().Set("ETag", `"v2"`)
					}
					if mode == "schedule-tag" {
						w.Header().Set("Schedule-Tag", `"schedule"`)
					}
					_, _ = io.WriteString(w, raw)
					return
				}
				if r.Method != http.MethodDelete {
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(405)
					return
				}
				deletes++
				if gets != 1 || r.Header.Get("If-Match") != `"v1"` {
					t.Error("unconditional DAV deletion")
				}
				switch mode {
				case "ok":
					w.WriteHeader(200)
				case "race":
					w.WriteHeader(412)
				case "denied":
					w.WriteHeader(403)
				case "redirect":
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(307)
				case "accepted":
					w.WriteHeader(202)
				case "multi-status":
					w.WriteHeader(207)
				default:
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			old := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			defer func() { calDAVHTTPTransport = old }()
			source := storage.CalendarSource{RemoteID: server.URL + "/calendar/"}
			existing := storage.CalendarEvent{RemoteID: server.URL + "/calendar/original.ics", ETag: `"v1"`, ICalUID: draft.RequestID + "@gofer"}
			if series {
				remote := calendarCreatedRemote(draft)
				remote.RemoteID, remote.ETag, remote.ICalUID = existing.RemoteID, existing.ETag, existing.ICalUID
				remote.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(draft)})
				existing = calendarStorageEvent("one", "source", remote)
			}
			switch mode {
			case "wrong-uid":
				existing.ICalUID = "different-event"
			case "collection":
				existing.RemoteID = source.RemoteID
			case "outside-collection":
				existing.RemoteID = server.URL + "/other/event.ics"
			case "traversal":
				existing.RemoteID = server.URL + "/calendar/%2e%2e/event.ics"
			}
			err = deleteCalDAVCalendarEventScope(t.Context(), source, "cal-user", "cal-password", existing, series)
			if redirects != 0 || deletes > 1 {
				t.Fatal("DAV deletion retried or followed redirect")
			}
			switch mode {
			case "success", "ok":
				if err != nil || deletes != 1 {
					t.Fatalf("deletes=%d err=%v", deletes, err)
				}
			case "changed", "wrong-uid":
				if !errors.Is(err, errCalendarUpdateConflict) || deletes != 0 {
					t.Fatalf("deletes=%d err=%v", deletes, err)
				}
			case "recurring", "guests", "organizer", "multiple-events", "schedule-tag", "collection", "outside-collection", "traversal", "single", "occurrence":
				if !errors.Is(err, errCalendarUpdateUnsupported) || deletes != 0 {
					t.Fatalf("deletes=%d err=%v", deletes, err)
				}
			case "race":
				if !errors.Is(err, errCalendarUpdateConflict) {
					t.Fatalf("expected conflict, got %v", err)
				}
			case "accepted", "multi-status":
				if err == nil || !calendarCreateUncertain(err) {
					t.Fatal("unconfirmed deletion reported success")
				}
			case "read-failure":
				var preflight calendarDeletePreflightError
				if !errors.As(err, &preflight) || deletes != 0 {
					t.Fatalf("deletes=%d err=%v", deletes, err)
				}
			default:
				if err == nil {
					t.Fatal("provider rejection reported success")
				}
			}
		})
	}
}
