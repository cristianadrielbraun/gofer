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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-msgauth/dkim"
)

type ownedIncomingFixture struct {
	*userContactPushFixture
	mu                               sync.Mutex
	body, etag, uid                  string
	reads, writes                    int
	missingRead, reject, ignoreWrite bool
	beforeRead, afterWrite           func()
}

func ownedIncomingCalendarFixture(t *testing.T) *ownedIncomingFixture {
	t.Helper()
	f := &ownedIncomingFixture{etag: `"v1"`}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alice/cal/event.ics" {
			t.Error("foreign provider resource", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "alice-calendar-secret" {
			t.Error("foreign provider credentials")
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case "GET":
			f.reads++
			if f.beforeRead != nil {
				f.beforeRead()
			}
			if f.missingRead {
				f.missingRead = false
				w.WriteHeader(503)
				return
			}
			w.Header().Set("ETag", f.etag)
			w.Header().Set("Schedule-Tag", `"schedule-v1"`)
			io.WriteString(w, f.body)
		case "PUT":
			if r.Header.Get("If-Match") != f.etag || r.Header.Get("If-Schedule-Tag-Match") != `"schedule-v1"` {
				t.Error("unconditional guest update")
			}
			if f.reject {
				w.WriteHeader(412)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			f.writes++
			if !f.ignoreWrite {
				f.body = string(body)
				f.etag = fmt.Sprintf(`"v%d"`, f.writes+1)
			}
			if f.afterWrite != nil {
				f.afterWrite()
			}
			w.WriteHeader(204)
		default:
			t.Error("unexpected native request", r.Method)
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(server.Close)
	f.userContactPushFixture = newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected mail/contacts dispatch", r.URL)
		w.WriteHeader(500)
	}))
	f.h.blobStore = f.h.userIMAP.Blobs()
	prior := calDAVHTTPTransport
	calDAVHTTPTransport = server.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = prior })
	draft := calendarProviderDraft(t, false)
	draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}, {Email: "other@example.com"}}
	draft.OrganizerEmail = "alice@example.com"
	draft.ScheduleAgent = "CLIENT"
	body, err := calendarCreateICS(draft)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendarUpdateDecodeICS([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	cal.Events()[0].Props.SetDateTime("DTSTAMP", time.Now().Add(-2*time.Hour).UTC())
	cal.Events()[0].Props.SetText("X-PRIVATE", "keep me")
	f.body, err = encodeCalendarReply(cal)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		err = f.h.userAccounts.WithUser(t.Context(), owner, func(local *config.AccountStore, db *storage.DB) error {
			id := f.accounts[owner].ID
			if err := local.SaveCalDAVConfig(t.Context(), owner, id, server.URL+"/", owner, owner+"-calendar-secret", false); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=1 WHERE id=?`, id); err != nil {
				return err
			}
			if err := db.ReplaceCalendarSources(t.Context(), owner, id, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: server.URL + "/" + owner + "/cal/", IsSelected: true, AccessRole: "owner"}}); err != nil {
				return err
			}
			ownedCalendar := &ical.Calendar{Component: cloneCalendarComponent(cal.Component)}
			ownedCalendar.Events()[0].Props.Get("ORGANIZER").Value = "mailto:" + owner + "@example.com"
			remote, err := calendarMeetingNormalized(ownedCalendar, http.Header{"Etag": {f.etag}}, server.URL+"/"+owner+"/cal/event.ics", owner+"@example.com")
			if err != nil {
				return err
			}
			f.uid = remote.ICalUID
			event := calendarStorageEvent(owner, "same-source", remote)
			event.ID = "same-event"
			if err := db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{event}, draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour)); err != nil {
				return err
			}
			_, err = db.Write().Exec(`INSERT INTO folders(id,account_id,name,role,remote_id,uid_validity) VALUES('incoming',?,'Inbox','inbox','INBOX',17)`, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *ownedIncomingFixture) seed(t *testing.T, status string, stamp time.Time, signed bool) storage.CalendarIncomingMessage {
	t.Helper()
	body := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:%s\r\nSEQUENCE:0\r\nDTSTAMP:%s\r\nORGANIZER:mailto:alice@example.com\r\nATTENDEE;PARTSTAT=%s:mailto:guest@example.com\r\nSUMMARY:Malicious replacement\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", f.uid, stamp.UTC().Format("20060102T150405Z"), status)
	raw := []byte("From: guest@example.com\r\nTo: alice@example.com\r\nContent-Type: text/calendar; method=REPLY\r\n\r\n" + body)
	if signed {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{24}, ed25519.SeedSize))
		f.h.calendarIncomingLookupTXT = func(context.Context, string) ([]string, error) {
			return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}, nil
		}
		var output bytes.Buffer
		if err := dkim.Sign(&output, bytes.NewReader(raw), &dkim.SignOptions{Domain: "example.com", Selector: "test", Signer: key}); err != nil {
			t.Fatal(err)
		}
		raw = output.Bytes()
	}
	c := storage.CalendarIncomingMessage{UserID: "alice", AccountID: f.accounts["alice"].ID, From: "guest@example.com"}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		result, err := db.Write().Exec(`INSERT INTO messages(account_id,from_email) VALUES(?,'guest@example.com')`, c.AccountID)
		if err != nil {
			return err
		}
		c.ID, _ = result.LastInsertId()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path, err := f.h.blobStore.StoreRaw(t.Context(), c.AccountID, c.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE messages SET raw_path=? WHERE id=?`, path, c.ID)
	f.exec(t, `INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(?,'incoming',?)`, c.ID, c.ID)
	return c
}
func (f *ownedIncomingFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(query, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func (f *ownedIncomingFixture) outcome(t *testing.T, id int64) string {
	t.Helper()
	var state string
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		return db.Read().QueryRow(`SELECT state FROM calendar_incoming_messages WHERE message_id=?`, id).Scan(&state)
	}); err != nil {
		t.Fatal(err)
	}
	return state
}
func (f *ownedIncomingFixture) event(t *testing.T, owner string) storage.CalendarEvent {
	t.Helper()
	var event storage.CalendarEvent
	if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		event, err = db.GetCalendarEvent(t.Context(), owner, "same-event")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestUserCalendarIncomingNativeSignedOrderingAndIsolation(t *testing.T) {
	f := ownedIncomingCalendarFixture(t)
	before := f.event(t, "bob")
	stamp := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, step := range []struct {
		status string
		delta  time.Duration
		writes int
		state  string
	}{{"ACCEPTED", 0, 1, "complete"}, {"ACCEPTED", 0, 1, "complete"}, {"DECLINED", -time.Minute, 1, "ignored"}, {"TENTATIVE", time.Minute, 2, "complete"}} {
		c := f.seed(t, step.status, stamp.Add(step.delta), true)
		// A different owner's lease during DKIM proves no Alice lease survives DNS.
		lookup := f.h.calendarIncomingLookupTXT
		f.h.calendarIncomingLookupTXT = func(ctx context.Context, name string) ([]string, error) {
			_ = f.event(t, "bob")
			return lookup(ctx, name)
		}
		err := f.h.processUserCalendarIncoming(t.Context(), c)
		if step.state == "complete" && err != nil {
			t.Fatal(err)
		}
		if f.writes != step.writes || f.outcome(t, c.ID) != step.state {
			t.Fatal("incorrect ordering", f.writes, f.outcome(t, c.ID), err)
		}
	}
	if after := f.event(t, "bob"); after.ETag != before.ETag || after.AttendeesJSON != before.AttendeesJSON {
		t.Fatal("foreign same-ID event changed")
	}
	event := f.event(t, "alice")
	if event.ETag != f.etag || event.ResponseStatus != "organizer" || !strings.Contains(event.AttendeesJSON, `"status":"TENTATIVE"`) || strings.Contains(event.Summary, "Malicious") {
		t.Fatal("incorrect confirmed cache", event)
	}
	cal, err := calendarUpdateDecodeICS([]byte(f.body))
	if err != nil {
		t.Fatal(err)
	}
	private, _ := cal.Events()[0].Props.Text("X-PRIVATE")
	if private != "keep me" || !strings.Contains(f.body, "PARTSTAT=NEEDS-ACTION") || cal.Events()[0].Props.Get("SEQUENCE").Value != "0" {
		t.Fatal("guest response damaged organizer data")
	}
}

func TestUserCalendarIncomingNativeUnverifiedAndUncertainWrites(t *testing.T) {
	for _, mode := range []string{"unsigned", "lost-confirmation", "conflict", "unconfirmed", "server-managed"} {
		t.Run(mode, func(t *testing.T) {
			f := ownedIncomingCalendarFixture(t)
			if mode == "server-managed" {
				f.body = strings.ReplaceAll(f.body, "SCHEDULE-AGENT=CLIENT", "SCHEDULE-AGENT=SERVER")
			}
			c := f.seed(t, "ACCEPTED", time.Now().Add(-time.Hour), mode != "unsigned")
			f.reject = mode == "conflict"
			f.ignoreWrite = mode == "unconfirmed"
			if mode == "lost-confirmation" {
				f.afterWrite = func() { f.missingRead = true }
			}
			_ = f.h.processUserCalendarIncoming(t.Context(), c)
			state := f.outcome(t, c.ID)
			if mode == "unsigned" || mode == "server-managed" {
				if state != "ignored" || f.writes != 0 || (mode == "unsigned" && f.reads != 0) {
					t.Fatal("unverified/server-managed reply applied", state, f.reads, f.writes)
				}
				return
			}
			if state != "retry" || f.event(t, "alice").ETag != `"v1"` {
				t.Fatal("uncertain write published", state)
			}
			if mode == "lost-confirmation" {
				f.afterWrite = nil
				f.exec(t, `UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 second') WHERE message_id=?`, c.ID)
				if err := f.h.processUserCalendarIncoming(t.Context(), c); err != nil {
					t.Fatal(err)
				}
				if f.writes != 1 || f.outcome(t, c.ID) != "complete" || f.event(t, "alice").ETag != f.etag {
					t.Fatal("retry repeated accepted write")
				}
			}
		})
	}
}

func TestUserCalendarIncomingRejectsChangesAcrossExternalWaits(t *testing.T) {
	for _, stage := range []string{"dns", "read", "write", "publication"} {
		for _, fault := range []string{"sender", "membership", "credentials", "event", "ambiguity", "reservation"} {
			if (stage == "dns" || stage == "read") && fault == "reservation" {
				continue
			}
			t.Run(stage+"/"+fault, func(t *testing.T) {
				f := ownedIncomingCalendarFixture(t)
				c := f.seed(t, "ACCEPTED", time.Now().Add(-time.Hour), true)
				fired := false
				mutate := func() {
					if fired {
						return
					}
					fired = true
					queries := map[string]string{
						"sender":      `UPDATE messages SET from_email='replacement@example.com' WHERE id=1`,
						"membership":  `UPDATE message_folder_state SET remote_uid=999 WHERE message_id=1`,
						"credentials": `UPDATE account_caldav_configs SET username='replacement'`,
						"event":       `UPDATE calendar_events SET etag='replacement' WHERE id='same-event'`,
						"ambiguity":   `INSERT INTO calendar_events(id,user_id,source_id,remote_id,ical_uid,organizer_email,attendees_json,all_day,start_at,end_at,start_date,end_date) SELECT 'duplicate',user_id,source_id,'duplicate.ics',ical_uid,organizer_email,attendees_json,all_day,start_at,end_at,start_date,end_date FROM calendar_events WHERE id='same-event'`,
						"reservation": `UPDATE calendar_incoming_responses SET response='DECLINED'`,
					}
					f.exec(t, queries[fault])
				}
				switch stage {
				case "dns":
					lookup := f.h.calendarIncomingLookupTXT
					f.h.calendarIncomingLookupTXT = func(ctx context.Context, name string) ([]string, error) { mutate(); return lookup(ctx, name) }
				case "read":
					f.beforeRead = mutate
				case "write":
					f.afterWrite = mutate
				case "publication":
					body := map[string]string{
						"sender":      `UPDATE messages SET from_email='replacement@example.com' WHERE id=1;`,
						"membership":  `UPDATE message_folder_state SET remote_uid=999 WHERE message_id=1;`,
						"credentials": `UPDATE account_caldav_configs SET username='replacement';`,
						"event":       `UPDATE calendar_events SET etag='replacement' WHERE id=NEW.id;`,
						"ambiguity":   `INSERT INTO calendar_events(id,user_id,source_id,remote_id,ical_uid,organizer_email,attendees_json,all_day,start_at,end_at,start_date,end_date) SELECT 'duplicate',user_id,source_id,'duplicate.ics',ical_uid,organizer_email,attendees_json,all_day,start_at,end_at,start_date,end_date FROM calendar_events WHERE id=NEW.id;`,
						"reservation": `UPDATE calendar_incoming_responses SET response='DECLINED';`,
					}[fault]
					f.exec(t, `CREATE TRIGGER faulty AFTER UPDATE ON calendar_events WHEN NEW.etag <> OLD.etag BEGIN `+body+` END`)
				}
				err := f.h.processUserCalendarIncoming(t.Context(), c)
				if err == nil {
					t.Fatal("stale operation reported success")
				}
				event := f.event(t, "alice")
				if strings.Contains(event.AttendeesJSON, `"status":"ACCEPTED"`) {
					t.Fatal("stale provider result published")
				}
				if stage == "dns" && f.reads != 0 {
					t.Fatal("changed claim dispatched provider")
				}
				if stage == "read" && f.writes != 0 {
					t.Fatal("changed preflight dispatched write")
				}
				if stage == "publication" {
					if event.ETag != `"v1"` || f.outcome(t, c.ID) != "retry" {
						t.Fatal("failed atomic publication left partial data", event.ETag, f.outcome(t, c.ID))
					}
					// Trigger side effects, including a newer receipt, must roll back too.
					f.exec(t, `DROP TRIGGER faulty`)
					f.exec(t, `UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 second') WHERE message_id=?`, c.ID)
					if err := f.h.processUserCalendarIncoming(t.Context(), c); err != nil {
						t.Fatal("retry did not recover rolled-back publication", err)
					}
					if f.writes != 1 || f.outcome(t, c.ID) != "complete" {
						t.Fatal("rollback retry duplicated native write")
					}
				}
				if event := f.event(t, "bob"); event.ETag != `"v1"` {
					t.Fatal("foreign event changed")
				}
			})
		}
	}
}

func TestUserCalendarIncomingWorkerUsesRetryDeadline(t *testing.T) {
	f := ownedIncomingCalendarFixture(t)
	c := f.seed(t, "ACCEPTED", time.Now().Add(-time.Hour), true)
	f.afterWrite = func() { f.missingRead = true }
	f.exec(t, `UPDATE calendar_sync_state SET next_attempt_at=datetime('now','+1 day') WHERE source_id='same-source'`)
	if _, err := f.system.Write().Exec(`INSERT INTO gofer_account_service_schedule(account_id,service,next_due_ms,revision) VALUES(?,'calendar',?,1) ON CONFLICT(account_id,service) DO UPDATE SET next_due_ms=excluded.next_due_ms`, c.AccountID, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	w := &userCalendarWorkers{h: f.h, options: UserCalendarSyncOptions{ScanInterval: time.Hour, RecoveryInterval: time.Hour}, wake: make(chan struct{}, 1)}
	w.process(t.Context(), userCalendarJob{owner: "alice", account: c.AccountID})
	if f.writes != 1 || f.outcome(t, c.ID) != "retry" {
		t.Fatal("scheduled incoming work missing")
	}
	var due int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_service_schedule WHERE account_id=? AND service='calendar'`, c.AccountID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if at := time.UnixMilli(due); at.Before(time.Now().Add(time.Minute)) || at.After(time.Now().Add(3*time.Minute)) {
		t.Fatal("reply retry stranded behind source deadline", at)
	}
	// Fresh, old mail beyond the first bounded batch still prompts another page.
	for range 13 {
		f.seed(t, "ACCEPTED", time.Now().Add(-time.Hour), false)
	}
	at, err := f.h.userAccounts.NextCalendarIncomingAttempt(t.Context(), "alice", c.AccountID)
	if err != nil || at.IsZero() || at.After(time.Now()) {
		t.Fatal("fresh backlog deadline absent", at, err)
	}
}

func TestUserCalendarIncomingJoinsShutdownDuringAuthentication(t *testing.T) {
	f := ownedIncomingCalendarFixture(t)
	c := f.seed(t, "ACCEPTED", time.Now().Add(-time.Hour), true)
	started := make(chan struct{})
	f.h.calendarIncomingLookupTXT = func(ctx context.Context, _ string) ([]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- f.h.processUserCalendarIncoming(t.Context(), c) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not start")
	}
	f.cancel()
	joined := make(chan struct{})
	go func() { f.h.userIMAP.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication not joined at shutdown")
	}
	if err := <-done; err == nil {
		t.Fatal("canceled authentication reported success")
	}
	if f.reads != 0 || f.writes != 0 {
		t.Fatal("shutdown authentication dispatched provider")
	}
}
