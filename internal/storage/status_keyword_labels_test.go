package storage

import (
	"path/filepath"
	"testing"
)

// Labels saved from keywords that later count as status, like
// $HasNoAttachment, go at the next start, with their messages' links. Real
// keyword labels and other providers' labels stay. Both the single database
// and the per-user stores, opened with OpenExisting, clean up.
func TestOpenForgetsStatusKeywordLabels(t *testing.T) {
	for name, open := range map[string]func(string) (*DB, error){"New": New, "OpenExisting": OpenExisting} {
		t.Run(name, func(t *testing.T) { testOpenForgetsStatusKeywordLabels(t, open) })
	}
}

func testOpenForgetsStatusKeywordLabels(t *testing.T, open func(string) (*DB, error)) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, statement := range []string{
		`INSERT OR IGNORE INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default')`,
		`INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('work', 'default', 'mira@example.com', 'imap')`,
		`INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('home', 'default', 'mira@example.net', 'gmail')`,
		`INSERT INTO messages (id, account_id) VALUES (1, 'work')`,
		`INSERT INTO messages (id, account_id) VALUES (2, 'home')`,
		`INSERT INTO labels (id, account_id, name, provider_id, provider_type) VALUES ('no-attachment', 'work', '$hasnoattachment', '$hasnoattachment', 'imap_keyword')`,
		`INSERT INTO labels (id, account_id, name, provider_id, provider_type) VALUES ('trusted', 'work', '$IsTrusted', '$IsTrusted', 'imap_keyword')`,
		`INSERT INTO labels (id, account_id, name, provider_id, provider_type) VALUES ('clients', 'work', 'Clients', 'Clients', 'imap_keyword')`,
		`INSERT INTO labels (id, account_id, name, provider_id, provider_type) VALUES ('gmail-muted', 'home', '$Muted', 'Label_7', 'gmail')`,
		`INSERT INTO message_labels (message_id, label_id) VALUES (1, 'no-attachment')`,
		`INSERT INTO message_labels (message_id, label_id) VALUES (1, 'clients')`,
		`INSERT INTO message_labels (message_id, label_id) VALUES (2, 'gmail-muted')`,
		`INSERT INTO message_label_copies (message_id, label_id, remote_message_id) VALUES (1, 'no-attachment', 'INBOX:7')`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err = open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Read().Query(`SELECT id FROM labels ORDER BY id`)
	if err != nil {
		t.Fatalf("list labels: %v", err)
	}
	var labels []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		labels = append(labels, id)
	}
	rows.Close()
	if len(labels) != 2 || labels[0] != "clients" || labels[1] != "gmail-muted" {
		t.Fatalf("labels after reopen = %v, want [clients gmail-muted]", labels)
	}
	var links, copies int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&links); err != nil || links != 2 {
		t.Fatalf("message labels = %d, %v; want 2", links, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_label_copies`).Scan(&copies); err != nil || copies != 0 {
		t.Fatalf("message label copies = %d, %v; want 0", copies, err)
	}
}
