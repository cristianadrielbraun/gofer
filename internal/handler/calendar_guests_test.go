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
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarGuestValidation(t *testing.T) {
	values := calendarCreateForm()
	values.Set("guests", `"Last, First" <GUEST@example.com>, guest@example.com, Other <other@example.com>, `)
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil || !draft.GuestsSet || len(draft.Guests) != 2 || draft.Guests[0].Email != "guest@example.com" || draft.Guests[0].Name != "Last, First" {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	for _, raw := range []string{"not-an-address", "guest@example.com\r\nBcc: leak@example.com", "guest@example.com; other@example.com"} {
		values.Set("guests", raw)
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
			t.Fatalf("accepted invalid guest: %q", raw)
		}
	}
	values.Set("guests", "guest@example.com")
	values.Set("repeat_frequency", "weekly")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Fatal("accepted recurring invitations")
	}
	if calendarGuestLastSeparator(`"Last, First" <guest@example.com>, next`) != strings.LastIndex(`"Last, First" <guest@example.com>, next`, ",") {
		t.Fatal("quoted guest names split incorrectly")
	}
}

func TestCalendarGuestSuggestionsAreScopedAndEscaped(t *testing.T) {
	h := calendarCreateFixture(t)
	for _, user := range []string{"one", "two"} {
		if _, err := h.db.SaveContact(t.Context(), user, models.Contact{Name: "Guest <script>", Email: user + "-guest@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	r := calendarCreateHTTPRequest(nil, "one")
	r.URL.RawQuery = "guests=guest"
	w := httptest.NewRecorder()
	h.handleCalendarGuestSuggestions(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "one-guest@example.com") || strings.Contains(w.Body.String(), "two-guest@example.com") || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("suggestions leaked or unescaped: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("guest suggestions are cacheable")
	}
}

func TestCalendarGuestProvidersCreateUpdateCancel(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			draft := calendarProviderDraft(t, false)
			draft.GuestsSet, draft.OrganizerEmail = true, "one@example.com"
			draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com", Name: "Guest"}}
			var stored map[string]any
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				writes++
				if provider == "gmail" && r.URL.Query().Get("sendUpdates") != "all" {
					t.Error("Google guest notifications not requested")
				}
				if r.Method == "DELETE" {
					if r.Header.Get("If-Match") != `"v2"` {
						t.Error("delete lost conditional version")
					}
					w.WriteHeader(204)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.Method == "POST" {
					stored = payload
					if provider == "gmail" {
						stored["organizer"] = map[string]any{"email": "one@example.com", "self": true}
						stored["etag"] = `"v1"`
					} else {
						if payload["responseRequested"] != true {
							t.Error("Graph response not requested")
						}
						stored["id"], stored["changeKey"], stored["@odata.etag"], stored["isOrganizer"] = "graph-id", "key1", `"v1"`, true
						stored["organizer"] = map[string]any{"emailAddress": map[string]string{"address": "one@example.com"}}
					}
					person := stored["attendees"].([]any)[0].(map[string]any)
					if provider == "gmail" {
						person["responseStatus"] = "accepted"
					} else {
						person["status"] = map[string]string{"response": "accepted"}
					}
					w.WriteHeader(201)
				} else {
					if r.Header.Get("If-Match") != `"v1"` {
						t.Error("update lost conditional version")
					}
					for key, value := range payload {
						stored[key] = value
					}
					if provider == "gmail" {
						stored["etag"] = `"v2"`
					} else {
						stored["changeKey"], stored["@odata.etag"] = "key2", `"v2"`
					}
					people := payload["attendees"].([]any)
					if len(people) != 2 {
						t.Error("guest additions not sent")
					}
					first := people[0].(map[string]any)
					if provider == "gmail" && first["responseStatus"] != "accepted" {
						t.Error("existing RSVP overwritten")
					}
					if provider == "outlook" && first["status"].(map[string]any)["response"] != "accepted" {
						t.Error("existing RSVP overwritten")
					}
				}
				_ = json.NewEncoder(w).Encode(stored)
			}))
			defer server.Close()
			priorGoogle, priorGraph := googleCalendarAPIBaseURL, outlookGraphBaseURL
			googleCalendarAPIBaseURL, outlookGraphBaseURL = server.URL, server.URL
			defer func() { googleCalendarAPIBaseURL, outlookGraphBaseURL = priorGoogle, priorGraph }()
			var event calendar.RemoteEvent
			var err error
			if provider == "gmail" {
				event, err = createGoogleCalendarEvent(t.Context(), "token", "primary", draft)
			} else {
				event, err = createOutlookCalendarEvent(t.Context(), "token", "primary", draft)
			}
			if err != nil || event.ResponseStatus != "organizer" {
				t.Fatalf("create=%#v err=%v", event, err)
			}
			existing := calendarStorageEvent("one", "one-source", event)
			draft.Guests = append(draft.Guests, calendar.GuestDraft{Email: "new@example.com"})
			if provider == "gmail" {
				event, err = updateGoogleCalendarEvent(t.Context(), "token", "primary", existing, draft)
			} else {
				event, err = updateOutlookCalendarEvent(t.Context(), "token", "primary", existing, draft)
			}
			if err != nil || !calendarMeetingGuestsMatch(event.Attendees, draft.Guests, draft.OrganizerEmail) {
				t.Fatalf("update=%#v err=%v", event, err)
			}
			existing = calendarStorageEvent("one", "one-source", event)
			if provider == "gmail" {
				err = deleteGoogleCalendarEvent(t.Context(), "token", "primary", existing)
			} else {
				err = deleteOutlookCalendarEvent(t.Context(), "token", "primary", existing)
			}
			if err != nil || writes != 3 {
				t.Fatalf("delete err=%v writes=%d", err, writes)
			}
			// A stale organizer cache must not authorize a received invitation.
			if provider == "gmail" {
				stored["organizer"].(map[string]any)["self"] = false
			} else {
				stored["isOrganizer"] = false
			}
			if provider == "gmail" {
				_, err = updateGoogleCalendarEvent(t.Context(), "token", "primary", existing, draft)
			} else {
				_, err = updateOutlookCalendarEvent(t.Context(), "token", "primary", existing, draft)
			}
			if !errors.Is(err, errCalendarUpdateUnsupported) || writes != 3 {
				t.Fatalf("received invitation reached write: %v", err)
			}
			if provider == "gmail" {
				err = deleteGoogleCalendarEvent(t.Context(), "token", "primary", existing)
			} else {
				err = deleteOutlookCalendarEvent(t.Context(), "token", "primary", existing)
			}
			if !errors.Is(err, errCalendarUpdateUnsupported) || writes != 3 {
				t.Fatalf("received invitation reached delete: %v", err)
			}
		})
	}
}

func TestCalendarGuestCalDAVSchedulingAndDurableFallback(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			h := calendarCreateFixture(t)
			body, etag := "", `"v1"`
			deleted, writes := false, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "user" || password != "pass" {
					t.Error("DAV identity missing")
				}
				switch r.Method {
				case "OPTIONS":
					if automatic {
						w.Header().Set("DAV", "calendar-access, calendar-auto-schedule")
					}
				case "PROPFIND":
					props := `<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>`
					if r.URL.Path == "/principal/" {
						props = `<c:calendar-user-address-set><d:href>mailto:one@example.com</d:href></c:calendar-user-address-set>`
					}
					w.WriteHeader(207)
					fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, props)
				case "PUT":
					if r.Header.Get("If-None-Match") == "*" && body != "" {
						w.WriteHeader(412)
						return
					}
					if writes == 0 {
						if r.Header.Get("If-None-Match") != "*" {
							t.Error("unsafe create")
						}
					} else if r.Header.Get("If-Match") != etag {
						t.Error("unsafe meeting update")
					}
					writes++
					data, _ := io.ReadAll(r.Body)
					body = string(data)
					etag = fmt.Sprintf(`"v%d"`, writes)
					w.Header().Set("ETag", etag)
					if writes == 1 {
						w.WriteHeader(201)
					} else {
						w.WriteHeader(204)
					}
				case "GET":
					if deleted || body == "" {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("ETag", etag)
					io.WriteString(w, body)
				case "DELETE":
					if r.Header.Get("If-Match") != etag {
						t.Error("unsafe cancellation")
					}
					deleted = true
					writes++
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer server.Close()
			prior := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			defer func() { calDAVHTTPTransport = prior }()
			source := storage.CalendarSource{ID: "one-source", UserID: "one", AccountID: "one-account", Provider: "caldav", RemoteID: server.URL + "/cal/", AccessRole: "owner", IsSelected: true}
			if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='imap',smtp_host='smtp.example.com',smtp_port=587 WHERE id='one-account'; UPDATE calendar_sources SET provider='caldav',remote_id=?,access_role='owner' WHERE id='one-source'`, source.RemoteID); err != nil {
				t.Fatal(err)
			}
			var err error
			h.accountStore, err = config.NewAccountStore(h.db, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.accountStore.SaveCalDAVConfig(t.Context(), "one", "one-account", source.RemoteID, "user", "pass", false); err != nil {
				t.Fatal(err)
			}
			credentials := calendarCredentials{username: "user", password: "pass", baseURL: source.RemoteID}
			draft := calendarProviderDraft(t, false)
			draft.GuestsSet = true
			draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}}
			if !automatic {
				if _, err := h.db.Write().Exec(`UPDATE accounts SET smtp_host='' WHERE id='one-account'`); err != nil {
					t.Fatal(err)
				}
				if _, err := h.createCalDAVMeeting(t.Context(), source, credentials, draft); err == nil || calendarCreateUncertain(err) || writes != 0 {
					t.Fatalf("missing SMTP reached a write or was reported uncertain: %v", err)
				}
				if _, err := h.db.Write().Exec(`UPDATE accounts SET smtp_host='smtp.example.com' WHERE id='one-account'`); err != nil {
					t.Fatal(err)
				}
			}
			event, err := h.createCalDAVMeeting(t.Context(), source, credentials, draft)
			if err != nil || event.ResponseStatus != "organizer" {
				t.Fatalf("create=%#v err=%v", event, err)
			}
			readSend := func(method string) storage.OutgoingSend {
				var id string
				if err := h.db.Read().QueryRow(`SELECT id FROM outgoing_sends WHERE json_extract(message_json,'$.calendar_notification.Method')=? AND (? != 'CANCEL' OR json_extract(message_json,'$.calendar_notification.Deleted')=1) ORDER BY created_at DESC,id DESC LIMIT 1`, method, method).Scan(&id); err != nil {
					t.Fatal(err)
				}
				send, err := h.db.GetOutgoingSend(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				return send
			}
			checkSend := func(send storage.OutgoingSend, want bool) {
				var snapshot outgoingMessageSnapshot
				if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
					t.Fatal(err)
				}
				unlock, err := h.beforeCalendarNotificationSend(t.Context(), send, snapshot.outgoingMessage())
				unlock()
				if (err == nil) != want {
					t.Fatalf("send allowed=%v want=%v err=%v", err == nil, want, err)
				}
			}
			if !automatic {
				send := readSend("REQUEST")
				checkSend(send, true)
				createdBody := body
				body = ""
				checkSend(send, false) // no notification before confirmed DAV save
				body = createdBody
				if _, err := h.createCalDAVMeeting(t.Context(), source, credentials, draft); err != nil {
					t.Fatal(err)
				}
				var count int
				_ = h.db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&count)
				if count != 1 {
					t.Fatalf("duplicate notification rows=%d", count)
				}
			}
			// Editing keeps a guest's answer and private reminder on the resource.
			body = strings.Replace(body, "PARTSTAT=NEEDS-ACTION", "PARTSTAT=ACCEPTED", 1)
			body = strings.Replace(body, "END:VEVENT", "X-PRIVATE:keep\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nDESCRIPTION:Private\r\nEND:VALARM\r\nEND:VEVENT", 1)
			existing := calendarStorageEvent("one", "one-source", event)
			// A concurrent provider edit must stop before staging another email.
			stale := existing
			stale.ETag = `"outdated"`
			if _, err := h.updateCalDAVMeeting(t.Context(), source, credentials, stale, draft); !errors.Is(err, errCalendarUpdateConflict) || writes != 1 {
				t.Fatalf("stale meeting write: %v", err)
			}
			draft.Summary = "Updated meeting"
			draft.Guests = append(draft.Guests, calendar.GuestDraft{Email: "new@example.com"})
			event, err = h.updateCalDAVMeeting(t.Context(), source, credentials, existing, draft)
			if err != nil || !strings.Contains(body, "PARTSTAT=ACCEPTED") || !strings.Contains(body, "X-PRIVATE:keep") || !strings.Contains(body, "BEGIN:VALARM") {
				t.Fatalf("update err=%v body=%s", err, body)
			}
			if !automatic {
				rows, err := h.db.Read().Query(`SELECT id FROM outgoing_sends`)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for rows.Next() {
					var id string
					_ = rows.Scan(&id)
					ids = append(ids, id)
				}
				rows.Close()
				for _, id := range ids {
					send, _ := h.db.GetOutgoingSend(t.Context(), id)
					var snapshot outgoingMessageSnapshot
					_ = json.Unmarshal(send.MessageJSON, &snapshot)
					cal, _ := calendarUpdateDecodeICS([]byte(snapshot.CalendarNotification.Calendar))
					sequence, _ := cal.Events()[0].Props.Text("SEQUENCE")
					checkSend(send, sequence == "1") // stale invitation cannot follow a newer update
					if strings.Contains(snapshot.CalendarNotification.Calendar, "X-PRIVATE") || strings.Contains(snapshot.CalendarNotification.Calendar, "VALARM") {
						t.Fatal("private metadata leaked to guests")
					}
				}
			}
			existing = calendarStorageEvent("one", "one-source", event)
			draft.Guests = []calendar.GuestDraft{{Email: "new@example.com"}}
			event, err = h.updateCalDAVMeeting(t.Context(), source, credentials, existing, draft)
			if err != nil {
				t.Fatal(err)
			}
			if !automatic {
				var id string
				if err := h.db.Read().QueryRow(`SELECT id FROM outgoing_sends WHERE json_extract(message_json,'$.calendar_notification.Method')='CANCEL' AND json_extract(message_json,'$.calendar_notification.Deleted')=0`).Scan(&id); err != nil {
					t.Fatal(err)
				}
				send, _ := h.db.GetOutgoingSend(t.Context(), id)
				if len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != "guest@example.com" {
					t.Fatal("removed guest cancellation had wrong recipients")
				}
				checkSend(send, true)
			}
			existing = calendarStorageEvent("one", "one-source", event)
			if err := h.deleteCalDAVMeeting(t.Context(), source, credentials, existing); err != nil {
				t.Fatal(err)
			}
			if !automatic {
				send := readSend("CANCEL")
				checkSend(send, true)
				deleted = false
				checkSend(send, false)
			}
			var count int
			_ = h.db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&count)
			if automatic && count != 0 {
				t.Fatal("auto scheduling also queued SMTP invitations")
			}
		})
	}
}

func TestCalendarMeetingSafetyChecks(t *testing.T) {
	cal, err := calendarUpdateDecodeICS([]byte(replyCalendar(replyEvent("DTSTART:20261002T070000Z\nDTEND:20261002T080000Z", ""))))
	if err != nil {
		t.Fatal(err)
	}
	if calendarMeetingCalDAVShape(cal, http.Header{}, "guest@example.com") == nil {
		t.Fatal("non-organizer meeting write accepted")
	}
	if err := calendarMeetingCalDAVShape(cal, http.Header{}, "host@example.com"); err != nil {
		t.Fatal(err)
	}
	cal.Events()[0].Props.Get("ATTENDEE").Params.Set("DELEGATED-TO", "mailto:other@example.com")
	if calendarMeetingCalDAVShape(cal, http.Header{}, "host@example.com") == nil {
		t.Fatal("delegated meeting accepted")
	}
	delete(cal.Events()[0].Props.Get("ATTENDEE").Params, "DELEGATED-TO")
	cal.Events()[0].Props.SetText("RRULE", "FREQ=DAILY")
	if calendarMeetingCalDAVShape(cal, http.Header{}, "host@example.com") == nil {
		t.Fatal("recurring meeting accepted")
	}
}
