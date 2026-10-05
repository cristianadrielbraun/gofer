package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestGoogleMeetPreviewBeforeSaveRetainsSameLinkAndReplays(t *testing.T) {
	for _, signature := range []bool{true, false} {
		t.Run(fmt.Sprintf("signature=%t", signature), func(t *testing.T) {
			testGoogleMeetPreviewBeforeSave(t, signature)
		})
	}
}

func testGoogleMeetPreviewBeforeSave(t *testing.T, signature bool) {
	conference := testGoogleMeet
	if !signature {
		conference = strings.Replace(conference, `,"signature":"keep-me"`, "", 1)
	}
	conference = strings.TrimSuffix(conference, "}") + `,"createRequest":{"requestId":"original","status":{"statusCode":"success"}}}`
	h := calendarCreateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider_account_id='google-subject' WHERE id='one-account'`); err != nil {
		t.Fatal(err)
	}
	h.mailboxAuth = mailauth.New(&mailauth.Config{}, h.db, testMailboxCredentialKey)
	expires := time.Now().Add(time.Hour)
	if err := h.mailboxAuth.UpsertOAuthAccount(t.Context(), "one-account", providers.OAuthGoogle, "google-subject", "token", "refresh", "Bearer", &expires, mailauth.GoogleCalendarEventsScope); err != nil {
		t.Fatal(err)
	}
	draftID := "9855dbe3-185a-4151-bd3b-099b59d3c154"
	remoteID := calendarMeetDraftRemoteID("one", "one-source", draftID)
	var temporary map[string]any
	posts, deletes, polls, eventPosts := 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("wrong credentials")
		}
		if strings.Contains(r.URL.Path, "/calendarList/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "primary", "accessRole": "owner", "conferenceProperties": map[string]any{"allowedConferenceSolutionTypes": []string{"hangoutsMeet"}}})
			return
		}
		if r.Method == "POST" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if r.URL.Query().Get("conferenceDataVersion") != "1" {
				t.Error("missing conferencing version")
			}
			if payload["id"] == remoteID {
				posts++
				if r.URL.Query().Get("sendUpdates") != "none" || payload["attendees"] != nil || payload["visibility"] != "private" || payload["transparency"] != "transparent" {
					t.Errorf("temporary event not silent/private: %v", payload)
				}
				reminders := payload["reminders"].(map[string]any)
				if reminders["useDefault"] != false || len(reminders["overrides"].([]any)) != 0 {
					t.Error("draft can trigger reminders")
				}
				temporary = payload
				temporary["etag"] = `"temp-v1"`
				temporary["conferenceData"] = json.RawMessage(testGoogleMeetPending)
				_ = json.NewEncoder(w).Encode(temporary)
				return
			}
			eventPosts++
			data := payload["conferenceData"].(map[string]any)
			if (signature && data["signature"] != "keep-me") || (!signature && data["signature"] != nil) || data["conferenceId"] != "abc-defg-hij" || data["createRequest"] != nil {
				t.Errorf("save allocated a different meeting: %v", data)
			}
			payload["etag"], payload["iCalUID"] = `"event-v1"`, "uid"
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		if r.Method == "DELETE" {
			deletes++
			if deletes == 1 {
				w.WriteHeader(503)
				return
			}
			if r.URL.Path != "/calendars/primary/events/"+remoteID || r.Header.Get("If-Match") != `"temp-v1"` || r.URL.Query().Get("sendUpdates") != "none" {
				t.Error("unsafe draft cleanup")
			}
			temporary = nil
			w.WriteHeader(204)
			return
		}
		if temporary == nil {
			w.WriteHeader(404)
			return
		}
		polls++
		temporary["conferenceData"] = json.RawMessage(conference)
		_ = json.NewEncoder(w).Encode(temporary)
	}))
	defer server.Close()
	prior := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	defer func() { googleCalendarAPIBaseURL = prior }()
	prepare := func(user string, want int) {
		r := httptest.NewRequest("POST", "/api/calendar/google-meet/drafts", strings.NewReader(url.Values{"source_id": {"one-source"}, "draft_id": {draftID}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
		w := httptest.NewRecorder()
		h.handleCalendarGoogleMeetDraft(w, r)
		if w.Code != want {
			t.Fatalf("preview status=%d body=%s", w.Code, w.Body.String())
		}
		if want == 200 && (!strings.Contains(w.Body.String(), "https://meet.google.com/abc-defg-hij") || strings.Contains(w.Body.String(), "signature")) {
			t.Fatal("missing preview or leaked signed conference")
		}
	}
	prepare("two", 404)
	prepare("one", 200)
	if pending, err := h.db.ListCalendarMeetDraftCleanup(t.Context()); err != nil || len(pending) != 1 {
		t.Fatal("failed cleanup not retained")
	}
	h.runCalendarMeetDraftCleanupTick(t.Context())
	prepare("one", 200) // Lost browser response reuses the persisted, cleaned draft.
	if posts != 1 || deletes != 2 || polls < 1 || eventPosts != 0 {
		t.Fatalf("posts=%d deletes=%d polls=%d saves=%d", posts, deletes, polls, eventPosts)
	}
	var count int
	if err := h.db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("preview created a visible event")
	}
	values := calendarCreateForm()
	values.Set("google_meet_meeting", "true")
	values.Set("google_meet_draft_id", draftID)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
		if w.Code != 201 || !strings.Contains(w.Body.String(), `"google_meet_unconfirmed":false`) {
			t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if eventPosts != 1 {
		t.Fatal("save replay created another event")
	}
	prepare("one", 409)
	values.Set("request_id", "c1c1dbe3-185a-4151-bd3b-099b59d3c154")
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 409 || eventPosts != 1 {
		t.Fatal("conference reused across separate events")
	}
}

func TestGoogleMeetDraftCleanupProtectsUnrelatedEventsAndHandlesTombstones(t *testing.T) {
	for _, mode := range []string{"changed", "weak-version", "tombstone", "missing", "gone", "ready"} {
		t.Run(mode, func(t *testing.T) {
			d := storage.CalendarMeetDraft{UserID: "one", SourceID: "source", DraftID: "draft", RemoteID: "remote"}
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					deletes++
					w.WriteHeader(204)
					return
				}
				if mode == "missing" {
					w.WriteHeader(404)
					return
				}
				if mode == "gone" {
					w.WriteHeader(410)
					return
				}
				remote := map[string]any{"id": "remote", "etag": `"v1"`, "extendedProperties": map[string]any{"private": map[string]string{"goferMeetDraft": "true", "goferMeetDraftID": "draft"}}}
				if mode == "changed" {
					delete(remote, "extendedProperties")
				}
				if mode == "weak-version" {
					remote["etag"] = `W/"v1"`
				}
				if mode == "tombstone" {
					remote = map[string]any{"id": "remote", "status": "cancelled"}
				}
				_ = json.NewEncoder(w).Encode(remote)
			}))
			defer server.Close()
			prior := googleCalendarAPIBaseURL
			googleCalendarAPIBaseURL = server.URL
			defer func() { googleCalendarAPIBaseURL = prior }()
			err := cleanupGoogleMeetDraft(t.Context(), storage.CalendarSource{RemoteID: "primary"}, d, "token")
			safe := mode != "changed" && mode != "weak-version"
			if (err == nil) != safe || (deletes == 1) != (mode == "ready") {
				t.Fatalf("err=%v deletes=%d", err, deletes)
			}
		})
	}
}

func TestGoogleMeetDraftAcceptsReadyConferenceAndPreservesSignatureWhenPresent(t *testing.T) {
	remote := googleCalendarEvent{ConferenceData: json.RawMessage(testGoogleMeet)}
	signed := calendarMeetDraftConference(remote)
	if !calendarGoogleMeetConfirmed(signed) || !strings.Contains(string(signed), "keep-me") {
		t.Fatal("lost signed fields")
	}
	remote.ConferenceData = json.RawMessage(strings.Replace(testGoogleMeet, `,"signature":"keep-me"`, "", 1))
	if !calendarGoogleMeetConfirmed(calendarMeetDraftConference(remote)) {
		t.Fatal("ready Meet conference without signature rejected")
	}
	ready := string(remote.ConferenceData)
	for _, status := range []string{"pending", "failure", ""} {
		remote.ConferenceData = json.RawMessage(strings.TrimSuffix(ready, "}") + `,"createRequest":{"status":{"statusCode":"` + status + `"}}}`)
		if len(calendarMeetDraftConference(remote)) != 0 {
			t.Fatalf("incomplete conference with link accepted: %q", status)
		}
	}
	remote.ConferenceData = json.RawMessage(strings.TrimSuffix(ready, "}") + `,"createRequest":{"requestId":"original","status":{"statusCode":"success"}}}`)
	if data := calendarMeetDraftConference(remote); !calendarGoogleMeetConfirmed(data) || strings.Contains(string(data), "createRequest") {
		t.Fatal("successful unsigned conference not prepared for copying")
	}
	remote.ConferenceData = json.RawMessage(testGoogleMeetPending)
	if len(calendarMeetDraftConference(remote)) != 0 {
		t.Fatal("pending conference accepted")
	}
}

func TestGoogleMeetPreviewUpdateLoadsOnlyOwnedReadyConference(t *testing.T) {
	h := calendarUpdateFixture(t)
	draftID := "9855dbe3-185a-4151-bd3b-099b59d3c154"
	d, err := h.db.BeginCalendarMeetDraft(t.Context(), "one", "one-source", draftID, "remote")
	if err != nil {
		t.Fatal(err)
	}
	values := calendarCreateForm()
	values.Set("google_meet_meeting", "true")
	values.Set("google_meet_draft_id", draftID)
	writes := 0
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		writes++
		if string(draft.GoogleMeetConference) != testGoogleMeet {
			t.Error("untrusted conference attached")
		}
		remote := calendarCreatedRemote(draft)
		remote.RemoteID = existing.RemoteID
		remote.ICalUID = existing.ICalUID
		remote.ETag = `"v2"`
		remote.ResponseStatus = "organizer"
		remote.Attendees = json.RawMessage(`[]`)
		remote.OnlineMeeting = draft.GoogleMeetConference
		return remote, nil
	}
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 409 || writes != 0 {
		t.Fatalf("pending preview accepted: %d %s", w.Code, w.Body.String())
	}
	if err := h.db.CompleteCalendarMeetDraft(t.Context(), d, testGoogleMeet); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 200 || writes != 1 {
		t.Fatalf("ready preview denied: %d %s", w.Code, w.Body.String())
	}
}

func TestGoogleMeetDraftPreparationResumesAfterDisconnect(t *testing.T) {
	d := storage.CalendarMeetDraft{UserID: "one", SourceID: "source", DraftID: "draft", RemoteID: "remote"}
	posts := 0
	created := false
	ready := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !created && r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		if r.Method == "POST" {
			posts++
			created = true
		}
		conference := testGoogleMeetPending
		if ready {
			conference = testGoogleMeet
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "remote", "conferenceData": json.RawMessage(conference), "extendedProperties": map[string]any{"private": map[string]string{"goferMeetDraft": "true", "goferMeetDraftID": "draft"}}})
	}))
	defer server.Close()
	prior := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	defer func() { googleCalendarAPIBaseURL = prior }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	source := storage.CalendarSource{RemoteID: "primary"}
	if _, err := prepareGoogleMeetDraft(ctx, source, d, "token"); err == nil {
		t.Fatal("pending preparation ignored disconnect deadline")
	}
	ready = true
	data, err := prepareGoogleMeetDraft(t.Context(), source, d, "token")
	if err != nil || !calendarGoogleMeetConfirmed(data) || posts != 1 {
		t.Fatalf("resume allocated another conference: %s err=%v posts=%d", data, err, posts)
	}
}

func TestGoogleMeetTemporaryDraftExcludedFromCalendarSync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"id": "temporary", "extendedProperties": map[string]any{"private": map[string]string{"goferMeetDraft": "true"}}},
			map[string]any{"id": "real", "start": map[string]string{"date": "2026-10-05"}, "end": map[string]string{"date": "2026-10-06"}},
		}})
	}))
	defer server.Close()
	prior := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	defer func() { googleCalendarAPIBaseURL = prior }()
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	page, err := listGoogleCalendarEvents(t.Context(), "token", "primary", calendar.EventQuery{WindowStart: start, WindowEnd: start.Add(24 * time.Hour)})
	if err != nil || len(page.Events) != 1 || page.Events[0].RemoteID != "real" {
		t.Fatalf("temporary preview appears as an appointment: %#v %v", page, err)
	}
}
