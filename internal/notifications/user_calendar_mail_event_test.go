package notifications

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedMailCalendarTransport struct {
	base     http.RoundTripper
	provider http.RoundTripper
	target   *url.URL
}

func (t ownedMailCalendarTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "gmail.googleapis.com" {
		return t.base.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	address.Path = strings.TrimPrefix(address.Path, "/gmail/v1")
	copy.URL = &address
	return t.provider.RoundTrip(copy)
}

func ownedMailCalendarMIME(owner, address string) string {
	return "Message-ID: <" + owner + "-invitation@example.com>\r\nSubject: Invitation\r\nFrom: Organizer <organizer@example.com>\r\nTo: " + address + "\r\nMIME-Version: 1.0\r\nContent-Type: text/calendar; method=REQUEST; charset=utf-8\r\n\r\nBEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:" + owner + " mail invitation\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:" + address + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func newOwnedMailCalendarFixture(t *testing.T, provider string, cached bool, before func(*http.Request)) (*userStorageFixture, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	f, _, server := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				base.ServeHTTP(w, r)
				return
			}
			owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
			path := "/users/me/messages/m1"
			if provider == "outlook" {
				path = "/me/messages/m1/$value"
			}
			if (owner != "alice" && owner != "bob") || r.Method != "GET" || r.URL.Path != path {
				t.Error("unexpected MIME request", owner, r.Method, r.URL.Path)
				http.Error(w, "unexpected", 400)
				return
			}
			if provider == "gmail" && r.URL.Query().Get("format") != "raw" {
				t.Error("not raw Gmail retrieval")
			}
			if provider == "outlook" && r.Header.Get("Prefer") != `IdType="ImmutableId"` {
				t.Error("missing immutable ID")
			}
			calls.Add(1)
			if before != nil {
				before(r)
			}
			if r.Context().Err() != nil {
				return
			}
			raw := ownedMailCalendarMIME(owner, owner+"@provider.test")
			if provider == "gmail" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"id": "m1", "raw": base64.RawURLEncoding.EncodeToString([]byte(raw))})
			} else {
				_, _ = fmt.Fprint(w, raw)
			}
		})
	})
	previous := http.DefaultTransport
	target, _ := url.Parse(server.URL)
	http.DefaultTransport = ownedMailCalendarTransport{base: previous, provider: server.Client().Transport, target: target}
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		address := owner + "@provider.test"
		if provider == "caldav" {
			address = owner + "@example.com"
		}
		path := ""
		if cached {
			var err error
			path, err = f.blobs.StoreRaw(t.Context(), f.accounts[owner].ID, 1, []byte(ownedMailCalendarMIME(owner, address)))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(_ *config.AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,name,role,uid_validity) VALUES(?,?,'INBOX','Inbox','inbox',100)`, owner+"-calendar-mail", f.accounts[owner].ID); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,remote_message_id,internet_message_id,subject,raw_path,body_text_path) VALUES(1,?,'m1',?,'cached body metadata',?,'existing-body')`, f.accounts[owner].ID, "<"+owner+"-invitation@example.com>", path); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(1,?,2)`, owner+"-calendar-mail"); err != nil {
				return err
			}
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			remote := "invitation"
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/invitation.ics"
			}
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", ICalUID: "private-uid", RemoteID: remote, ETag: "version", Summary: owner + " current invitation", OrganizerEmail: "organizer@example.com", ResponseStatus: "needsAction", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, calls
}

func TestUserMailCalendarHTTPIsolationAndRawOnlyRecovery(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, cached := range []bool{true, false} {
			if provider == "caldav" && !cached {
				continue
			}
			t.Run(fmt.Sprintf("%s/cached=%v", provider, cached), func(t *testing.T) {
				f, calls := newOwnedMailCalendarFixture(t, provider, cached, nil)
				for _, owner := range []string{"alice", "bob"} {
					other := map[string]string{"alice": "bob", "bob": "alice"}[owner]
					r := f.request(owner, "GET", "/api/mail/1/calendar?user_id="+other+"&account_id="+f.accounts[other].ID, "")
					if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Body.String(), owner+" current invitation") || strings.Contains(r.Body.String(), other+" current invitation") || !strings.Contains(r.Body.String(), `data-calendar-response-ready="false"`) || !strings.Contains(r.Body.String(), `mail_id=1`) {
						t.Fatal("owned invitation footer", provider, owner, r.Code, r.Body.String())
					}
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var subject, body, raw string
						err := db.Read().QueryRow(`SELECT subject,body_text_path,raw_path FROM messages WHERE id=1`).Scan(&subject, &body, &raw)
						if err == nil && (subject != "cached body metadata" || body != "existing-body" || raw == "") {
							return fmt.Errorf("raw retrieval rewrote body: %q %q %q", subject, body, raw)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if r := f.request(owner, "GET", "/api/mail/1/calendar", ""); r.Code != 200 {
						t.Fatal("cached repeat", r.Code)
					}
				}
				want := int32(0)
				if !cached {
					want = 2
				}
				if calls.Load() != want {
					t.Fatal("duplicate HTTP", calls.Load(), want)
				}
				for _, id := range []string{"unknown", "0", "-1", "999"} {
					if r := f.request("alice", "GET", "/api/mail/"+id+"/calendar", ""); r.Code != 204 || r.Body.Len() != 0 {
						t.Fatal("missing mail fallback", id, r.Code)
					}
				}
				var count int
				if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 0 {
					t.Fatal("central mail fallback", count, err)
				}
			})
		}
	}
}

func TestUserMailCalendarHTTPProviderWaitRejectsChangesAndFreesStore(t *testing.T) {
	for _, mode := range []string{"message", "reconnect", "deleting", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			f, calls := newOwnedMailCalendarFixture(t, "outlook", false, func(r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer alice-access" {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.request("alice", "GET", "/api/mail/1/calendar", "") }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("MIME request did not run")
			}
			if r := f.request("bob", "GET", "/api/mail/1/calendar", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "bob current invitation") {
				t.Fatal("store held across MIME HTTP", r.Code, r.Body.String())
			}
			switch mode {
			case "message":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET remote_message_id='new' WHERE id=1`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "reconnect":
				expiry := time.Now().Add(time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "reconnected", "new-refresh", "Bearer", &expiry, userOutlookHTTPScopes); err != nil {
					t.Fatal(err)
				}
			case "deleting":
				if _, err := f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			case "root-stop":
				f.stopIMAP()
			}
			unblock()
			select {
			case r := <-done:
				if r.Code != 204 || r.Body.Len() != 0 {
					t.Fatal("stale invitation rendered", mode, r.Code, r.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("late request did not finish")
			}
			if calls.Load() != 2 {
				t.Fatal("MIME replay", calls.Load())
			}
			if mode != "deleting" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var path string
					err := db.Read().QueryRow(`SELECT COALESCE(raw_path,'') FROM messages WHERE id=1`).Scan(&path)
					if err == nil && path != "" {
						return fmt.Errorf("late raw path published %s", path)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserMailCalendarHTTPRenderReleasesSoleStore(t *testing.T) {
	f, _ := newOwnedMailCalendarFixture(t, "caldav", true, nil)
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(w.release) }) }
	defer unblock()
	req := httptest.NewRequest("GET", "/api/mail/1/calendar", nil).WithContext(t.Context())
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(w, req); close(done) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarEvent(ctx, "bob", "same-event"); return err }); err != nil {
		t.Fatal("render pinned sole store", err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not finish")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice current invitation") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestUserMailCalendarHTTPReadGrantDoesNotOfferResponse(t *testing.T) {
	f, calls := newOwnedMailCalendarFixture(t, "outlook", true, nil)
	expiry := time.Now().Add(time.Hour)
	if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "cached", "refresh", "Bearer", &expiry, "https://graph.microsoft.com/Calendars.Read"); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/mail/1/calendar", "")
	if r.Code != 200 || strings.Contains(r.Body.String(), "data-calendar-response-form") || !strings.Contains(r.Body.String(), "Calendar write access") {
		t.Fatal("read grant offered write action", r.Code, r.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("cached metadata refreshed credentials")
	}
}

func TestUserMailCalendarHTTPIMAPRecoveryAndUIDValidity(t *testing.T) {
	for _, mode := range []string{"recover", "validity-change"} {
		t.Run(mode, func(t *testing.T) {
			f, calls := newOwnedMailCalendarFixture(t, "caldav", true, nil)
			server := newRoutedIMAPServer(t)
			f.useIMAPServer(t, server)
			server.mu.Lock()
			server.bodyOverride = ownedMailCalendarMIME("alice", "alice@example.com")
			if mode == "validity-change" {
				server.validity = 999
			}
			server.mu.Unlock()
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE messages SET raw_path='' WHERE id=1`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			r := f.request("alice", "GET", "/api/mail/1/calendar", "")
			if mode == "recover" {
				if r.Code != 200 || !strings.Contains(r.Body.String(), "alice current invitation") {
					t.Fatal("owned IMAP MIME recovery", r.Code, r.Body.String())
				}
			} else if r.Code != 204 || r.Body.Len() != 0 {
				t.Fatal("mismatched mailbox UID validity rendered", r.Code, r.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatal("IMAP recovery used calendar HTTP")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var subject, body, raw string
				err := db.Read().QueryRow(`SELECT subject,body_text_path,COALESCE(raw_path,'') FROM messages WHERE id=1`).Scan(&subject, &body, &raw)
				if err != nil {
					return err
				}
				if subject != "cached body metadata" || body != "existing-body" {
					return fmt.Errorf("raw fetch changed parsed metadata")
				}
				if (raw != "") != (mode == "recover") {
					return fmt.Errorf("wrong publication %q", raw)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserMailCalendarHTTPForcedRefreshAfterAcceptedMailRead(t *testing.T) {
	var api *ownedCalendarSyncAPI
	var rawCalls atomic.Int32
	f, _, _ := newOwnedCalendarFixture(t, "outlook", func(base *ownedCalendarDiscoveryAPI) http.Handler {
		api = &ownedCalendarSyncAPI{base: base, calls: map[string]int{}, entered: make(chan struct{}), release: make(chan struct{})}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/me/messages/m1/$value" {
				if r.Header.Get("Authorization") != "Bearer alice-access" {
					t.Error("wrong mailbox token", r.Header.Get("Authorization"))
					http.Error(w, "wrong", 401)
					return
				}
				rawCalls.Add(1)
				raw := strings.ReplaceAll(ownedMailCalendarMIME("alice", "alice@provider.test"), "organizer@example.com", "alice@provider.test")
				_, _ = fmt.Fprint(w, raw)
				return
			}
			if r.URL.Query().Get("$skiptoken") == "two" {
				// The shared paging fixture deliberately repeats a UID/time. Use
				// a distinct second appointment here so invitation matching is unique.
				rec := httptest.NewRecorder()
				api.ServeHTTP(rec, r)
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture", 500)
					return
				}
				body["value"].([]any)[0].(map[string]any)["iCalUId"] = "other-uid"
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			api.ServeHTTP(w, r)
		})
	})
	t.Cleanup(api.unblock)
	account := f.accounts["alice"].ID
	if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), "alice", account, true); err != nil {
		t.Fatal(err)
	}
	if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", account, []string{"same-source"}); err != nil {
		t.Fatal(err)
	}
	// The mail token is cached; the known Calendar grant requires a separate
	// scoped refresh, which legitimately advances the central revision afterward.
	if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET scopes=? WHERE account_id=?`, userOutlookHTTPScopes, account); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,name,role,uid_validity) VALUES('mail',?,'INBOX','Inbox','inbox',100)`, account); err != nil {
			return err
		}
		if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,remote_message_id,internet_message_id) VALUES(1,?,'m1','<alice-invitation@example.com>')`, account); err != nil {
			return err
		}
		_, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(1,'mail',2)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/mail/1/calendar?refresh=1", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "alice private refreshed appointment") || strings.Contains(r.Body.String(), "bob private") {
		t.Fatal("scoped refresh discarded accepted raw MIME", r.Code, r.Body.String())
	}
	if rawCalls.Load() != 1 || api.base.count("alice", "token") != 1 || api.base.count("bob", "token") != 0 {
		t.Fatal("wrong owner/replayed operation", rawCalls.Load(), api.base.count("alice", "token"), api.base.count("bob", "token"))
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var path string
		err := db.Read().QueryRow(`SELECT raw_path FROM messages WHERE id=1`).Scan(&path)
		if err == nil && path == "" {
			return fmt.Errorf("accepted raw MIME missing")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserMailCalendarHTTPAmbiguousCancelledAndDeselectedInvitations(t *testing.T) {
	for _, mode := range []string{"ambiguous", "cancelled", "deselected", "wrong-organizer"} {
		t.Run(mode, func(t *testing.T) {
			f, calls := newOwnedMailCalendarFixture(t, "outlook", true, nil)
			if mode == "deselected" {
				if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", f.accounts["alice"].ID, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					query := `UPDATE calendar_events SET status='cancelled' WHERE id='same-event'`
					if mode == "wrong-organizer" {
						query = `UPDATE calendar_events SET organizer_email='other-organizer@example.com' WHERE id='same-event'`
					}
					if mode == "ambiguous" {
						query = `INSERT INTO calendar_events(id,user_id,source_id,remote_id,ical_uid,etag,summary,organizer_email,start_at,end_at) SELECT 'second-event',user_id,source_id,'other-remote',ical_uid,etag,'ambiguous private event',organizer_email,start_at,end_at FROM calendar_events WHERE id='same-event'`
					}
					_, err := db.Write().Exec(query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			r := f.request("alice", "GET", "/api/mail/1/calendar", "")
			if r.Code != 200 || strings.Contains(r.Body.String(), "data-calendar-response-form") {
				t.Fatal("unverified invitation offered response", mode, r.Code, r.Body.String())
			}
			if mode == "cancelled" {
				if !strings.Contains(r.Body.String(), "Cancelled event") {
					t.Fatal("cancelled status missing", r.Body.String())
				}
			} else if !strings.Contains(r.Body.String(), "could not be matched") {
				t.Fatal("unmatched reason missing", mode, r.Body.String())
			}
			if r := f.request("bob", "GET", "/api/mail/1/calendar", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "data-calendar-response-form") {
				t.Fatal("foreign calendar changed", r.Code, r.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatal("local invitation verification performed HTTP")
			}
		})
	}
}

func TestUserMailCalendarHTTPOutlookWrappedUIDPreservesMatching(t *testing.T) {
	f, calls := newOwnedMailCalendarFixture(t, "outlook", true, nil)
	header, _ := hex.DecodeString("040000008200e00074c5b7101a82e008")
	data := make([]byte, 40)
	copy(data, header)
	data[20] = 42
	payload := []byte("vCal-Uid\x01\x00\x00\x00private-uid\x00")
	binary.LittleEndian.PutUint32(data[36:40], uint32(len(payload)))
	wrapped := strings.ToUpper(hex.EncodeToString(append(data, payload...)))
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_events SET ical_uid=? WHERE id='same-event'`, wrapped)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/mail/1/calendar", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "alice current invitation") || !strings.Contains(r.Body.String(), "data-calendar-response-form") {
		t.Fatal("wrapped provider UID lost", r.Code, r.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("wrapped UID required HTTP")
	}
}
