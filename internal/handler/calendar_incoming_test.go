package handler

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"github.com/emersion/go-msgauth/dkim"
)

type incomingDAVFixture struct {
	body, etag, uid                                            string
	writes                                                     int
	reject, dropConfirmation, confirmationMissing, ignoreWrite bool
}

func calendarIncomingFixture(t *testing.T) (*Handler, *incomingDAVFixture) {
	t.Helper()
	h := calendarCreateFixture(t)
	f := &incomingDAVFixture{etag: `"v1"`}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cal/event.ics" {
			t.Errorf("wrong resource: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "pass" {
			t.Error("missing calendar identity")
		}
		switch r.Method {
		case "GET":
			if f.confirmationMissing {
				f.confirmationMissing = false
				w.WriteHeader(503)
				return
			}
			w.Header().Set("ETag", f.etag)
			io.WriteString(w, f.body)
		case "PUT":
			if r.Header.Get("If-Match") != f.etag {
				t.Error("unsafe reply write")
			}
			if f.reject {
				w.WriteHeader(412)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if !f.ignoreWrite {
				f.body = string(body)
			}
			f.writes++
			f.etag = fmt.Sprintf(`"v%d"`, f.writes+1)
			if f.dropConfirmation {
				f.dropConfirmation = false
				f.confirmationMissing = true
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(server.Close)
	prior := calDAVHTTPTransport
	calDAVHTTPTransport = server.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = prior })
	source := storage.CalendarSource{ID: "one-source", UserID: "one", AccountID: "one-account", Provider: "caldav", RemoteID: server.URL + "/cal/", AccessRole: "owner", IsSelected: true}
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='imap',email_sync_enabled=1 WHERE id='one-account'; UPDATE calendar_sources SET provider='caldav',access_role='owner',remote_id=? WHERE id='one-source'; INSERT INTO folders(id,account_id,name,role,remote_id) VALUES('incoming','one-account','Inbox','inbox','INBOX')`, source.RemoteID); err != nil {
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
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	draft := calendarProviderDraft(t, false)
	draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}, {Email: "other@example.com"}}
	draft.OrganizerEmail = "one@example.com"
	draft.ScheduleAgent = "CLIENT"
	f.body, err = calendarCreateICS(draft)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendarUpdateDecodeICS([]byte(f.body))
	if err != nil {
		t.Fatal(err)
	}
	cal.Events()[0].Props.SetDateTime("DTSTAMP", time.Now().Add(-2*time.Hour).UTC())
	cal.Events()[0].Props.SetText("X-PRIVATE", "keep me")
	f.body, err = encodeCalendarReply(cal)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := calendarMeetingNormalized(cal, http.Header{"Etag": {f.etag}}, server.URL+"/cal/event.ics", "one@example.com")
	if err != nil {
		t.Fatal(err)
	}
	f.uid = remote.ICalUID
	event := calendarStorageEvent("one", "one-source", remote)
	event.ID = "incoming-meeting"
	if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{event}, draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return h, f
}

func incomingReplyMail(t *testing.T, h *Handler, f *incomingDAVFixture, status string, stamp time.Time, signed bool) []byte {
	t.Helper()
	body := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:%s\r\nSEQUENCE:0\r\nDTSTAMP:%s\r\nORGANIZER:mailto:one@example.com\r\nATTENDEE;PARTSTAT=%s:mailto:guest@example.com\r\nSUMMARY:Malicious replacement title\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", f.uid, stamp.UTC().Format("20060102T150405Z"), status)
	raw := []byte("From: guest@example.com\r\nTo: one@example.com\r\nContent-Type: text/calendar; method=REPLY\r\n\r\n" + body)
	if !signed {
		return raw
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{24}, ed25519.SeedSize))
	h.calendarIncomingLookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}, nil
	}
	var output bytes.Buffer
	if err := dkim.Sign(&output, bytes.NewReader(raw), &dkim.SignOptions{Domain: "example.com", Selector: "test", Signer: key}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func storeIncomingTestMail(t *testing.T, h *Handler, raw []byte) int64 {
	t.Helper()
	result, err := h.db.Write().Exec(`INSERT INTO messages(account_id,from_email) VALUES('one-account','guest@example.com')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	path, err := h.blobStore.StoreRaw(t.Context(), "one-account", id, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Write().Exec(`UPDATE messages SET raw_path=? WHERE id=?`, path, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(?,'incoming',?)`, id, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func incomingOutcome(t *testing.T, h *Handler, id int64) string {
	t.Helper()
	var state string
	if err := h.db.Read().QueryRow(`SELECT state FROM calendar_incoming_messages WHERE message_id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCalendarIncomingWorkerSignedReplyAndOrdering(t *testing.T) {
	h, f := calendarIncomingFixture(t)
	stamp := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	raw := incomingReplyMail(t, h, f, "ACCEPTED", stamp, true)
	id := storeIncomingTestMail(t, h, raw)
	h.runCalendarIncomingTick(t.Context())
	if f.writes != 1 || incomingOutcome(t, h, id) != "complete" {
		t.Fatalf("reply not applied: writes=%d", f.writes)
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "incoming-meeting")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(event.AttendeesJSON, `"status":"ACCEPTED"`) || event.ETag != f.etag || event.ResponseStatus != "organizer" || strings.Contains(event.Summary, "Malicious") {
		t.Fatalf("incorrect cache: %+v", event)
	}
	stored, err := calendarUpdateDecodeICS([]byte(f.body))
	if err != nil {
		t.Fatal(err)
	}
	private, _ := stored.Events()[0].Props.Text("X-PRIVATE")
	if private != "keep me" || stored.Events()[0].Props.Get("SEQUENCE").Value != "0" || !strings.Contains(f.body, "PARTSTAT=NEEDS-ACTION") {
		t.Fatal("reply damaged other event content")
	}
	duplicate := storeIncomingTestMail(t, h, raw)
	h.runCalendarIncomingTick(t.Context())
	if f.writes != 1 || incomingOutcome(t, h, duplicate) != "complete" {
		t.Fatal("duplicate wrote a second response")
	}
	older := storeIncomingTestMail(t, h, incomingReplyMail(t, h, f, "DECLINED", stamp.Add(-time.Minute), true))
	h.runCalendarIncomingTick(t.Context())
	if f.writes != 1 || incomingOutcome(t, h, older) != "ignored" {
		t.Fatal("older reply overwrote newer status")
	}
	newer := storeIncomingTestMail(t, h, incomingReplyMail(t, h, f, "TENTATIVE", stamp.Add(time.Minute), true))
	h.runCalendarIncomingTick(t.Context())
	if f.writes != 2 || incomingOutcome(t, h, newer) != "complete" {
		t.Fatal("newer response was not applied")
	}
	var notifications int
	_ = h.db.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&notifications)
	if notifications != 0 {
		t.Fatal("receiving a response sent another invitation")
	}
}

func TestCalendarIncomingWorkerLostConfirmationAndConflict(t *testing.T) {
	for _, mode := range []string{"lost confirmation", "conflict", "unconfirmed status"} {
		t.Run(mode, func(t *testing.T) {
			h, f := calendarIncomingFixture(t)
			f.dropConfirmation = mode == "lost confirmation"
			f.reject = mode == "conflict"
			f.ignoreWrite = mode == "unconfirmed status"
			id := storeIncomingTestMail(t, h, incomingReplyMail(t, h, f, "ACCEPTED", time.Now().Add(-time.Hour), true))
			h.runCalendarIncomingTick(t.Context())
			if incomingOutcome(t, h, id) != "retry" {
				t.Fatal("ambiguous write not retained for recovery")
			}
			f.ignoreWrite = false
			f.reject = false
			if _, err := h.db.Write().Exec(`UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 minute')`); err != nil {
				t.Fatal(err)
			}
			h.runCalendarIncomingTick(t.Context())
			if incomingOutcome(t, h, id) != "complete" {
				t.Fatal("reply did not recover")
			}
			want := 1
			if mode == "unconfirmed status" {
				want = 2
			}
			if f.writes != want {
				t.Fatalf("unsafe recovery writes=%d want=%d", f.writes, want)
			}
		})
	}
}

func TestCalendarIncomingWorkerIgnoresUnsafeReplies(t *testing.T) {
	for _, mode := range []string{"unsigned", "old sequence", "server scheduled", "foreign organizer", "removed guest", "canceled", "wrong UID", "other mailbox", "deselected"} {
		t.Run(mode, func(t *testing.T) {
			h, f := calendarIncomingFixture(t)
			raw := incomingReplyMail(t, h, f, "ACCEPTED", time.Now().Add(-time.Hour), mode != "unsigned")
			// Alter the server resource, not the signed reply, to exercise fresh checks.
			switch mode {
			case "old sequence":
				cal, err := calendarUpdateDecodeICS([]byte(f.body))
				if err != nil {
					t.Fatal(err)
				}
				cal.Events()[0].Props.Get("SEQUENCE").Value = "1"
				f.body, err = encodeCalendarReply(cal)
				if err != nil {
					t.Fatal(err)
				}
			case "server scheduled":
				f.body = strings.ReplaceAll(f.body, "SCHEDULE-AGENT=CLIENT", "SCHEDULE-AGENT=SERVER")
			case "foreign organizer":
				f.body = strings.ReplaceAll(f.body, "one@example.com", "other-host@example.com")
			case "removed guest":
				f.body = strings.ReplaceAll(f.body, "guest@example.com", "removed@example.com")
			case "canceled":
				f.body = strings.Replace(f.body, "END:VEVENT", "STATUS:CANCELLED\r\nEND:VEVENT", 1)
			case "wrong UID":
				f.body = strings.ReplaceAll(f.body, f.uid, "replacement-uid")
			}
			id := storeIncomingTestMail(t, h, raw)
			candidate := storage.CalendarIncomingMessage{ID: id, UserID: "one", AccountID: "one-account", From: "guest@example.com"}
			if mode == "other mailbox" {
				candidate.UserID = "two"
			}
			if mode == "deselected" {
				if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.processCalendarIncomingMessage(t.Context(), candidate); err == nil {
				t.Fatal("unsafe reply accepted")
			}
			if f.writes != 0 {
				t.Fatal("unsafe reply changed provider event")
			}
		})
	}
}
