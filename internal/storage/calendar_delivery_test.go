package storage

import (
	"path/filepath"
	"testing"
)

func TestCalendarDeliveryV102Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','owner@example.com');
 INSERT INTO messages(id,account_id,from_email) VALUES(1,'account','guest@example.com');
 ALTER TABLE calendar_incoming_messages RENAME TO incoming_new;
 CREATE TABLE calendar_incoming_messages (
 message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
 state TEXT NOT NULL CHECK(state IN ('complete','ignored','retry')),
 next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 attempts INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
 DROP TABLE incoming_new;
 INSERT INTO calendar_incoming_messages(message_id,state,attempts) VALUES(1,'complete',2);
 DELETE FROM schema_version WHERE version>102;
 INSERT OR REPLACE INTO schema_version(version) VALUES(102);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, attempts int
	var state, event, uid, attendee, reason string
	if err := db.Read().QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatal("migration version", version, err)
	}
	if err := db.Read().QueryRow(`SELECT state,attempts,event_id,ical_uid,attendee,reason_code FROM calendar_incoming_messages WHERE message_id=1`).Scan(&state, &attempts, &event, &uid, &attendee, &reason); err != nil || state != "complete" || attempts != 2 || event != "" || uid != "" || attendee != "" || reason != "" {
		t.Fatal("migration lost old receipt", state, attempts, err)
	}
}
