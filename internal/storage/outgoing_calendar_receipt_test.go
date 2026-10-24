package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A sent calendar invitation keeps who it went to and which event it was
// about, but not the message or private notification details.
func TestFinishedCalendarNotificationKeepsReceipt(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'host@example.com')`); err != nil {
		t.Fatal(err)
	}
	note := `{"subject":"Invitation: Planning","calendar_notification":{"UserID":"default","SourceID":"source","ResourceID":"/cal/planning.ics","Method":"REQUEST","Calendar":"BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n","ExpectedCalendar":"private","Deleted":false,"UserAuthority":{"Owner":"default"}}}`
	insert := func(id, messageJSON string) {
		if _, err := db.Write().ExecContext(ctx, `INSERT INTO outgoing_sends (id, account_id, transport, envelope_from, envelope_recipients, mime_data, message_json, send_after, status)
			VALUES (?, 'acc', 'smtp', 'host@example.com', '["guest@example.com"]', 'MIME', ?, CURRENT_TIMESTAMP, ?)`, id, messageJSON, OutgoingSendSending); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteOutgoingSend(ctx, id, "<"+id+"@example.com>", false); err != nil {
			t.Fatal(err)
		}
	}
	insert("invite", note)
	insert("ordinary", `{"subject":"Hello"}`)

	invite, err := db.GetOutgoingSend(ctx, "invite")
	if err != nil {
		t.Fatal(err)
	}
	if len(invite.MIMEData) != 0 || len(invite.EnvelopeRecipients) != 1 || invite.EnvelopeRecipients[0] != "guest@example.com" {
		t.Fatalf("invite = %#v", invite)
	}
	var kept struct {
		Subject              string                     `json:"subject"`
		CalendarNotification map[string]json.RawMessage `json:"calendar_notification"`
	}
	if err := json.Unmarshal(invite.MessageJSON, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Subject != "" || kept.CalendarNotification["ExpectedCalendar"] != nil || kept.CalendarNotification["UserAuthority"] != nil {
		t.Fatalf("receipt kept message details: %s", invite.MessageJSON)
	}
	for field, want := range map[string]string{"UserID": `"default"`, "SourceID": `"source"`, "ResourceID": `"/cal/planning.ics"`, "Method": `"REQUEST"`, "Deleted": `false`} {
		if got := string(kept.CalendarNotification[field]); got != want {
			t.Fatalf("%s = %s, want %s", field, got, want)
		}
	}
	ordinary, err := db.GetOutgoingSend(ctx, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary.MIMEData) != 0 || len(ordinary.MessageJSON) != 0 || len(ordinary.EnvelopeRecipients) != 0 {
		t.Fatalf("ordinary send kept data: %#v", ordinary)
	}

	// Retention still deletes old receipts.
	old := time.Now().UTC().Add(-OutgoingSendRetentionPeriod - time.Hour)
	if _, err := db.Write().ExecContext(ctx, `UPDATE outgoing_sends SET updated_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	pruned, err := db.PruneDurableMailJobs(ctx, time.Now().UTC(), RetentionBatchSize)
	if err != nil || pruned.OutgoingSends != 2 {
		t.Fatalf("pruned = %#v, err = %v", pruned, err)
	}
}
