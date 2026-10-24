package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// ownedJMAPCalendarServer serves each owner's JMAP account, which has a
// calendar "c1" holding a weekly series and a calendar "c2"; its password is
// the fixture's mailbox password.
func ownedJMAPCalendarServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, password, ok := r.BasicAuth()
		if !ok || password != "synthetic-only" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/.well-known/jmap" {
			_, _ = w.Write([]byte(`{"capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:calendars": {}},
			  "accounts": {"a1": {"name": "` + owner + `"}}, "primaryAccounts": {"urn:ietf:params:jmap:calendars": "a1"},
			  "apiUrl": "http://` + r.Host + `/api/", "state": "s"}`))
			return
		}
		var request struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		var name string
		var args struct {
			IDs    []string          `json:"ids"`
			Filter map[string]string `json:"filter"`
		}
		_ = json.Unmarshal(request.MethodCalls[0][0], &name)
		_ = json.Unmarshal(request.MethodCalls[0][1], &args)
		series := map[string]any{"id": "s1", "@type": "Event", "uid": owner + "-standup@example.test", "title": owner + " standup",
			"start": "2026-10-05T09:00:00", "timeZone": "Europe/Prague", "duration": "PT15M",
			"recurrenceRule": map[string]any{"frequency": "weekly", "count": 3}}
		var answer any = map[string]any{"type": "unknownMethod"}
		responseName := "error"
		switch name {
		case "Calendar/get":
			responseName = name
			answer = map[string]any{"accountId": "a1", "state": "c", "list": []any{
				map[string]any{"id": "c1", "name": owner + " calendar", "isDefault": true, "myRights": map[string]bool{"mayReadItems": true, "mayWriteAll": true}},
				map[string]any{"id": "c2", "name": "Holidays", "myRights": map[string]bool{"mayReadItems": true}},
			}}
		case "CalendarEvent/get":
			responseName = name
			list := []any{}
			if slices.Contains(args.IDs, "s1") {
				list = append(list, series)
			}
			answer = map[string]any{"accountId": "a1", "state": "e1", "list": list, "notFound": []string{}}
		case "CalendarEvent/query":
			responseName = name
			ids := []string{}
			if args.Filter["inCalendar"] == "c1" {
				ids = append(ids, "s1")
			}
			answer = map[string]any{"accountId": "a1", "ids": ids, "position": 0, "total": len(ids)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{responseName, answer, "0"}}, "sessionState": "s"})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUserJMAPCalendarDiscoveryAndSync(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	server := ownedJMAPCalendarServer(t)
	address, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(address.Port())
	f := newUserStorageFixtureServices(t, true, &mailauth.Config{}, nil, nil)
	if err := f.system.AddPlaintextTransportException(t.Context(), "http", address.Hostname(), port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "http", address.Hostname(), port, "administrator"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET provider='jmap',jmap_session_url=?,imap_host='',smtp_host='' WHERE id=?`, server.URL+"/.well-known/jmap", f.accounts[owner].ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, owner := range []string{"alice", "bob"} {
		response := f.request(owner, "POST", "/api/accounts/"+f.accounts[owner].ID+"/calendar/discover", "calendar_protocol=jmap")
		if response.Code != 200 || !strings.Contains(response.Body.String(), "Discovered 2 JMAP calendar(s)") {
			t.Fatalf("%s discovery = %d %s", owner, response.Code, response.Body.String())
		}
		response = f.request(owner, "POST", "/api/calendar/sync", url.Values{"month": {"2026-10"}, "account_id": {f.accounts[owner].ID}}.Encode())
		if response.Code != 200 {
			t.Fatalf("%s sync = %d %s", owner, response.Code, response.Body.String())
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			sources, err := db.ListCalendarSourcesForAccount(t.Context(), owner, f.accounts[owner].ID)
			if err != nil {
				return err
			}
			if len(sources) != 2 || sources[0].Provider != "jmap" {
				t.Fatalf("%s sources = %+v", owner, sources)
			}
			events, err := db.ListCalendarEvents(t.Context(), owner, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				return err
			}
			var starts []string
			for _, event := range events {
				if event.Summary != owner+" standup" {
					t.Fatalf("%s got %q", owner, event.Summary)
				}
				starts = append(starts, event.StartAt.UTC().Format(time.RFC3339))
			}
			slices.Sort(starts)
			if !slices.Equal(starts, []string{"2026-10-05T07:00:00Z", "2026-10-12T07:00:00Z", "2026-10-19T07:00:00Z"}) {
				t.Fatalf("%s occurrences = %v", owner, starts)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestUserJMAPCalendarLiveWrites creates, edits and deletes an event on a
// real server through the owned HTTP routes, and skips unless
// GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestUserJMAPCalendarLiveWrites(t *testing.T) {
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	f := newUserStorageFixtureServices(t, true, &mailauth.Config{}, nil, nil)
	address, _ := url.Parse(sessionURL)
	port, _ := strconv.Atoi(address.Port())
	if address.Scheme == "http" {
		if err := f.system.AddPlaintextTransportException(t.Context(), "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
		if err := f.system.AddPrivateTargetException(t.Context(), "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
	}
	account, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: user, DisplayName: "Live", JMAPSessionURL: sessionURL, Username: user, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := f.request("alice", "POST", "/api/accounts/"+account.ID+"/calendar/discover", "calendar_protocol=jmap"); r.Code != 200 || !strings.Contains(r.Body.String(), "JMAP calendar(s)") {
		t.Fatalf("discovery = %d %s", r.Code, r.Body.String())
	}
	var source storage.CalendarSource
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		sources, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", account.ID)
		for _, candidate := range sources {
			if candidate.IsPrimary {
				source = candidate
			}
		}
		return err
	}); err != nil || source.ID == "" {
		t.Fatalf("source = %+v, %v", source, err)
	}
	cfg := &models.AccountConfig{Provider: "jmap", JMAPSessionURL: sessionURL, Username: user, JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
	connection, err := mail.OpenJMAPCalendars(t.Context(), cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	summary := "Gofer owned live " + time.Now().UTC().Format("150405")
	form := url.Values{"request_id": {uuid.NewString()}, "source_id": {source.ID}, "summary": {summary}, "start_date": {"2026-12-09"}, "end_date": {"2026-12-09"},
		"start_time": {"10:00"}, "end_time": {"11:00"}, "timezone": {"Europe/Prague"}, "description": {"Owned notes"}}
	r := f.request("alice", "POST", "/api/calendar/events", form.Encode())
	var created map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &created); err != nil || r.Code != 201 {
		t.Fatalf("create = %d %s", r.Code, r.Body.String())
	}
	eventID, _ := created["event_id"].(string)
	stored := func() storage.CalendarEvent {
		t.Helper()
		var event storage.CalendarEvent
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			var err error
			event, err = db.GetCalendarEvent(t.Context(), "alice", eventID)
			return err
		}); err != nil {
			t.Fatalf("stored event = %+v, %v", event, err)
		}
		return event
	}
	remote := func(event storage.CalendarEvent) jmap.CalendarEvent {
		t.Helper()
		parsed, _ := url.Parse(event.RemoteID)
		name := strings.TrimSuffix(parsed.Path[strings.LastIndex(parsed.Path, "/")+1:], ".ics")
		found, err := connection.Event(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	event := stored()
	t.Cleanup(func() {
		if found := remote(event); found != nil {
			_, _ = connection.Delete(context.Background(), found.ID())
		}
	})
	if found := remote(event); found == nil || found["title"] != summary {
		t.Fatalf("created on the server = %+v (event %+v)", found, event)
	}

	form.Set("summary", summary+" edited")
	form.Set("start_time", "12:00")
	form.Set("end_time", "13:00")
	form.Set("version", event.ETag)
	if r := f.request("alice", "PATCH", "/api/calendar/events/"+eventID, form.Encode()); r.Code != 200 || !strings.Contains(r.Body.String(), `"saved":true`) {
		t.Fatalf("edit = %d %s", r.Code, r.Body.String())
	}
	event = stored()
	if found := remote(event); found == nil || found["title"] != summary+" edited" {
		t.Fatalf("edited on the server = %+v", found)
	}
	query := url.Values{"version": {event.ETag}}
	if r := f.request("alice", "DELETE", "/api/calendar/events/"+eventID+"?"+query.Encode(), ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"deleted":true`) {
		t.Fatalf("delete = %d %s", r.Code, r.Body.String())
	}
	if found := remote(event); found != nil {
		t.Fatal("the deleted event is still on the server")
	}
}
