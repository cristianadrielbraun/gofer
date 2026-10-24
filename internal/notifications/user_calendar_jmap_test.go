package notifications

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
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
