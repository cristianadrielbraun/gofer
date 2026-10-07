package notifications

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Native fixture reuses the existing read-only wire representation, then saves
// the actual conditional JSON/ICS sent by the registered per-user edit handler.
// Its fresh series refresh deliberately fails after acceptance: the HTTP result
// must report saved + refresh_pending, without cached fabricated occurrences.
type ownedCalendarSeriesUpdateAPI struct {
	t                 *testing.T
	provider, scope   string
	mu                sync.Mutex
	json              map[string]map[string]map[string]any
	dav               map[string]string
	writes, refreshes map[string]int
	completeRefresh   bool
}

func (a *ownedCalendarSeriesUpdateAPI) serve(base http.Handler) http.Handler {
	oldScope := a.scope
	if oldScope == "conversion" {
		oldScope = "event"
	}
	reads := (&ownedCalendarDeleteAPI{t: a.t, provider: a.provider, scope: oldScope, writes: map[string]int{}, bodies: map[string]string{}}).serve(base)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			base.ServeHTTP(w, r)
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		collection := "/calendars/primary/events"
		id := strings.TrimPrefix(r.URL.Path, collection+"/")
		if a.provider == "outlook" {
			collection = "/me" + collection
			id = strings.TrimPrefix(r.URL.Path, collection+"/")
		}
		if a.provider == "caldav" {
			user, password, ok := r.BasicAuth()
			owner = user
			if !ok || password != owner+"-calendar-secret" {
				a.t.Error("wrong series credentials")
				w.WriteHeader(401)
				return
			}
			collection = "/users/" + owner + "/calendars/primary/"
			id = "event.ics"
		}
		if owner != "alice" && owner != "bob" {
			a.t.Error("unknown series owner", owner)
			w.WriteHeader(401)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		refresh := r.Method == "GET" && (r.URL.Path == collection || strings.HasSuffix(r.URL.Path, "/calendarView")) || r.Method == "REPORT" && a.writes[owner] > 0
		if refresh {
			a.refreshes[owner]++
			if a.completeRefresh {
				a.serveRefresh(w, r, owner, collection)
			} else {
				w.WriteHeader(503)
			}
			return
		}
		write := r.Method == "PATCH" || r.Method == "PUT"
		if write {
			target := collection + "/event"
			version := `"v1"`
			if a.scope == "series" {
				target = collection + "/master"
				version = `"master-v1"`
			}
			if a.provider == "outlook" {
				version = "W/" + version
			}
			if a.provider == "caldav" {
				target = collection + id
				version = `"v1"`
			}
			if r.URL.Path != target || r.Header.Get("If-Match") != version {
				a.t.Error("wrong conditional series target", r.URL.Path, r.Header.Get("If-Match"), target, version)
				w.WriteHeader(412)
				return
			}
			a.writes[owner]++
			if a.provider == "caldav" {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					a.t.Error(err)
					w.WriteHeader(400)
					return
				}
				a.dav[owner] = string(raw)
				w.Header().Set("ETag", `"v2"`)
				w.WriteHeader(204)
				return
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				a.t.Error(err)
				w.WriteHeader(400)
				return
			}
			current := a.json[owner][id]
			if current == nil {
				a.t.Error("series was not preflighted")
				w.WriteHeader(400)
				return
			}
			for key, value := range payload {
				current[key] = value
			}
			if a.provider == "gmail" {
				current["etag"] = `"v2"`
			} else {
				current["@odata.etag"], current["changeKey"] = `W/"v2"`, `"v2"`
				if a.scope == "conversion" {
					current["type"] = "seriesMaster"
				}
			}
			_ = json.NewEncoder(w).Encode(current)
			return
		}
		if r.Method != "GET" && r.Method != "REPORT" {
			a.t.Error("unexpected series request", r.Method)
			w.WriteHeader(400)
			return
		}
		if a.provider == "caldav" {
			if r.Method == "GET" && a.dav[owner] != "" {
				w.Header().Set("ETag", `"v2"`)
				_, _ = io.WriteString(w, a.dav[owner])
				return
			}
			reads.ServeHTTP(w, r)
			return
		}
		if a.json[owner] == nil {
			a.json[owner] = map[string]map[string]any{}
		}
		if a.json[owner][id] == nil {
			wire := httptest.NewRecorder()
			reads.ServeHTTP(wire, r)
			if wire.Code != 200 {
				w.WriteHeader(wire.Code)
				_, _ = w.Write(wire.Body.Bytes())
				return
			}
			var event map[string]any
			if err := json.Unmarshal(wire.Body.Bytes(), &event); err != nil {
				a.t.Error(err)
				w.WriteHeader(400)
				return
			}
			a.json[owner][id] = event
		}
		_ = json.NewEncoder(w).Encode(a.json[owner][id])
	})
}
func newOwnedCalendarSeriesUpdateFixture(t *testing.T, provider, scope string, complete ...bool) (*userStorageFixture, *ownedCalendarSeriesUpdateAPI) {
	t.Helper()
	api := &ownedCalendarSeriesUpdateAPI{t: t, provider: provider, scope: scope, json: map[string]map[string]map[string]any{}, dav: map[string]string{}, writes: map[string]int{}, refreshes: map[string]int{}}
	api.completeRefresh = len(complete) > 0 && complete[0]
	f, _, server := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler { return api.serve(base) })
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			remote, parent := "event", "master"
			if scope == "conversion" {
				parent = ""
			}
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/event.ics"
				if parent != "" {
					parent = remote
					remote += "#recurrence=" + url.QueryEscape("2026-10-03T10:00:00Z")
				}
			}
			rows := []storage.CalendarEvent{{ID: "same-event", RemoteID: remote, SeriesRemoteID: parent, ICalUID: "private-uid", ETag: `"v1"`, Summary: owner + " cached", StartAt: &start, EndAt: &end}}
			if scope != "conversion" {
				rows = append(rows, storage.CalendarEvent{ID: "outside-event", RemoteID: "outside", SeriesRemoteID: parent, ICalUID: "private-uid", ETag: `"sibling-v1"`, Summary: owner + " outside", StartAt: &start, EndAt: &end})
			}
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", rows, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}
func TestUserCalendarUpdateHTTPNativeRecurringScopesAndPendingRefresh(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, scope := range []string{"series", "occurrence", "conversion"} {
			t.Run(provider+"/"+scope, func(t *testing.T) {
				f, api := newOwnedCalendarSeriesUpdateFixture(t, provider, scope)
				form := ownedCalendarCreateForm()
				form.Set("summary", "Edited recurring title")
				form.Set("start_date", "2026-10-03")
				form.Set("end_date", "2026-10-03")
				form.Set("version", `"v1"`)
				if scope == "series" {
					form.Set("edit_scope", "series")
					if provider != "caldav" {
						form.Set("version", `"master-v1"`)
					}
				}
				if scope == "occurrence" {
					form.Set("edit_scope", "occurrence")
				} else {
					form.Set("repeat_frequency", "daily")
					form.Set("repeat_interval", "1")
					form.Set("repeat_end", "count")
					form.Set("repeat_count", "4")
				}
				r := f.request("alice", "PATCH", "/api/calendar/events/same-event", form.Encode())
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"saved":true`) {
					t.Fatal("native recurrence update", r.Code, r.Body.String())
				}
				if scope != "occurrence" && !strings.Contains(r.Body.String(), `"refresh_pending":true`) {
					t.Fatal("failed refresh misreported", r.Body.String())
				}
				for _, owner := range []string{"alice", "bob"} {
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var title string
						var deleted bool
						if err := db.Read().QueryRow(`SELECT summary,is_deleted FROM calendar_events WHERE id='same-event'`).Scan(&title, &deleted); err != nil {
							return err
						}
						if owner == "bob" && (deleted || title != "bob cached") {
							t.Fatal("other owner's recurrence changed", title, deleted)
						}
						if owner == "alice" {
							if scope == "occurrence" && (deleted || title != "Edited recurring title") {
								t.Fatal("wrong occurrence publication", title, deleted)
							}
							if scope != "occurrence" && !deleted {
								t.Fatal("old occurrence retained after accepted recurrence mutation")
							}
						}
						if scope != "conversion" {
							var sibling bool
							if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='outside-event'`).Scan(&sibling); err != nil {
								return err
							}
							if sibling != (owner == "alice" && scope == "series") {
								t.Fatal("wrong sibling scope", owner, scope, sibling)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				api.mu.Lock()
				writes, other, refreshes := api.writes["alice"], api.writes["bob"], api.refreshes["alice"]
				api.mu.Unlock()
				if writes != 1 || other != 0 || scope != "occurrence" && refreshes == 0 {
					t.Fatal("write/refresh did not use original owner or released gates", writes, other, refreshes)
				}
			})
		}
	}
}

func (a *ownedCalendarSeriesUpdateAPI) serveRefresh(w http.ResponseWriter, r *http.Request, owner, collection string) {
	if a.provider == "caldav" {
		// The provider returns the four actual expanded instances of its saved
		// daily COUNT=4 rule, rather than asking local code to synthesize them.
		var wire strings.Builder
		wire.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Native fixture//EN\r\n")
		for day := 3; day <= 6; day++ {
			fmt.Fprintf(&wire, "BEGIN:VEVENT\r\nUID:private-uid\r\nDTSTAMP:20261003T090000Z\r\nRECURRENCE-ID:202610%02dT100000Z\r\nDTSTART:202610%02dT100000Z\r\nDTEND:202610%02dT110000Z\r\nSUMMARY:Edited recurring title\r\nEND:VEVENT\r\n", day, day, day)
		}
		wire.WriteString("END:VCALENDAR\r\n")
		var escaped bytes.Buffer
		_ = xml.EscapeText(&escaped, []byte(wire.String()))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%sevent.ics</d:href><d:propstat><d:prop><d:getetag>&quot;v2&quot;</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, collection, escaped.String())
		return
	}
	parent := "master"
	if a.scope == "conversion" {
		parent = "event"
	}
	master := a.json[owner][parent]
	if master == nil {
		a.t.Error("refresh had no saved native master")
		w.WriteHeader(500)
		return
	}
	var instances []map[string]any
	for index, id := range []string{"event", "outside", "native-three", "native-four"} {
		if a.scope == "conversion" && index == 0 {
			id = "event-instance"
		}
		copy := map[string]any{}
		for key, value := range master {
			copy[key] = value
		}
		delete(copy, "recurrence")
		copy["id"] = id
		day := index + 3
		if a.provider == "gmail" {
			copy["etag"], copy["recurringEventId"] = `"instance-v2"`, parent
			copy["start"] = map[string]string{"dateTime": fmt.Sprintf("2026-10-%02dT10:00:00Z", day), "timeZone": "UTC"}
			copy["end"] = map[string]string{"dateTime": fmt.Sprintf("2026-10-%02dT11:00:00Z", day), "timeZone": "UTC"}
			copy["originalStartTime"] = copy["start"]
		} else {
			copy["changeKey"], copy["@odata.etag"], copy["type"], copy["seriesMasterId"] = `"instance-v2"`, `W/"instance-v2"`, "occurrence", parent
			copy["start"] = map[string]string{"dateTime": fmt.Sprintf("2026-10-%02dT10:00:00", day), "timeZone": "UTC"}
			copy["end"] = map[string]string{"dateTime": fmt.Sprintf("2026-10-%02dT11:00:00", day), "timeZone": "UTC"}
		}
		instances = append(instances, copy)
	}
	key := "items"
	if a.provider == "outlook" {
		key = "value"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{key: instances})
}

func TestUserCalendarUpdateHTTPSeriesRefreshUsesNativeInstancesAfterGatesRelease(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, scope := range []string{"series", "conversion"} {
			t.Run(provider+"/"+scope, func(t *testing.T) {
				f, api := newOwnedCalendarSeriesUpdateFixture(t, provider, scope, true)
				form := ownedCalendarCreateForm()
				form.Set("summary", "Edited recurring title")
				form.Set("start_date", "2026-10-03")
				form.Set("end_date", "2026-10-03")
				form.Set("version", `"v1"`)
				if scope == "series" {
					form.Set("edit_scope", "series")
					if provider != "caldav" {
						form.Set("version", `"master-v1"`)
					}
				}
				form.Set("repeat_frequency", "daily")
				form.Set("repeat_interval", "1")
				form.Set("repeat_end", "count")
				form.Set("repeat_count", "4")
				r := f.request("alice", "PATCH", "/api/calendar/events/same-event", form.Encode())
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"saved":true`) || !strings.Contains(r.Body.String(), `"refresh_pending":false`) {
					t.Fatal("accepted mutation/native refresh", r.Code, r.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var active, changed int
					if err := db.Read().QueryRow(`SELECT count(*),sum(CASE WHEN summary='Edited recurring title' AND series_remote_id<>'' THEN 1 ELSE 0 END) FROM calendar_events WHERE is_deleted=0`).Scan(&active, &changed); err != nil {
						return err
					}
					if active != 4 || changed != 4 {
						t.Fatal("native instances not cached", active, changed)
					}
					if scope == "conversion" {
						var oldDeleted bool
						if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='same-event'`).Scan(&oldDeleted); err != nil {
							return err
						}
						if !oldDeleted {
							t.Fatal("former master shown as ordinary event")
						}
					}
					var state string
					if err := db.Read().QueryRow(`SELECT state FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state); err != nil {
						return err
					}
					if state != "ok" {
						t.Fatal("native completed refresh not recorded", state)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
					var count int
					if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events WHERE summary LIKE 'bob %' AND is_deleted=0`).Scan(&count); err != nil {
						return err
					}
					want := 2
					if scope == "conversion" {
						want = 1
					}
					if count != want {
						t.Fatal("refresh crossed owners", count, want)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				writes, refreshes := api.writes["alice"], api.refreshes["alice"]
				api.mu.Unlock()
				if writes != 1 || refreshes == 0 {
					t.Fatal("missing bounded native mutation/instance refresh", writes, refreshes)
				}
			})
		}
	}
}

func TestUserCalendarMeetingDraftHTTPPreparedMeetPatchKeepsPrivateEventTarget(t *testing.T) {
	preview := &ownedMeetingPreviewAPI{t: t, provider: "gmail", remote: map[string]map[string]any{}, posts: map[string]int{}, deletes: map[string]int{}}
	final := &ownedCalendarCreateAPI{t: t, provider: "gmail", remote: map[string]map[string]any{}, dav: map[string]string{}, etags: map[string]string{}, writes: map[string]int{}, creates: map[string]int{}, calls: map[string]int{}}
	// A real created Google event identifies its organizer. Retain that wire
	// field through the ordinary create, preflight, conditional PATCH and cache.
	final.afterWrite = func(r *http.Request) {
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		final.remote[owner]["organizer"] = map[string]any{"self": true, "email": owner + "@provider.test"}
	}
	f, _, _ := newOwnedCalendarFixture(t, "gmail", func(base *ownedCalendarDiscoveryAPI) http.Handler {
		drafts, events := preview.serve(base), final.serve(base)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			collection := "/calendars/primary/events"
			if r.URL.Path == collection+"/"+strings.ReplaceAll(ownedCalendarCreateID, "-", "") {
				events.ServeHTTP(w, r)
				return
			}
			if r.Method == "POST" && r.URL.Path == collection {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var payload map[string]any
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if payload["id"] == strings.ReplaceAll(ownedCalendarCreateID, "-", "") {
					events.ServeHTTP(w, r)
					return
				}
			}
			drafts.ServeHTTP(w, r)
		})
	})
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		form := ownedCalendarCreateForm()
		created := f.request(owner, "POST", "/api/calendar/events", form.Encode())
		var result map[string]any
		if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != 201 {
			t.Fatal("ordinary native create", created.Code, created.Body.String(), err)
		}
		eventID, _ := result["event_id"].(string)
		previewForm := url.Values{"source_id": {"same-source"}, "draft_id": {ownedPreviewID}, "event_id": {eventID}}
		ready := f.request(owner, "POST", ownedPreviewPath("gmail"), previewForm.Encode())
		if ready.Code != 200 {
			t.Fatal("event-scoped Meet preparation", ready.Code, ready.Body.String())
		}
		form.Set("summary", "Event with prepared Meet")
		form.Set("version", `"created-v1"`)
		form.Set("google_meet_meeting", "true")
		form.Set("google_meet_draft_id", ownedPreviewID)
		saved := f.request(owner, "PATCH", "/api/calendar/events/"+eventID, form.Encode())
		if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"google_meet_unconfirmed":false`) {
			t.Fatal("prepared Meet PATCH", saved.Code, saved.Body.String())
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			event, err := db.GetCalendarEvent(t.Context(), owner, eventID)
			if err != nil {
				return err
			}
			if event.Summary != "Event with prepared Meet" || !strings.Contains(event.OnlineMeetingJSON, "https://meet.google.com/abc-defg-hij") || !strings.Contains(event.OnlineMeetingJSON, "private-signature") || event.ResponseStatus != "organizer" {
				t.Fatal("prepared Meet/organizer changed", event)
			}
			var used string
			if err := db.Read().QueryRow(`SELECT used_by FROM calendar_meet_drafts WHERE draft_id=?`, ownedPreviewID).Scan(&used); err != nil {
				return err
			}
			if used != "event:"+eventID {
				t.Fatal("meeting bound to another target", used)
			}
			start := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{event, {ID: "other-event", RemoteID: "another-event", ICalUID: "other-uid", ETag: `"other-v1"`, Summary: "Other ordinary event", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		form.Set("version", `"other-v1"`)
		denied := f.request(owner, "PATCH", "/api/calendar/events/other-event", form.Encode())
		if denied.Code != 409 || !strings.Contains(denied.Body.String(), `"uncertain":false`) {
			t.Fatal("prepared Meet retargeted", denied.Code, denied.Body.String())
		}
		final.mu.Lock()
		writes, creates := final.writes[owner], final.creates[owner]
		final.mu.Unlock()
		preview.mu.Lock()
		posts := preview.posts[owner]
		preview.mu.Unlock()
		if writes != 2 || creates != 1 || posts != 1 {
			t.Fatal("meeting edit duplicated or retargeted native work", writes, creates, posts)
		}
	}
}
