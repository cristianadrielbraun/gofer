package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestScheduledSendLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	msgID, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID:         "acc",
		FolderID:          "drafts",
		InternetMessageID: "<draft@example.com>",
		Subject:           "Scheduled",
		FromEmail:         "user@example.com",
		ToRecipients:      []Recipient{{Email: "friend@example.com"}},
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}

	future := time.Now().UTC().Add(time.Hour).Round(time.Second)
	scheduled, err := db.UpsertScheduledSend(ctx, "acc", msgID, future)
	if err != nil {
		t.Fatalf("UpsertScheduledSend() error = %v", err)
	}
	if scheduled.Status != ScheduledSendPending || !scheduled.ScheduledFor.Equal(future) {
		t.Fatalf("scheduled = %#v, want pending at %s", scheduled, future)
	}

	due, err := db.ClaimDueScheduledSends(ctx, future.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ClaimDueScheduledSends(before due) error = %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due before scheduled time len=%d, want 0", len(due))
	}

	due, err = db.ClaimDueScheduledSends(ctx, future.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("ClaimDueScheduledSends(due) error = %v", err)
	}
	if len(due) != 1 || due[0].ID != scheduled.ID {
		t.Fatalf("due = %#v, want scheduled row", due)
	}

	if err := db.CompleteScheduledSend(ctx, scheduled.ID, "<sent@example.com>"); err != nil {
		t.Fatalf("CompleteScheduledSend() error = %v", err)
	}
	got, err := db.GetScheduledSendByMessageID(ctx, msgID)
	if err != nil {
		t.Fatalf("GetScheduledSendByMessageID() error = %v", err)
	}
	if got.Status != ScheduledSendSent || got.SentMessageID != "<sent@example.com>" {
		t.Fatalf("completed scheduled send = %#v", got)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", got.AttemptCount)
	}
}

func TestResetStaleScheduledSends(t *testing.T) {
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
	scheduled, err := db.UpsertScheduledSend(ctx, "acc", msgID, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("UpsertScheduledSend() error = %v", err)
	}
	if _, err := db.ClaimDueScheduledSends(ctx, time.Now(), 1); err != nil {
		t.Fatalf("ClaimDueScheduledSends() error = %v", err)
	}
	if err := db.ResetStaleScheduledSends(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("ResetStaleScheduledSends() error = %v", err)
	}
	got, err := db.GetScheduledSendByMessageID(ctx, msgID)
	if err != nil {
		t.Fatalf("GetScheduledSendByMessageID() error = %v", err)
	}
	if got.ID != scheduled.ID || got.Status != ScheduledSendPending {
		t.Fatalf("reset scheduled send = %#v, want pending %s", got, scheduled.ID)
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
		AccountID:         "acc",
		FolderID:          "drafts",
		InternetMessageID: "<scheduled@example.com>",
		Subject:           "Scheduled draft",
		FromEmail:         "user@example.com",
		ToRecipients:      []Recipient{{Email: "friend@example.com"}},
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage(scheduled) error = %v", err)
	}
	if _, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID:         "acc",
		FolderID:          "drafts",
		InternetMessageID: "<plain-draft@example.com>",
		Subject:           "Plain draft",
		FromEmail:         "user@example.com",
	}); err != nil {
		t.Fatalf("SaveDraftMessage(plain) error = %v", err)
	}
	if _, err := db.UpsertScheduledSend(ctx, "acc", msgID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("UpsertScheduledSend() error = %v", err)
	}

	count, err := db.GetFolderEmailCountFilteredForUser(ctx, "default", "scheduled", models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetFolderEmailCountFilteredForUser() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("scheduled count = %d, want 1", count)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "scheduled", 0, 50, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].Subject != "Scheduled draft" {
		t.Fatalf("scheduled page = %#v", page)
	}
}
