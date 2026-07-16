package storage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestParticipantFilterSearchesAllMailAndDeduplicatesFolderCopies(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('acc', 'default', 'outlook', 'owner@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_sent", AccountID: "acc", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "acc_archive", AccountID: "acc", Name: "Archive", Role: "archive", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{
		{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: "received",
			InternetMessageID: "<received@example.com>",
			Subject:           "Received from contact",
			FromName:          "Contact",
			FromEmail:         "PERSON@example.com",
			DateSent:          now,
			DateReceived:      now,
			ToRecipients:      []Recipient{{Email: "owner@example.com"}},
		},
		{
			AccountID:         "acc",
			FolderID:          "acc_sent",
			ProviderMessageID: "sent",
			InternetMessageID: "<sent@example.com>",
			Subject:           "Sent to contact",
			FromName:          "Owner",
			FromEmail:         "owner@example.com",
			DateSent:          now.Add(-time.Hour),
			DateReceived:      now.Add(-time.Hour),
			ToRecipients:      []Recipient{{Email: "person@example.com"}},
		},
	}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}

	receivedID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<received@example.com>")
	if err != nil || receivedID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(received) = %d, %v", receivedID, err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, is_read)
		VALUES (?, 'acc_archive', 1)`, receivedID); err != nil {
		t.Fatalf("insert duplicate archive state: %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 10, models.EmailFilters{Participant: " person@example.com "})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 2 || len(page.Emails) != 2 {
		t.Fatalf("participant page total=%d emails=%#v, want both sent and received mail once", page.TotalCount, page.Emails)
	}
	got := map[string]bool{}
	for _, email := range page.Emails {
		got[email.ID] = true
	}
	if !got[strconv.FormatInt(receivedID, 10)] {
		t.Fatalf("participant results missing received email %d: %#v", receivedID, page.Emails)
	}
	sentID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<sent@example.com>")
	if err != nil || !got[strconv.FormatInt(sentID, 10)] {
		t.Fatalf("participant results missing sent email %d (%v): %#v", sentID, err, page.Emails)
	}

	accountFolderPage, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "acc_inbox", 0, 10, models.EmailFilters{Participant: "person@example.com"})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser(account folder) error = %v", err)
	}
	if accountFolderPage.TotalCount != 2 || len(accountFolderPage.Emails) != 2 {
		t.Fatalf("account-folder participant page total=%d emails=%#v, want the same global contact activity", accountFolderPage.TotalCount, accountFolderPage.Emails)
	}
}
