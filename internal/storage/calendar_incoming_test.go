package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarIncomingStorageMigrationScopeAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incoming.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('foreign','foreign','foreign');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','owner@example.com'),('foreign-account','foreign','imap','foreign@example.com');
 INSERT INTO folders(id,account_id,name,role) VALUES('inbox','account','Inbox','inbox'),('sent','account','Sent','sent'),('foreign-inbox','foreign-account','Inbox','inbox');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "caldav", []CalendarSource{{ID: "source", RemoteID: "https://cal.example.com/", IsSelected: true, IsHidden: true}}); err != nil {
		t.Fatal(err)
	}
	start, end := time.Now(), time.Now().Add(time.Hour)
	event := CalendarEvent{ID: "event", RemoteID: "event.ics", ICalUID: "uid", ETag: `"v1"`, OrganizerEmail: "owner@example.com", StartAt: &start, EndAt: &end, AttendeesJSON: `[{"email":"guest@example.com","status":"NEEDS-ACTION"}]`}
	if err := db.ReplaceCalendarEvents(t.Context(), "owner", "source", []CalendarEvent{event}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,from_email) VALUES(1,'account','guest@example.com'),(2,'account','stranger@example.com'),(3,'account','guest@example.com'),(4,'foreign-account','guest@example.com');
 INSERT INTO message_folder_state(message_id,folder_id) VALUES(1,'inbox'),(2,'inbox'),(3,'sent'),(4,'foreign-inbox');`); err != nil {
		t.Fatal(err)
	}
	messages, err := db.ListCalendarIncomingMessages(t.Context(), 12)
	if err != nil || len(messages) != 1 || messages[0].ID != 1 {
		t.Fatalf("candidate scope: %+v %v", messages, err)
	}
	m := messages[0]
	if err := db.FinishCalendarIncomingMessage(t.Context(), CalendarIncomingMessage{ID: 1, UserID: "foreign", AccountID: "account"}, "ignored", ""); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ListCalendarIncomingMessages(t.Context(), 12)
	if err != nil || len(messages) != 1 {
		t.Fatal("foreign outcome removed owner's message")
	}
	if err := db.FinishCalendarIncomingMessage(t.Context(), m, "retry", "offline"); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ListCalendarIncomingMessages(t.Context(), 12)
	if err != nil || len(messages) != 0 {
		t.Fatal("retry not deferred")
	}
	if _, err := db.Write().Exec(`UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 minute')`); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ListCalendarIncomingMessages(t.Context(), 12)
	if err != nil || len(messages) != 1 {
		t.Fatal("due retry lost")
	}
	event, err = db.FindCalendarIncomingEvent(t.Context(), "owner", "account", "uid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindCalendarIncomingEvent(t.Context(), "foreign", "account", "uid"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign UID lookup allowed")
	}
	stamp := time.Now().UTC().Truncate(time.Second)
	if ok, err := db.ReserveCalendarIncomingResponse(t.Context(), event, "guest@example.com", 0, stamp, "ACCEPTED"); err != nil || !ok {
		t.Fatal("reply not reserved", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		offset time.Duration
		status string
		want   bool
	}{{0, "ACCEPTED", true}, {-time.Second, "DECLINED", false}, {0, "DECLINED", false}, {time.Second, "TENTATIVE", true}} {
		if ok, err := db.ReserveCalendarIncomingResponse(t.Context(), event, "guest@example.com", 0, stamp.Add(test.offset), test.status); err != nil || ok != test.want {
			t.Fatalf("restart ordering %+v: %v %v", test, ok, err)
		}
	}
	// Upgrade an actual v101 shape, independently of fresh initialization.
	if _, err := db.Write().Exec(`DROP TABLE calendar_incoming_messages; DROP TABLE calendar_incoming_responses; DROP INDEX idx_messages_calendar_sender; DELETE FROM schema_version WHERE version>=102`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT OR REPLACE INTO schema_version(version) VALUES(101)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("upgrade version=%d err=%v", version, err)
	}
	if err := db.FinishCalendarIncomingMessage(t.Context(), m, "complete", ""); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ListCalendarIncomingMessages(t.Context(), 12)
	if err != nil || len(messages) != 0 {
		t.Fatal("completed message replayed")
	}
	if _, err := db.Write().Exec(`DELETE FROM messages WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_incoming_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal("mail deletion did not retire its receipt")
	}
}
