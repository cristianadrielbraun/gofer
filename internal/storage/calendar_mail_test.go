package storage

import (
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wrappedMailCalendarUID(uid string) string {
	header, _ := hex.DecodeString(outlookCalendarUIDHeader)
	data := make([]byte, 40)
	copy(data, header)
	// Nonzero creation bytes must not prevent matching the embedded UID.
	data[20] = 42
	payload := []byte(outlookCalendarUIDMarker + uid + "\x00")
	binary.LittleEndian.PutUint32(data[36:40], uint32(len(payload)))
	return strings.ToUpper(hex.EncodeToString(append(data, payload...)))
}

func TestMailCalendarEventsResolveOutlookUIDWithinReceivingAccount(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "mail-calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign');
	 INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','outlook','guest@example.com'),('other','owner','gmail','other@example.com')`); err != nil {
		t.Fatal(err)
	}
	uid := "2406eaf3e52e4400920e101770ea08ec@google.com"
	wrapped := wrappedMailCalendarUID(uid)
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, account := range []string{"account", "other"} {
		provider, eventUID := "outlook", wrapped
		if account == "other" {
			provider, eventUID = "gmail", uid
		}
		if err := db.ReplaceCalendarSources(t.Context(), "owner", account, provider, []CalendarSource{{ID: account + "-source", RemoteID: "calendar", IsSelected: true}}); err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceCalendarEvents(t.Context(), "owner", account+"-source", []CalendarEvent{{ID: account + "-event", RemoteID: "remote", ICalUID: eventUID, OrganizerEmail: "host@example.com", ResponseStatus: "accepted", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ user, account, uid, eventID string }{
		{"owner", "account", uid, "account-event"},
		{"owner", "account", wrapped, "account-event"},
		{"owner", "other", wrapped, "other-event"},
		{"foreign", "account", uid, ""},
		{"owner", "missing", uid, ""},
		{"owner", "account", strings.ToUpper(uid), ""},
	} {
		events, err := db.ListMailCalendarEvents(t.Context(), tc.user, tc.account, tc.uid)
		if err != nil {
			t.Fatal(err)
		}
		if tc.eventID == "" {
			if len(events) != 0 {
				t.Fatalf("unexpected match: %+v", tc)
			}
		} else if len(events) != 1 || events[0].ID != tc.eventID {
			t.Fatalf("incorrect match: %+v, events=%+v", tc, events)
		}
	}
	// A matching payload with an invalid header/length is not an equivalent UID.
	badSize := wrapped[:72] + "00000000" + wrapped[80:]
	badHeader := "FF" + wrapped[2:]
	for _, invalid := range []string{badSize, badHeader, wrapped[:len(wrapped)-2] + "01"} {
		if _, err := db.Write().Exec(`UPDATE calendar_events SET ical_uid=? WHERE id='account-event'`, invalid); err != nil {
			t.Fatal(err)
		}
		if events, err := db.ListMailCalendarEvents(t.Context(), "owner", "account", uid); err != nil || len(events) != 0 {
			t.Fatalf("malformed wrapper matched: %v, %v", events, err)
		}
	}
	if _, err := db.Write().Exec(`UPDATE calendar_events SET ical_uid=? WHERE id='account-event'; UPDATE calendar_sources SET is_selected=0 WHERE id='account-source'`, wrapped); err != nil {
		t.Fatal(err)
	}
	if events, err := db.ListMailCalendarEvents(t.Context(), "owner", "account", uid); err != nil || len(events) != 0 {
		t.Fatalf("deselected calendar matched: %v, %v", events, err)
	}
}
