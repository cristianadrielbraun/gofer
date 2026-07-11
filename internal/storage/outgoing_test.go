package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func outgoingTestInput(accountID string, messageID int64, sendAfter time.Time, scheduled bool) QueueOutgoingSendInput {
	return QueueOutgoingSendInput{
		AccountID:          accountID,
		MessageID:          messageID,
		DraftID:            "<draft@example.com>",
		Transport:          OutgoingTransportSMTP,
		EnvelopeFrom:       "user@example.com",
		EnvelopeRecipients: []string{"friend@example.com"},
		MIMEData:           []byte("Message-ID: <stable@example.com>\r\n\r\nImmutable body"),
		MessageJSON:        []byte(`{"message_id":"<stable@example.com>"}`),
		SendAfter:          sendAfter,
		IsScheduled:        scheduled,
	}
}

func TestOutgoingSendLifecycleKeepsImmutablePayload(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	msgID, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@example.com>",
		Subject: "Scheduled", FromEmail: "user@example.com", ToRecipients: []Recipient{{Email: "friend@example.com"}},
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}

	future := time.Now().UTC().Add(time.Hour).Round(time.Second)
	queued, err := db.QueueOutgoingSend(ctx, outgoingTestInput("acc", msgID, future, true))
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if queued.Status != OutgoingSendPending || !queued.SendAfter.Equal(future) || !queued.IsScheduled {
		t.Fatalf("queued = %#v, want scheduled pending at %s", queued, future)
	}

	due, err := db.ClaimDueOutgoingSends(ctx, future.Add(-time.Minute), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("ClaimDueOutgoingSends(before due) = %#v, %v", due, err)
	}
	due, err = db.ClaimDueOutgoingSends(ctx, future.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("ClaimDueOutgoingSends(due) error = %v", err)
	}
	if len(due) != 1 || due[0].ID != queued.ID || !bytes.Equal(due[0].MIMEData, outgoingTestInput("acc", msgID, future, true).MIMEData) {
		t.Fatalf("claimed = %#v, want original queued payload", due)
	}
	if err := db.CompleteOutgoingSend(ctx, queued.ID, "<stable@example.com>"); err != nil {
		t.Fatalf("CompleteOutgoingSend() error = %v", err)
	}
	got, err := db.GetOutgoingSendByMessageID(ctx, msgID)
	if err != nil {
		t.Fatalf("GetOutgoingSendByMessageID() error = %v", err)
	}
	if got.Status != OutgoingSendSent || got.SentMessageID != "<stable@example.com>" || got.AttemptCount != 1 {
		t.Fatalf("completed send = %#v", got)
	}
	if len(got.MIMEData) != 0 || len(got.MessageJSON) != 0 || len(got.EnvelopeRecipients) != 0 {
		t.Fatalf("completed send retained delivery payload: %#v", got)
	}
}

func TestUpdatingScheduledSendKeepsStableOperationID(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	msgID, err := db.SaveDraftMessage(ctx, DraftMessageInput{AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@example.com>", FromEmail: "user@example.com"})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	firstInput := outgoingTestInput("acc", msgID, time.Now().Add(time.Hour), true)
	first, err := db.QueueOutgoingSend(ctx, firstInput)
	if err != nil {
		t.Fatalf("first QueueOutgoingSend() error = %v", err)
	}
	secondInput := firstInput
	secondInput.MIMEData = []byte("Message-ID: <updated@example.com>\r\n\r\nUpdated")
	secondInput.SendAfter = firstInput.SendAfter.Add(time.Hour)
	second, err := db.QueueOutgoingSend(ctx, secondInput)
	if err != nil {
		t.Fatalf("second QueueOutgoingSend() error = %v", err)
	}
	if second.ID != first.ID || !bytes.Equal(second.MIMEData, secondInput.MIMEData) || !second.SendAfter.Equal(secondInput.SendAfter) {
		t.Fatalf("updated send first=%#v second=%#v", first, second)
	}
}

func TestInterruptedOutgoingSendBecomesAmbiguousAndIsNotRetried(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	queued, err := db.QueueOutgoingSend(ctx, outgoingTestInput("acc", 0, time.Now().Add(-time.Minute), false))
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if sends, err := db.ClaimDueOutgoingSends(ctx, time.Now(), 1); err != nil || len(sends) != 1 {
		t.Fatalf("claim = %#v, %v", sends, err)
	}
	if count, err := db.MarkInterruptedOutgoingSendsAmbiguous(ctx, "interrupted"); err != nil || count != 1 {
		t.Fatalf("MarkInterruptedOutgoingSendsAmbiguous() = %d, %v", count, err)
	}
	got, err := db.GetOutgoingSend(ctx, queued.ID)
	if err != nil || got.Status != OutgoingSendAmbiguous || got.LastError != "interrupted" {
		t.Fatalf("interrupted send = %#v, %v", got, err)
	}
	if sends, err := db.ClaimDueOutgoingSends(ctx, time.Now().Add(time.Hour), 1); err != nil || len(sends) != 0 {
		t.Fatalf("ambiguous send was claimed again: %#v, %v", sends, err)
	}
}

func TestPendingOutgoingSendSurvivesDatabaseRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	queued, err := db.QueueOutgoingSend(ctx, outgoingTestInput("acc", 0, time.Now().Add(-time.Minute), false))
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatalf("reopen New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sends, err := db.ClaimDueOutgoingSends(ctx, time.Now(), 1)
	if err != nil || len(sends) != 1 || sends[0].ID != queued.ID {
		t.Fatalf("send after restart = %#v, %v", sends, err)
	}
}

func TestScheduledVirtualFolderListsPendingScheduledDrafts(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	msgID, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID: "acc", FolderID: "drafts", InternetMessageID: "<scheduled@example.com>", Subject: "Scheduled draft",
		FromEmail: "user@example.com", ToRecipients: []Recipient{{Email: "friend@example.com"}},
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage(scheduled) error = %v", err)
	}
	if _, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID: "acc", FolderID: "drafts", InternetMessageID: "<plain-draft@example.com>", Subject: "Plain draft", FromEmail: "user@example.com",
	}); err != nil {
		t.Fatalf("SaveDraftMessage(plain) error = %v", err)
	}
	if _, err := db.QueueOutgoingSend(ctx, outgoingTestInput("acc", msgID, time.Now().Add(time.Hour), true)); err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}

	count, err := db.GetFolderEmailCountFilteredForUser(ctx, "default", "scheduled", models.EmailFilters{})
	if err != nil || count != 1 {
		t.Fatalf("scheduled count = %d, %v", count, err)
	}
	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "scheduled", 0, 50, models.EmailFilters{})
	if err != nil || page.TotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].Subject != "Scheduled draft" {
		t.Fatalf("scheduled page = %#v, %v", page, err)
	}
}
