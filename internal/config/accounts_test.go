package config

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newAccountStoreTestStore(t *testing.T) (*storage.DB, *AccountStore) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	return db, store
}

func seedAccountStoreTestUser(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func TestCreateAccountPurgesPendingDeletingAccount(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)

	email := "user@gmail.com"
	accountID := generateAccountID(email)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, is_deleting)
		VALUES (?, 'default', 'gmail', ?, 'Old Gmail', 1)`, accountID, email); err != nil {
		t.Fatalf("insert deleting account: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, remote_id, name)
		VALUES ('old_inbox', ?, 'INBOX', 'Inbox')`, accountID); err != nil {
		t.Fatalf("insert old folder: %v", err)
	}

	account, err := store.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider:     "gmail",
		EmailAddress: email,
		DisplayName:  "Fresh Gmail",
		Username:     email,
		AuthMethod:   "oauth2",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if account.ID != accountID {
		t.Fatalf("account.ID = %q, want %q", account.ID, accountID)
	}

	var isDeleting int
	var displayName string
	if err := db.Read().QueryRowContext(ctx, `SELECT is_deleting, display_name FROM accounts WHERE id = ?`, accountID).Scan(&isDeleting, &displayName); err != nil {
		t.Fatalf("query account: %v", err)
	}
	if isDeleting != 0 || displayName != "Fresh Gmail" {
		t.Fatalf("account state = deleting %d name %q, want active Fresh Gmail", isDeleting, displayName)
	}

	var folderCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE account_id = ?`, accountID).Scan(&folderCount); err != nil {
		t.Fatalf("query folders: %v", err)
	}
	if folderCount != 0 {
		t.Fatalf("folderCount = %d, want old folders purged", folderCount)
	}
}

func TestGetAccountByIDIgnoresDeletingAccount(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address, is_deleting)
		VALUES ('acc_deleting', 'default', 'gmail', 'user@gmail.com', 1)`); err != nil {
		t.Fatalf("insert deleting account: %v", err)
	}

	account, err := store.GetAccountByID(ctx, "acc_deleting")
	if err != nil {
		t.Fatalf("GetAccountByID() error = %v", err)
	}
	if account != nil {
		t.Fatalf("GetAccountByID() = %#v, want nil for deleting account", account)
	}
}

func TestDeleteAccountOnlyDeletesMarkedDeletingRows(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('acc_active', 'default', 'gmail', 'user@gmail.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	if err := store.DeleteAccount(ctx, "acc_active"); err != nil {
		t.Fatalf("DeleteAccount(active) error = %v", err)
	}
	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = 'acc_active'`).Scan(&count); err != nil {
		t.Fatalf("query active account: %v", err)
	}
	if count != 1 {
		t.Fatalf("active account count = %d, want 1", count)
	}

	if err := store.MarkAccountDeleting(ctx, "acc_active"); err != nil {
		t.Fatalf("MarkAccountDeleting() error = %v", err)
	}
	if err := store.DeleteAccount(ctx, "acc_active"); err != nil {
		t.Fatalf("DeleteAccount(deleting) error = %v", err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = 'acc_active'`).Scan(&count); err != nil {
		t.Fatalf("query deleted account: %v", err)
	}
	if count != 0 {
		t.Fatalf("deleted account count = %d, want 0", count)
	}
}

func TestDeleteAccountCleansAccountDataExplicitly(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)

	const accountID = "acc_delete"
	statements := []string{
		`INSERT INTO accounts (id, user_id, provider, email_address, is_deleting)
		 VALUES ('acc_delete', 'default', 'gmail', 'delete@example.com', 1)`,
		`INSERT INTO folders (id, account_id, remote_id, name)
		 VALUES ('acc_delete_inbox', 'acc_delete', 'INBOX', 'Inbox')`,
		`INSERT INTO messages (id, account_id, internet_message_id, subject, thread_id)
		 VALUES (1001, 'acc_delete', '<root@example.com>', 'Root', 'thread-delete')`,
		`INSERT INTO messages (id, account_id, internet_message_id, subject, thread_id, thread_parent_id)
		 VALUES (1002, 'acc_delete', '<child@example.com>', 'Child', 'thread-delete', 1001)`,
		`INSERT INTO threads (id, account_id, subject, normalized_subject, root_message_id)
		 VALUES ('thread-delete', 'acc_delete', 'Root', 'root', 1001)`,
		`INSERT INTO folder_thread_state (folder_id, thread_key, head_message_id, account_id)
		 VALUES ('acc_delete_inbox', 'thread-delete', 1002, 'acc_delete')`,
		`INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		 VALUES (1001, 'acc_delete_inbox', 1), (1002, 'acc_delete_inbox', 2)`,
		`INSERT INTO labels (id, account_id, name, provider_type)
		 VALUES ('acc_delete_label', 'acc_delete', 'Important', 'gmail')`,
		`INSERT INTO message_labels (message_id, label_id)
		 VALUES (1001, 'acc_delete_label')`,
		`INSERT INTO label_sync_state (account_id, provider_type, scope)
		 VALUES ('acc_delete', 'gmail', 'labels')`,
		`INSERT INTO label_mutation_queue (account_id, message_id, provider_type, operation, label_name)
		 VALUES ('acc_delete', 1001, 'gmail', 'add', 'Important')`,
		`INSERT INTO sync_state (account_id, folder_id)
		 VALUES ('acc_delete', 'acc_delete_inbox')`,
		`CREATE TABLE IF NOT EXISTS gmail_watch_state (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			topic_name TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO gmail_watch_state (account_id, topic_name)
		 VALUES ('acc_delete', 'topic')`,
		`INSERT INTO gmail_poll_state (account_id, profile_history_id)
		 VALUES ('acc_delete', 'history')`,
		`INSERT INTO message_recipients (message_id, kind, email)
		 VALUES (1001, 'to', 'to@example.com')`,
		`INSERT INTO attachments (message_id, filename, storage_path)
		 VALUES (1001, 'a.txt', '/tmp/a.txt')`,
		`INSERT INTO message_references (message_id, referenced_message_id, ordinal)
		 VALUES (1002, '<root@example.com>', 0)`,
		`INSERT INTO unresolved_references (account_id, referenced_message_id, child_message_id, ordinal)
		 VALUES ('acc_delete', '<missing@example.com>', 1002, 0)`,
		`INSERT INTO remote_content_messages (message_id)
		 VALUES (1001)`,
		`INSERT INTO scheduled_sends (id, account_id, message_id, scheduled_for)
		 VALUES ('scheduled-delete', 'acc_delete', 1001, CURRENT_TIMESTAMP)`,
		`INSERT INTO message_search(rowid, account_id, thread_key, subject, sender, recipients, snippet, body, attachment_names)
		 VALUES (1001, 'acc_delete', 'thread-delete', 'Root', 'sender', 'recipient', 'snippet', 'body', 'a.txt')`,
		`CREATE TABLE IF NOT EXISTS message_search_docs (
			message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
			account_id TEXT NOT NULL,
			subject TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO message_search_docs (message_id, account_id, subject)
		 VALUES (1001, 'acc_delete', 'Root')`,
		`INSERT INTO account_contact_sync_configs (account_id, user_id, provider)
		 VALUES ('acc_delete', 'default', 'gmail')`,
		`INSERT INTO account_contact_address_books (account_id, user_id, id, url)
		 VALUES ('acc_delete', 'default', 'book-delete', 'https://contacts.example/book')`,
		`INSERT INTO contacts (id, user_id, display_name)
		 VALUES ('contact-delete', 'default', 'Delete Contact')`,
		`INSERT INTO contact_sources (id, user_id, contact_id, provider, account_id, remote_id)
		 VALUES ('source-delete', 'default', 'contact-delete', 'gmail', 'acc_delete', 'remote-contact')`,
		`INSERT INTO contact_profiles (id, user_id, display_name)
		 VALUES ('profile-delete', 'default', 'Delete Profile')`,
		`INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, remote_id)
		 VALUES ('card-delete', 'default', 'profile-delete', 'provider', 'gmail', 'acc_delete', 'remote-card')`,
		`INSERT INTO contact_fields (id, user_id, profile_id, card_id, kind, value)
		 VALUES ('field-delete', 'default', 'profile-delete', 'card-delete', 'email', 'delete@example.com')`,
		`INSERT INTO contact_groups (id, user_id, provider, account_id, remote_id, name)
		 VALUES ('group-delete', 'default', 'gmail', 'acc_delete', 'remote-group', 'Group')`,
		`INSERT INTO contact_card_groups (card_id, group_id, user_id)
		 VALUES ('card-delete', 'group-delete', 'default')`,
		`INSERT INTO contact_save_targets (contact_id, user_id, target)
		 VALUES ('contact-delete', 'default', 'account:acc_delete')`,
		`INSERT INTO contact_conflicts (id, user_id, profile_id, account_id)
		 VALUES ('conflict-delete', 'default', 'profile-delete', 'acc_delete')`,
	}
	for _, stmt := range statements {
		if _, err := db.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed statement failed: %v\n%s", err, stmt)
		}
	}

	progress := make(map[string]int64)
	if err := store.DeleteAccountWithProgress(ctx, accountID, func(p AccountDeletionProgress) {
		progress[p.Step] += p.RowsAffected
	}); err != nil {
		t.Fatalf("DeleteAccountWithProgress() error = %v", err)
	}

	for _, check := range []struct {
		name  string
		query string
	}{
		{"accounts", `SELECT COUNT(*) FROM accounts WHERE id = 'acc_delete'`},
		{"folders", `SELECT COUNT(*) FROM folders WHERE account_id = 'acc_delete'`},
		{"messages", `SELECT COUNT(*) FROM messages WHERE account_id = 'acc_delete'`},
		{"message_folder_state", `SELECT COUNT(*) FROM message_folder_state WHERE folder_id = 'acc_delete_inbox'`},
		{"labels", `SELECT COUNT(*) FROM labels WHERE account_id = 'acc_delete'`},
		{"message_labels", `SELECT COUNT(*) FROM message_labels WHERE label_id = 'acc_delete_label'`},
		{"label_sync_state", `SELECT COUNT(*) FROM label_sync_state WHERE account_id = 'acc_delete'`},
		{"label_mutation_queue", `SELECT COUNT(*) FROM label_mutation_queue WHERE account_id = 'acc_delete'`},
		{"sync_state", `SELECT COUNT(*) FROM sync_state WHERE account_id = 'acc_delete'`},
		{"gmail_watch_state", `SELECT COUNT(*) FROM gmail_watch_state WHERE account_id = 'acc_delete'`},
		{"gmail_poll_state", `SELECT COUNT(*) FROM gmail_poll_state WHERE account_id = 'acc_delete'`},
		{"message_search", `SELECT COUNT(*) FROM message_search WHERE account_id = 'acc_delete'`},
		{"message_search_docs", `SELECT COUNT(*) FROM message_search_docs WHERE account_id = 'acc_delete'`},
		{"account_contact_sync_configs", `SELECT COUNT(*) FROM account_contact_sync_configs WHERE account_id = 'acc_delete'`},
		{"account_contact_address_books", `SELECT COUNT(*) FROM account_contact_address_books WHERE account_id = 'acc_delete'`},
		{"contact_sources", `SELECT COUNT(*) FROM contact_sources WHERE account_id = 'acc_delete'`},
		{"contact_cards", `SELECT COUNT(*) FROM contact_cards WHERE account_id = 'acc_delete'`},
		{"contact_groups", `SELECT COUNT(*) FROM contact_groups WHERE account_id = 'acc_delete'`},
		{"contact_conflicts", `SELECT COUNT(*) FROM contact_conflicts WHERE account_id = 'acc_delete'`},
	} {
		var count int
		if err := db.Read().QueryRowContext(ctx, check.query).Scan(&count); err != nil {
			t.Fatalf("query %s: %v", check.name, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", check.name, count)
		}
	}
	for _, step := range []string{"delete message folder state by folder", "delete messages", "delete contact cards", "delete account"} {
		if progress[step] == 0 {
			t.Fatalf("progress[%q] = 0, want row deletion progress", step)
		}
	}
}

func TestListDeletingAccountIDs(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address, is_deleting, updated_at)
		VALUES
			('acc_active', 'default', 'imap', 'active@example.com', 0, '2026-01-02 00:00:00'),
			('acc_deleting_b', 'default', 'gmail', 'b@example.com', 1, '2026-01-03 00:00:00'),
			('acc_deleting_a', 'default', 'gmail', 'a@example.com', 1, '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}

	ids, err := store.ListDeletingAccountIDs(ctx)
	if err != nil {
		t.Fatalf("ListDeletingAccountIDs() error = %v", err)
	}
	want := []string{"acc_deleting_a", "acc_deleting_b"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %#v, want %#v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %#v, want %#v", ids, want)
		}
	}
}
