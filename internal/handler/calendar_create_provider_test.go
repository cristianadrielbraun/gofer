package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

func calendarProviderDraft(t *testing.T, allDay bool) calendar.EventDraft {
	t.Helper()
	values := calendarCreateForm()
	if allDay {
		values.Set("all_day", "true")
	}
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func TestGoogleCalendarCreationUsesStableIDAndRecoversConflict(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		t.Run(fmt.Sprint(allDay), func(t *testing.T) {
			draft := calendarProviderDraft(t, allDay)
			var stored map[string]any
			var posts, gets int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing authorization")
				}
				if !strings.HasPrefix(r.URL.Path, "/calendars/work@example.com/events") {
					t.Errorf("wrong source URL %s", r.URL.Path)
				}
				if r.Method == http.MethodGet {
					gets++
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				posts++
				if stored != nil {
					w.WriteHeader(409)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
					t.Error(err)
				}
				if stored["id"] != strings.ReplaceAll(draft.RequestID, "-", "") {
					t.Error("event identity is not stable")
				}
				if _, ok := stored["attendees"]; ok {
					t.Error("creation added invitations")
				}
				if _, ok := stored["recurrence"]; ok {
					t.Error("creation added recurrence")
				}
				start := stored["start"].(map[string]any)
				if allDay {
					if start["date"] != "2026-10-02" {
						t.Error("missing date-only start")
					}
				} else if start["timeZone"] != "Europe/Prague" || start["dateTime"] != "2026-10-02T09:00:00+02:00" {
					t.Error("wrong zoned start")
				}
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(stored)
			}))
			defer server.Close()
			prior := googleCalendarAPIBaseURL
			googleCalendarAPIBaseURL = server.URL
			defer func() { googleCalendarAPIBaseURL = prior }()
			for range 2 {
				remote, err := createGoogleCalendarEvent(t.Context(), "test-token", "work@example.com", draft)
				if err != nil || remote.AllDay != allDay || remote.Summary != "Planning" {
					t.Fatalf("created=%#v, err=%v", remote, err)
				}
			}
			if posts != 2 || gets != 1 {
				t.Fatalf("posts=%d gets=%d", posts, gets)
			}
			stored["extendedProperties"] = map[string]any{"private": map[string]string{"goferCreateHash": "foreign"}}
			if _, err := createGoogleCalendarEvent(t.Context(), "test-token", "work@example.com", draft); err == nil {
				t.Fatal("foreign conflicting event accepted")
			}
		})
	}
}

func TestOutlookCalendarCreationUsesTransactionIDAndPreservesDates(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		t.Run(fmt.Sprint(allDay), func(t *testing.T) {
			draft := calendarProviderDraft(t, allDay)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/me/calendars/primary/events" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
					t.Error("incorrect Graph creation request")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["transactionId"] != draft.RequestID || payload["isAllDay"] != allDay {
					t.Error("missing idempotency or all-day state")
				}
				if _, ok := payload["attendees"]; ok {
					t.Error("creation added attendees")
				}
				start := payload["start"].(map[string]any)
				end := payload["end"].(map[string]any)
				if start["timeZone"] != "Europe/Prague" || end["timeZone"] != "Europe/Prague" {
					t.Error("creation discarded the chosen timezone")
				}
				if allDay && (start["dateTime"] != "2026-10-02T00:00:00" || end["dateTime"] != "2026-10-03T00:00:00") {
					t.Error("invalid exclusive all-day end")
				}
				payload["id"] = "graph-event"
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			prior := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = prior }()
			remote, err := createOutlookCalendarEvent(t.Context(), "test-token", "primary", draft)
			if err != nil || remote.RemoteID != "graph-event" || remote.AllDay != allDay {
				t.Fatalf("created=%#v, err=%v", remote, err)
			}
			if !allDay && (!remote.StartAt.Equal(*draft.StartAt) || !remote.EndAt.Equal(*draft.EndAt)) {
				t.Fatal("Graph creation changed the instants")
			}
		})
	}
}

func TestCalDAVCreationIsConditionalEscapedAndRetryable(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		t.Run(fmt.Sprint(allDay), func(t *testing.T) {
			draft := calendarProviderDraft(t, allDay)
			draft.Description = "Notes; commas, unicode: Žluťoučký\r\nBEGIN:VEVENT\nSUMMARY:injection"
			var stored string
			var puts, gets int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "calendar-user" || password != "calendar-password" {
					t.Error("missing calendar credentials")
				}
				if r.URL.Path != "/calendar/"+draft.RequestID+".ics" {
					t.Error("wrong resource URL")
				}
				w.Header().Set("ETag", `"created-v1"`)
				if r.Method == "GET" {
					gets++
					_, _ = io.WriteString(w, stored)
					return
				}
				puts++
				if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/calendar") {
					t.Error("unsafe CalDAV creation request")
				}
				if stored != "" {
					w.WriteHeader(412)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				stored = string(raw)
				decoded, err := ical.NewDecoder(strings.NewReader(stored)).Decode()
				if err != nil || len(decoded.Events()) != 1 {
					t.Error("iCalendar text injection created extra components")
				}
				w.WriteHeader(201)
			}))
			defer server.Close()
			prior := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			defer func() { calDAVHTTPTransport = prior }()
			source := storage.CalendarSource{RemoteID: server.URL + "/calendar/"}
			for range 2 {
				remote, err := createCalDAVCalendarEvent(t.Context(), source, "calendar-user", "calendar-password", draft)
				if err != nil || remote.AllDay != allDay || remote.ICalUID != draft.RequestID+"@gofer" || remote.Summary != "Planning" {
					t.Fatalf("created=%#v, err=%v", remote, err)
				}
			}
			if puts != 2 || gets != 1 {
				t.Errorf("puts=%d gets=%d", puts, gets)
			}
		})
	}
}

func TestCalendarCreationRejectsRedirectsAndMissingProviderIdentity(t *testing.T) {
	var leaked int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked++; w.WriteHeader(201) }))
	defer foreign.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 307) }))
	defer redirect.Close()
	var output map[string]any
	if err := calendarCreateJSON(t.Context(), http.MethodPost, redirect.URL, "secret", map[string]string{"title": "Private"}, &output); err == nil || leaked != 0 {
		t.Fatal("provider creation followed a redirect with private data")
	}
	for _, err := range []error{calendarCreateProviderError{400}, calendarCreateProviderError{403}, calendarCreateProviderError{409}, calendarCreateAuthError{context.Canceled}} {
		if calendarCreateUncertain(err) {
			t.Fatalf("known preflight/provider rejection classified as ambiguous: %v", err)
		}
	}
	if !calendarCreateUncertain(context.DeadlineExceeded) || !calendarCreateUncertain(calendarCreateProviderError{503}) {
		t.Fatal("uncertain result must retain the same request ID")
	}
}
