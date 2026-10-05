package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func calendarTeamsPreviewFixture(t *testing.T) *Handler {
	t.Helper()
	h := calendarCreateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook',provider_account_id='microsoft-subject' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE account_id='one-account'`); err != nil {
		t.Fatal(err)
	}
	h.mailboxAuth = mailauth.New(&mailauth.Config{}, h.db, testMailboxCredentialKey)
	expires := time.Now().Add(time.Hour)
	if err := h.mailboxAuth.UpsertOAuthAccount(t.Context(), "one-account", providers.OAuthMicrosoft, "microsoft-subject", "token", "refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
		t.Fatal(err)
	}
	return h
}
func TestCalendarTeamsPreviewFinalizesSameEventAndRecoversLostResponses(t *testing.T) {
	for _, mode := range []string{"business", "personal", "lost-create", "lost-save", "lost-local", "all-day"} {
		t.Run(mode, func(t *testing.T) {
			h := calendarTeamsPreviewFixture(t)
			draftID := "2d07e60e-5f8b-49d7-8e3c-c9e3bf5640af"
			join := "https://teams.microsoft.com/l/meetup-join/19%3ameeting_TEST/0?context=test"
			if mode == "personal" {
				join = "https://teams.live.com/meet/123456"
			}
			var remote map[string]any
			posts, patches, deletes, reads := 0, 0, 0, 0
			failConfirmation := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
					t.Error("wrong credentials or unstable identity")
				}
				if r.URL.Path == "/me/calendars/primary" {
					provider := "teamsForBusiness"
					if mode == "personal" {
						provider = "unknown"
					}
					json.NewEncoder(w).Encode(map[string]any{"id": "primary", "canEdit": true, "allowedOnlineMeetingProviders": []string{provider}, "defaultOnlineMeetingProvider": provider})
					return
				}
				collection := "/me/calendars/primary/events"
				if r.URL.Path == collection && r.Method == "GET" {
					if !strings.Contains(r.URL.Query().Get("$filter"), "draft:"+draftID) || r.URL.Query().Get("$expand") != calendarTeamsDraftExpand() {
						t.Error("draft recovery lookup not scoped")
					}
					events := []any{}
					if remote != nil {
						events = append(events, remote)
					}
					json.NewEncoder(w).Encode(map[string]any{"value": events})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/calendarView") {
					if r.URL.Query().Get("$expand") != calendarTeamsDraftExpand() {
						t.Error("sync cannot recognize drafts")
					}
					events := []any{}
					if remote != nil {
						events = append(events, remote)
					}
					json.NewEncoder(w).Encode(map[string]any{"value": events})
					return
				}
				if r.Method == "POST" {
					posts++
					if posts > 1 {
						t.Error("allocated a duplicate prepared event")
					}
					if err := json.NewDecoder(r.Body).Decode(&remote); err != nil {
						t.Error(err)
					}
					if remote["transactionId"] != draftID || remote["attendees"] != nil || remote["sensitivity"] != "private" || remote["showAs"] != "free" || remote["isReminderOn"] != false {
						t.Error("preparation is not private, silent, or idempotent")
					}
					if mode == "personal" && remote["onlineMeetingProvider"] != nil {
						t.Error("consumer provider overwritten")
					}
					remote["id"], remote["iCalUId"], remote["changeKey"], remote["@odata.etag"], remote["isOrganizer"] = "temporary-native", "uid", "v1", `W/"v1"`, true
					if mode == "lost-create" {
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"id": "temporary-native"})
					return // Extended properties are absent from Graph POST responses.
				}
				if r.URL.Path != collection+"/temporary-native" {
					t.Errorf("wrong native resource: %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if r.Method == "PATCH" {
					patches++
					if patches > 1 {
						t.Error("retried successful PATCH instead of recovering its result")
					}
					if r.Header.Get("If-Match") != `W/"v1"` {
						t.Error("missing conditional update")
					}
					var payload map[string]any
					if json.NewDecoder(r.Body).Decode(&payload) != nil {
						t.Error("invalid PATCH")
					}
					if payload["isOnlineMeeting"] != nil || payload["onlineMeetingProvider"] != nil || payload["onlineMeeting"] != nil {
						t.Error("save reallocates conferencing")
					}
					body := payload["body"].(map[string]any)["content"].(string)
					if !strings.Contains(body, join) {
						t.Error("Microsoft meeting block removed")
					}
					for key, value := range payload {
						remote[key] = value
					}
					remote["changeKey"], remote["@odata.etag"] = "v2", `W/"v2"`
					if mode == "lost-save" {
						failConfirmation = true
					}
					w.WriteHeader(204)
					return
				}
				if r.Method == "DELETE" {
					deletes++
					w.WriteHeader(204)
					return
				}
				reads++
				if failConfirmation {
					failConfirmation = false
					w.WriteHeader(503)
					return
				}
				if r.URL.Query().Get("$expand") != calendarTeamsDraftExpand() {
					t.Error("missing ownership marker read")
				}
				if patches == 0 && reads >= 2 {
					provider := "teamsForBusiness"
					if mode == "personal" {
						provider = "unknown"
					}
					remote["onlineMeetingProvider"] = provider
					remote["onlineMeeting"] = map[string]string{"joinUrl": join}
					remote["body"] = map[string]string{"contentType": "html", "content": `<a href="` + join + `">Join Teams</a>`}
				}
				json.NewEncoder(w).Encode(remote)
			}))
			defer server.Close()
			prior := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = prior }()
			preview := func(user string, want int) {
				r := calendarCreateHTTPRequest(url.Values{"source_id": {"one-source"}, "draft_id": {draftID}}, user)
				w := httptest.NewRecorder()
				h.handleCalendarTeamsDraft(w, r)
				if w.Code != want {
					t.Fatalf("preview status=%d body=%s", w.Code, w.Body.String())
				}
				if want == 200 && !strings.Contains(w.Body.String(), strings.ReplaceAll(join, "&", "\\u0026")) {
					t.Fatal("ready link missing")
				}
			}
			preview("two", 404)
			if mode == "lost-create" {
				preview("one", 502)
			}
			preview("one", 200)
			preview("one", 200)
			query := calendar.EventQuery{WindowStart: time.Now().Add(-time.Hour), WindowEnd: time.Now().Add(time.Hour)}
			page, err := listOutlookCalendarEvents(t.Context(), "token", "primary", query)
			if err != nil || len(page.Events) != 0 {
				t.Fatal("prepared event leaked into calendar sync", err)
			}
			values := calendarCreateForm()
			values.Set("teams_meeting", "true")
			values.Set("teams_draft_id", draftID)
			values.Set("guests", "guest@example.com")
			if mode == "all-day" {
				values.Set("all_day", "true")
			}
			if mode == "lost-local" {
				if _, err := h.db.Write().Exec(`CREATE TRIGGER block_calendar_create BEFORE INSERT ON calendar_events BEGIN SELECT RAISE(FAIL,'local unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			save := func(want int) {
				w := httptest.NewRecorder()
				h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
				if w.Code != want {
					t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
				}
				if want == 201 && !strings.Contains(w.Body.String(), `"teams_unconfirmed":false`) {
					t.Fatal("ready preview saved without Teams link")
				}
				if want != 201 && !strings.Contains(w.Body.String(), `"uncertain":true`) {
					t.Fatal("lost response allowed a different save identity")
				}
			}
			if mode == "lost-save" || mode == "lost-local" {
				save(expectedTeamsSaveFailure(mode))
				if mode == "lost-local" {
					h.db.Write().Exec(`DROP TRIGGER block_calendar_create`)
				}
			}
			save(201)
			save(201)
			if posts != 1 || patches != 1 || deletes != 0 {
				t.Fatalf("posts=%d patches=%d deletes=%d", posts, patches, deletes)
			}
			d, err := h.db.GetCalendarTeamsDraft(t.Context(), "one", "one-source", draftID)
			if err != nil || d.State != "saved" || d.RemoteID != "temporary-native" {
				t.Fatal("final event identity not retained", err)
			}
			page, err = listOutlookCalendarEvents(t.Context(), "token", "primary", query)
			if err != nil || len(page.Events) != 1 || page.Events[0].RemoteID != d.RemoteID {
				t.Fatal("finalized event missing from sync", err)
			}
			h.handleCalendarTeamsDraftDiscard(httptest.NewRecorder(), calendarCreateHTTPRequest(url.Values{"source_id": {"one-source"}, "draft_id": {draftID}}, "one"))
			h.runCalendarTeamsDraftCleanupTick(t.Context())
			if deletes != 0 {
				t.Fatal("saved meeting deleted by abandoned draft cleanup")
			}
			values.Set("request_id", "8c9298d4-e9d1-456e-8b48-e7e4c7929893")
			saveConflict := httptest.NewRecorder()
			h.handleCreateCalendarEvent(saveConflict, calendarCreateHTTPRequest(values, "one"))
			if saveConflict.Code != 409 {
				t.Fatal("prepared event reused for another save")
			}
		})
	}
}
func expectedTeamsSaveFailure(mode string) int {
	if mode == "lost-local" {
		return 503
	}
	return 502
}

func TestCalendarTeamsDraftCleanupRetriesAndProtectsChangedOrFinalizedEvents(t *testing.T) {
	for _, mode := range []string{"abandoned", "lost-id", "changed-marker", "guests", "version", "changed-title", "missing", "saving-private", "saving-final"} {
		t.Run(mode, func(t *testing.T) {
			h := calendarTeamsPreviewFixture(t)
			draftID := "4e12c8f0-88b4-4c30-bba4-ff1fa32db7ad"
			ctx := t.Context()
			d, err := h.db.BeginCalendarTeamsDraft(ctx, "one", "one-source", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "lost-id" {
				if err := h.db.SetCalendarTeamsDraftRemote(ctx, d, "remote"); err != nil {
					t.Fatal(err)
				}
				d.RemoteID = "remote"
			}
			if err := h.db.CompleteCalendarTeamsDraft(ctx, d, `{"provider":"teamsForBusiness","joinUrl":"https://teams.microsoft.com/meet/123"}`); err != nil {
				t.Fatal(err)
			}
			d, _ = h.db.GetCalendarTeamsDraft(ctx, "one", "one-source", draftID)
			if strings.HasPrefix(mode, "saving-") {
				if err := h.db.BindCalendarTeamsDraft(ctx, d, "create:request"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := h.db.AbandonCalendarTeamsDraft(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			remote := map[string]any{"id": "remote", "subject": "Preparing Teams meeting", "@odata.etag": `W/"v1"`, "isOrganizer": true, "attendees": []any{}, "sensitivity": "private", "showAs": "free", "isReminderOn": false, "singleValueExtendedProperties": calendarTeamsDraftProperties("draft:" + draftID)}
			switch mode {
			case "changed-marker":
				remote["singleValueExtendedProperties"] = calendarTeamsDraftProperties("different")
			case "guests":
				remote["attendees"] = []any{map[string]any{"emailAddress": map[string]string{"address": "guest@example.com"}}}
			case "version":
				remote["@odata.etag"] = "bad"
			case "changed-title":
				remote["subject"] = "User meeting"
			case "saving-final":
				remote["singleValueExtendedProperties"] = calendarTeamsDraftProperties("saved:request:hash")
				remote["subject"] = "Saved meeting"
				remote["attendees"] = []any{map[string]any{"emailAddress": map[string]string{"address": "guest@example.com"}}}
			}
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" || r.Method == "PATCH" {
					t.Error("cleanup allocated or changed an event")
					w.WriteHeader(500)
					return
				}
				if r.Method == "DELETE" {
					deletes++
					if r.Header.Get("If-Match") != `W/"v1"` {
						t.Error("unconditional cleanup")
					}
					if deletes == 1 {
						w.WriteHeader(503)
					} else {
						w.WriteHeader(204)
					}
					return
				}
				if mode == "missing" {
					w.WriteHeader(404)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/events") {
					json.NewEncoder(w).Encode(map[string]any{"value": []any{remote}})
					return
				}
				json.NewEncoder(w).Encode(remote)
			}))
			defer server.Close()
			prior := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = prior }()
			h.runCalendarTeamsDraftCleanupTick(ctx)
			h.runCalendarTeamsDraftCleanupTick(ctx)
			cleaned := mode == "abandoned" || mode == "lost-id" || mode == "missing" || mode == "saving-private"
			got, _ := h.db.GetCalendarTeamsDraft(ctx, "one", "one-source", draftID)
			if cleaned && got.State != "cleaned" {
				t.Fatalf("cleanup state=%s", got.State)
			}
			if (mode == "abandoned" || mode == "lost-id" || mode == "saving-private") && deletes != 2 {
				t.Fatalf("cleanup retries=%d", deletes)
			}
			if !cleaned && deletes != 0 {
				t.Fatal("modified or finalized event deleted")
			}
			if mode == "saving-final" {
				if got.State != "saved" {
					t.Fatal("lost finalized save not retained")
				}
				if err := h.db.BindCalendarTeamsDraft(ctx, got, "create:request"); err != nil {
					t.Fatal("worker recovery blocked safe save retry", err)
				}
			}
		})
	}
}

func TestCalendarTeamsDiscardBeforePreparationNeverCreatesEvent(t *testing.T) {
	h := calendarTeamsPreviewFixture(t)
	values := url.Values{"source_id": {"one-source"}, "draft_id": {"e45d9c4c-971f-4bff-b7f6-ac3babf30db6"}}
	w := httptest.NewRecorder()
	h.handleCalendarTeamsDraftDiscard(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 204 {
		t.Fatal("discard failed")
	}
	w = httptest.NewRecorder()
	h.handleCalendarTeamsDraft(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 409 {
		t.Fatal("late preparation bypassed discard tombstone")
	}
}
