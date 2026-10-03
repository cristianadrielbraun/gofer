package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestCalendarReplyQueueAtomicAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reply.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('foreign','foreign','foreign'); INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','me@example.com')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "caldav", []CalendarSource{{ID: "source", RemoteID: "https://cal.test/cal/", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	job := CalendarReplyJob{UserID: "owner", SourceID: "source", ResourceID: "https://cal.test/cal/event.ics", RemoteID: "https://cal.test/cal/event.ics#recurrence=day", Version: `"v1"`, Response: "accepted", Payload: `{"scope":"occurrence"}`}
	input := QueueOutgoingSendInput{AccountID: "account", Transport: "smtp", EnvelopeFrom: "me@example.com", EnvelopeRecipients: []string{"host@example.com"}, MIMEData: []byte("mime"), MessageJSON: []byte(`{"calendar_reply":"reply"}`)}
	foreign := job
	foreign.UserID = "foreign"
	if _, err := db.QueueCalendarReply(t.Context(), foreign, input); err == nil {
		t.Fatal("foreign user queued a reply")
	}
	id, err := db.QueueCalendarReply(t.Context(), job, input)
	if err != nil {
		t.Fatal(err)
	}
	job.Version = `"new-version"`
	job.RemoteID = "other-occurrence"
	if _, err := db.QueueCalendarReply(t.Context(), job, input); err == nil {
		t.Fatal("pending resource allowed a conflicting reply")
	}
	var sends int
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&sends)
	if sends != 1 {
		t.Fatal("failed transaction orphaned a deliverable email")
	}
	if _, err := db.GetCalendarReply(t.Context(), "foreign", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign user read private reply")
	}
	db.Close()
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.PendingCalendarReply(t.Context(), "owner", "source", job.ResourceID)
	if err != nil || stored.ID != id || stored.Version != `"v1"` || stored.SendStatus != "pending" {
		t.Fatalf("reply lost across restart: %+v %v", stored, err)
	}
	if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status='sent' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.ListCalendarReplyFollowups(t.Context())
	if err != nil || len(jobs) != 1 {
		t.Fatal("sent reply follow-up missing", err)
	}
	if err := db.FinishCalendarReply(t.Context(), "foreign", id, "complete"); err != nil {
		t.Fatal(err)
	}
	jobs, _ = db.ListCalendarReplyFollowups(t.Context())
	if len(jobs) != 1 {
		t.Fatal("foreign user completed reply")
	}
	if err := db.FinishCalendarReply(t.Context(), "owner", id, "complete"); err != nil {
		t.Fatal(err)
	}
	jobs, _ = db.ListCalendarReplyFollowups(t.Context())
	if len(jobs) != 0 {
		t.Fatal("finished reply would be processed again")
	}
	if _, err := db.Write().Exec(`ALTER TABLE calendar_reply_jobs DROP COLUMN attempted_at; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(100)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV100ToV101(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	upgraded, err := db.GetCalendarReply(t.Context(), "owner", id)
	if err != nil || upgraded.State != "complete" || upgraded.Payload != stored.Payload {
		t.Fatal("recovery migration lost a queued reply", err)
	}
	if err := db.AttemptCalendarReply(t.Context(), "owner", id); err != nil {
		t.Fatal("recovery migration omitted attempted timestamp", err)
	}
	if _, err := db.Write().Exec(`DROP TABLE calendar_reply_jobs; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(99)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV99ToV100(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	_ = db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if version != 100 {
		t.Fatal("migration missing")
	}
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&sends)
	if sends != 1 {
		t.Fatal("migration damaged mail queue")
	}
}
