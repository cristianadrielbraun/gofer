package notifications

import (
	"os"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const repairCalendarMIME = "From: alice@example.com\r\nTo: guest@example.com\r\nSubject: Invitation\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nYou are invited\r\n" +
	"--b\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:repair-1\r\n" +
	"DTSTART:20260101T100000Z\r\nDTEND:20260101T110000Z\r\nSUMMARY:Repair\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n--b--\r\n"

// The old parser cached a calendar invitation as plain iCalendar text with no
// attachments. Opening it in managed mode repairs it from the saved original.
func TestUserCalendarBodyRepairUsesSavedMIME(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	server.mu.Lock()
	server.bodyOverride = repairCalendarMIME
	server.mu.Unlock()
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
		if err := f.imap.EnsureBody(t.Context(), owner, 1); err != nil {
			t.Fatal(err)
		}
	}
	legacy := func(owner string, rawWithoutCalendar bool) {
		t.Helper()
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var rawPath, textPath string
			if err := db.Read().QueryRow(`SELECT COALESCE(raw_path,''),COALESCE(body_text_path,'') FROM messages WHERE id=1`).Scan(&rawPath, &textPath); err != nil {
				return err
			}
			if rawPath == "" || textPath == "" {
				t.Fatal("body fetch kept no saved original or text body", owner, rawPath, textPath)
			}
			if err := os.WriteFile(textPath, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"), 0600); err != nil {
				return err
			}
			if rawWithoutCalendar {
				if err := os.WriteFile(rawPath, []byte("From: bob@example.com\r\nSubject: Plain\r\nContent-Type: text/plain\r\n\r\nBEGIN:VCALENDAR looking text\r\n"), 0600); err != nil {
					return err
				}
			}
			_, err := db.Write().Exec(`DELETE FROM attachments WHERE message_id=1; UPDATE messages SET body_html_path='',body_html_original_path='' WHERE id=1`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	legacy("alice", false)
	legacy("bob", true)
	requests := map[string]int{"alice": server.bodyRequests("alice"), "bob": server.bodyRequests("bob")}
	for _, owner := range []string{"alice", "bob"} {
		if r := f.request(owner, "GET", "/email/1", ""); r.Code != 200 {
			t.Fatal("open message", owner, r.Code, r.Body.String())
		}
		if server.bodyRequests(owner) != requests[owner] {
			t.Fatal("repair contacted the mail server", owner)
		}
	}
	calendarParts := func(owner string) int {
		t.Helper()
		var count int
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT count(*) FROM attachments WHERE message_id=1 AND content_type='text/calendar'`).Scan(&count)
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if calendarParts("alice") != 1 {
		t.Fatal("legacy calendar body not repaired")
	}
	if calendarParts("bob") != 0 {
		t.Fatal("message without a calendar part was rewritten")
	}
	// A repaired message no longer qualifies, so reopening changes nothing.
	if r := f.request("alice", "GET", "/email/1", ""); r.Code != 200 || calendarParts("alice") != 1 {
		t.Fatal("reopening repeated the repair", r.Code)
	}
}
