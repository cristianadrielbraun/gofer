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

func seedUserScopedMessageMutationTest(t *testing.T) (*DB, int64, int64) {
	t.Helper()
	ctx := t.Context()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name)
		VALUES ('other', 'other@example.com', 'Other');
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('owned-account', 'default', 'imap', 'default@example.com'),
		       ('other-account', 'other', 'imap', 'other@example.com');
		INSERT INTO folders (id, account_id, remote_id, name, role)
		VALUES ('owned-inbox', 'owned-account', 'INBOX', 'Inbox', 'inbox'),
		       ('owned-archive', 'owned-account', 'Archive', 'Archive', 'archive'),
		       ('other-inbox', 'other-account', 'INBOX', 'Inbox', 'inbox');
		INSERT INTO messages (
			id, account_id, internet_message_id, thread_id, subject, from_email, body_html_path
		) VALUES
			(701, 'owned-account', '<owned-mutation@example.com>', 'shared-thread', 'Owned', 'sender@example.com', 'owned-body.html'),
			(702, 'other-account', '<other-mutation@example.com>', 'shared-thread', 'Other', 'sender@example.com', 'other-body.html');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_starred)
		VALUES (701, 'owned-inbox', 71, 0, 0),
		       (702, 'other-inbox', 72, 0, 0);
		INSERT INTO attachments (message_id, filename, content_type, size_bytes, storage_path)
		VALUES (701, 'owned.txt', 'text/plain', 5, 'owned-attachment.txt')`); err != nil {
		t.Fatalf("seed message mutations: %v", err)
	}
	return db, 701, 702
}

func TestUserScopedMessageMutationLookupsRejectForeignUser(t *testing.T) {
	ctx := t.Context()
	db, ownedMessageID, _ := seedUserScopedMessageMutationTest(t)

	info, err := db.GetMessageMutationInfoForUser(ctx, ownedMessageID, "default")
	if err != nil || info == nil || info.AccountID != "owned-account" || info.ThreadID != "shared-thread" || info.IsRead || info.IsStarred {
		t.Fatalf("GetMessageMutationInfoForUser(owner) = %#v, %v", info, err)
	}
	info, err = db.GetMessageMutationInfoForUser(ctx, ownedMessageID, "other")
	if err != nil || info != nil {
		t.Fatalf("GetMessageMutationInfoForUser(other) = %#v, %v; want nil", info, err)
	}

	infos, err := db.GetThreadMutationInfosForUser(ctx, "owned-account", "shared-thread", "default")
	if err != nil || len(infos) != 1 || infos[0].MessageID != ownedMessageID {
		t.Fatalf("GetThreadMutationInfosForUser(owner) = %#v, %v", infos, err)
	}
	infos, err = db.GetThreadMutationInfosForUser(ctx, "owned-account", "shared-thread", "other")
	if err != nil || len(infos) != 0 {
		t.Fatalf("GetThreadMutationInfosForUser(other) = %#v, %v; want empty", infos, err)
	}
}

func TestUserScopedMessageMutationsRejectForeignUserWithoutPartialChanges(t *testing.T) {
	t.Run("mixed read batch rolls back", func(t *testing.T) {
		ctx := t.Context()
		db, ownedMessageID, foreignMessageID := seedUserScopedMessageMutationTest(t)

		if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{ownedMessageID, foreignMessageID}, true, "default"); err == nil {
			t.Fatal("SetMessagesReadAndQueueForUser() error = nil, want ownership failure")
		}

		var isRead, mutations int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT is_read FROM message_folder_state WHERE message_id = ? AND folder_id = 'owned-inbox'`,
			ownedMessageID,
		).Scan(&isRead); err != nil {
			t.Fatalf("query owned read state: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM message_mutations WHERE message_id IN (?, ?)`,
			ownedMessageID, foreignMessageID,
		).Scan(&mutations); err != nil {
			t.Fatalf("query queued mutations: %v", err)
		}
		if isRead != 0 || mutations != 0 {
			t.Fatalf("mixed read changed state is_read=%d mutations=%d", isRead, mutations)
		}
	})

	t.Run("mixed move batch rolls back", func(t *testing.T) {
		ctx := t.Context()
		db, ownedMessageID, foreignMessageID := seedUserScopedMessageMutationTest(t)

		if err := db.MoveMessagesAndQueueForUser(
			ctx,
			[]int64{ownedMessageID, foreignMessageID},
			"owned-inbox",
			"owned-archive",
			"default",
		); err == nil {
			t.Fatal("MoveMessagesAndQueueForUser() error = nil, want ownership failure")
		}

		var inboxDeleted, archiveRows, mutations int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'owned-inbox'`,
			ownedMessageID,
		).Scan(&inboxDeleted); err != nil {
			t.Fatalf("query owned inbox state: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'owned-archive'`,
			ownedMessageID,
		).Scan(&archiveRows); err != nil {
			t.Fatalf("query owned archive state: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM message_mutations WHERE message_id IN (?, ?)`,
			ownedMessageID, foreignMessageID,
		).Scan(&mutations); err != nil {
			t.Fatalf("query queued mutations: %v", err)
		}
		if inboxDeleted != 0 || archiveRows != 0 || mutations != 0 {
			t.Fatalf("mixed move changed state inbox_deleted=%d archive_rows=%d mutations=%d", inboxDeleted, archiveRows, mutations)
		}
	})

	t.Run("foreign permanent delete is rejected", func(t *testing.T) {
		ctx := t.Context()
		db, _, foreignMessageID := seedUserScopedMessageMutationTest(t)

		if err := db.PermanentlyDeleteMessageAndQueueForUser(ctx, foreignMessageID, "other-inbox", "default"); err == nil {
			t.Fatal("PermanentlyDeleteMessageAndQueueForUser() error = nil, want ownership failure")
		}

		var isDeleted, mutations int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'other-inbox'`,
			foreignMessageID,
		).Scan(&isDeleted); err != nil {
			t.Fatalf("query foreign delete state: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM message_mutations WHERE message_id = ?`,
			foreignMessageID,
		).Scan(&mutations); err != nil {
			t.Fatalf("query foreign delete mutations: %v", err)
		}
		if isDeleted != 0 || mutations != 0 {
			t.Fatalf("foreign delete changed state is_deleted=%d mutations=%d", isDeleted, mutations)
		}
	})

	t.Run("foreign content writes are no-ops", func(t *testing.T) {
		ctx := t.Context()
		db, ownedMessageID, _ := seedUserScopedMessageMutationTest(t)

		if err := db.ClearEmailDataForUser(ctx, ownedMessageID, "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("ClearEmailDataForUser(other) error = %v, want sql.ErrNoRows", err)
		}
		if err := db.UpdateMessageBodyHTMLPathForUser(ctx, ownedMessageID, "attacker.html", "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("UpdateMessageBodyHTMLPathForUser(other) error = %v, want sql.ErrNoRows", err)
		}
		if err := db.AllowRemoteContentForMessageForUser(ctx, ownedMessageID, "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("AllowRemoteContentForMessageForUser(other) error = %v, want sql.ErrNoRows", err)
		}
		if err := db.AllowRemoteContentForSenderFromMessageForUser(ctx, ownedMessageID, "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("AllowRemoteContentForSenderFromMessageForUser(other) error = %v, want sql.ErrNoRows", err)
		}

		var bodyPath string
		var attachments, messageAllows, senderAllows, senderMarkers int
		if err := db.Read().QueryRowContext(ctx, `SELECT body_html_path FROM messages WHERE id = ?`, ownedMessageID).Scan(&bodyPath); err != nil {
			t.Fatalf("query body path: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id = ?`, ownedMessageID).Scan(&attachments); err != nil {
			t.Fatalf("query attachments: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_content_messages WHERE message_id = ?`, ownedMessageID).Scan(&messageAllows); err != nil {
			t.Fatalf("query message remote-content allows: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_content_senders`).Scan(&senderAllows); err != nil {
			t.Fatalf("query sender remote-content allows: %v", err)
		}
		if err := db.Read().QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM app_settings
			WHERE key LIKE 'remote_content_sender_allow_%'`,
		).Scan(&senderMarkers); err != nil {
			t.Fatalf("query sender remote-content markers: %v", err)
		}
		if bodyPath != "owned-body.html" || attachments != 1 || messageAllows != 0 || senderAllows != 0 || senderMarkers != 0 {
			t.Fatalf("foreign content write changed state body=%q attachments=%d message_allows=%d sender_allows=%d sender_markers=%d",
				bodyPath, attachments, messageAllows, senderAllows, senderMarkers)
		}
	})

	t.Run("sender allow is effective only for the approving user", func(t *testing.T) {
		ctx := t.Context()
		db, ownedMessageID, foreignMessageID := seedUserScopedMessageMutationTest(t)

		if err := db.AllowRemoteContentForSenderFromMessageForUser(ctx, ownedMessageID, "default"); err != nil {
			t.Fatalf("AllowRemoteContentForSenderFromMessageForUser(owner): %v", err)
		}
		if !db.IsRemoteContentAllowedForSenderForUser(ctx, "sender@example.com", "default") {
			t.Fatal("owner sender allow = false, want true")
		}
		if db.IsRemoteContentAllowedForSenderForUser(ctx, "sender@example.com", "other") {
			t.Fatal("other sender allow = true before approval, want false")
		}

		if err := db.AllowRemoteContentForMessageForUser(ctx, foreignMessageID, "other"); err != nil {
			t.Fatalf("AllowRemoteContentForMessageForUser(other): %v", err)
		}
		if db.IsRemoteContentAllowedForSenderForUser(ctx, "sender@example.com", "other") {
			t.Fatal("other sender allow = true after message-only approval, want false")
		}
		if err := db.AllowRemoteContentForSenderFromMessageForUser(ctx, foreignMessageID, "other"); err != nil {
			t.Fatalf("AllowRemoteContentForSenderFromMessageForUser(other): %v", err)
		}
		if !db.IsRemoteContentAllowedForSenderForUser(ctx, "sender@example.com", "other") {
			t.Fatal("other sender allow = false after approval, want true")
		}
	})
}
