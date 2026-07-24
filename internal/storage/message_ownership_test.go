package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserScopedMessageReadsRejectForeignUser(t *testing.T) {
	ctx := t.Context()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name)
		VALUES ('other', 'other@example.com', 'Other')`); err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('owned-account', 'default', 'imap', 'default@example.com'),
		       ('other-account', 'other', 'imap', 'other@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, remote_id, name, role)
		VALUES ('owned-inbox', 'owned-account', 'INBOX', 'Inbox', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	bodyPath := filepath.Join(t.TempDir(), "body.html")
	originalPath := filepath.Join(t.TempDir(), "body-original.html")
	rawPath := filepath.Join(t.TempDir(), "raw.eml")
	if err := os.WriteFile(bodyPath, []byte("<p>owned body</p>"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := os.WriteFile(originalPath, []byte("<p>owned original</p>"), 0o600); err != nil {
		t.Fatalf("write original body: %v", err)
	}
	if err := os.WriteFile(rawPath, []byte("Subject: owned\r\n\r\nowned raw"), 0o600); err != nil {
		t.Fatalf("write raw message: %v", err)
	}

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (
			id, account_id, internet_message_id, thread_id, subject, from_email,
			snippet, body_html_path, body_html_original_path, raw_path
		) VALUES (
			501, 'owned-account', '<owned@example.com>', 'owned-thread', 'Owned',
			'sender@example.com', 'owned preview', ?, ?, ?
		)`, bodyPath, originalPath, rawPath,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		VALUES (501, 'owned-inbox', 77)`); err != nil {
		t.Fatalf("insert folder state: %v", err)
	}

	email, err := db.GetEmailByIDForUser(ctx, "501", "default")
	if err != nil || email == nil || email.Subject != "Owned" {
		t.Fatalf("GetEmailByIDForUser(owner) = %#v, %v", email, err)
	}
	email, err = db.GetEmailByIDForUser(ctx, "501", "other")
	if err != nil || email != nil {
		t.Fatalf("GetEmailByIDForUser(other) = %#v, %v; want nil", email, err)
	}

	email, err = db.GetEmailByIDForFolderForUser(ctx, "501", "owned-inbox", "default")
	if err != nil || email == nil || email.FolderID != "owned-inbox" {
		t.Fatalf("GetEmailByIDForFolderForUser(owner) = %#v, %v", email, err)
	}
	email, err = db.GetEmailByIDForFolderForUser(ctx, "501", "owned-inbox", "other")
	if err != nil || email != nil {
		t.Fatalf("GetEmailByIDForFolderForUser(other) = %#v, %v; want nil", email, err)
	}

	storageInfo, err := db.GetMessageStorageInfoForUser(ctx, 501, "default")
	if err != nil || storageInfo == nil || storageInfo.AccountID != "owned-account" || storageInfo.RawPath != rawPath {
		t.Fatalf("GetMessageStorageInfoForUser(owner) = %#v, %v", storageInfo, err)
	}
	storageInfo, err = db.GetMessageStorageInfoForUser(ctx, 501, "other")
	if err != nil || storageInfo != nil {
		t.Fatalf("GetMessageStorageInfoForUser(other) = %#v, %v; want nil", storageInfo, err)
	}

	fetchInfo, err := db.GetMessageFetchInfoForUser(ctx, 501, "default")
	if err != nil || fetchInfo == nil || fetchInfo.AccountID != "owned-account" || fetchInfo.RemoteUID != 77 {
		t.Fatalf("GetMessageFetchInfoForUser(owner) = %#v, %v", fetchInfo, err)
	}
	fetchInfo, err = db.GetMessageFetchInfoForUser(ctx, 501, "other")
	if err != nil || fetchInfo != nil {
		t.Fatalf("GetMessageFetchInfoForUser(other) = %#v, %v; want nil", fetchInfo, err)
	}

	body, err := db.GetEmailBodyForUser(ctx, "501", "default")
	if err != nil || !strings.Contains(string(body), "owned body") {
		t.Fatalf("GetEmailBodyForUser(owner) = %q, %v", body, err)
	}
	body, err = db.GetEmailBodyForUser(ctx, "501", "other")
	if err != nil || body != nil {
		t.Fatalf("GetEmailBodyForUser(other) = %q, %v; want nil", body, err)
	}

	original, err := db.GetEmailOriginalHTMLBodyForUser(ctx, "501", "default")
	if err != nil || !strings.Contains(string(original), "owned original") {
		t.Fatalf("GetEmailOriginalHTMLBodyForUser(owner) = %q, %v", original, err)
	}
	original, err = db.GetEmailOriginalHTMLBodyForUser(ctx, "501", "other")
	if err != nil || original != nil {
		t.Fatalf("GetEmailOriginalHTMLBodyForUser(other) = %q, %v; want nil", original, err)
	}

	sender, err := db.GetMessageSenderEmailForUser(ctx, 501, "default")
	if err != nil || sender != "sender@example.com" {
		t.Fatalf("GetMessageSenderEmailForUser(owner) = %q, %v", sender, err)
	}
	sender, err = db.GetMessageSenderEmailForUser(ctx, 501, "other")
	if !errors.Is(err, sql.ErrNoRows) || sender != "" {
		t.Fatalf("GetMessageSenderEmailForUser(other) = %q, %v; want sql.ErrNoRows", sender, err)
	}

	accountID, err := db.GetThreadAccountIDForUser(ctx, "owned-thread", "default")
	if err != nil || accountID != "owned-account" {
		t.Fatalf("GetThreadAccountIDForUser(owner) = %q, %v", accountID, err)
	}
	accountID, err = db.GetThreadAccountIDForUser(ctx, "owned-thread", "other")
	if err != nil || accountID != "" {
		t.Fatalf("GetThreadAccountIDForUser(other) = %q, %v; want empty", accountID, err)
	}

	thread, err := db.GetThreadMessagesForUser(ctx, "owned-account", "owned-thread", "default")
	if err != nil || len(thread) != 1 || thread[0].Subject != "Owned" {
		t.Fatalf("GetThreadMessagesForUser(owner) = %#v, %v", thread, err)
	}
	thread, err = db.GetThreadMessagesForUser(ctx, "owned-account", "owned-thread", "other")
	if err != nil || len(thread) != 0 {
		t.Fatalf("GetThreadMessagesForUser(other) = %#v, %v; want empty", thread, err)
	}
}

func TestUserScopedMessageReadsRejectDeletingAccount(t *testing.T) {
	ctx := t.Context()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, email_address, is_deleting)
		VALUES ('deleting-account', 'default', 'default@example.com', 1);
		INSERT INTO messages (id, account_id, subject)
		VALUES (601, 'deleting-account', 'Deleting')`); err != nil {
		t.Fatalf("insert deleting message: %v", err)
	}

	email, err := db.GetEmailByIDForUser(ctx, "601", "default")
	if err != nil || email != nil {
		t.Fatalf("GetEmailByIDForUser(deleting) = %#v, %v; want nil", email, err)
	}
	info, err := db.GetMessageStorageInfoForUser(ctx, 601, "default")
	if err != nil || info != nil {
		t.Fatalf("GetMessageStorageInfoForUser(deleting) = %#v, %v; want nil", info, err)
	}
}
