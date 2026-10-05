package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// Inject a failure precisely when indexing starts, after the mail mutation has
// run. This avoids timing-dependent sleeps and exercises the real SQLite driver.
type searchFailureKey struct{}
type searchAfterDeleteKey struct{}
type searchAfterInsertKey struct{}
type searchFailureConnector struct{ dsn string }

func (c searchFailureConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c searchFailureConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &searchFailureConn{Conn: conn}, nil
}

type searchFailureConn struct{ driver.Conn }

func (c *searchFailureConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "DELETE FROM message_search") {
		if fail, ok := ctx.Value(searchFailureKey{}).(func() error); ok {
			return nil, fail()
		}
	}
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err == nil && (strings.HasPrefix(strings.TrimSpace(query), "DELETE FROM message_search") || strings.HasPrefix(strings.TrimSpace(query), "INSERT INTO message_search")) {
		return &searchFailureStmt{Stmt: stmt, query: query}, nil
	}
	return stmt, err
}

type searchFailureStmt struct {
	driver.Stmt
	query string
}

func (s *searchFailureStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	result, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	if err == nil && strings.HasPrefix(strings.TrimSpace(s.query), "DELETE FROM message_search") {
		if cancel, ok := ctx.Value(searchAfterDeleteKey{}).(func()); ok {
			cancel()
		}
	}
	if err == nil && strings.HasPrefix(strings.TrimSpace(s.query), "INSERT INTO message_search") {
		if fail, ok := ctx.Value(searchAfterInsertKey{}).(func() error); ok {
			err = fail()
		}
	}
	return result, err
}
func (c *searchFailureConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func TestMessageSearchBatchRollback(t *testing.T) {
	for _, provider := range []bool{false, true} {
		name := "imap"
		if provider {
			name = "provider"
		}
		t.Run(name, func(t *testing.T) {
			db := newContactsTestDB(t)
			ctx := t.Context()
			if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES ('acc','default','me@example.com')`); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "folder", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := db.UpsertSyncMessages(ctx, []SyncMessage{{AccountID: "acc", FolderID: "folder", MessageID: "<existing@example.com>", Subject: "originalsubject", RemoteUID: 1, DateSent: now}}); err != nil {
				t.Fatal(err)
			}
			useSearchFailureWriter(t, db)
			before := searchMutationSnapshot(t, db)
			indexErr := errors.New("second search document failed")
			inserts := 0
			ctx = context.WithValue(ctx, searchAfterInsertKey{}, func() error {
				inserts++
				if inserts == 2 {
					return indexErr
				}
				return nil
			})
			keys := []string{"<existing@example.com>", "<new-one@example.com>", "<new-two@example.com>"}
			var err error
			if provider {
				var msgs []ProviderSyncMessage
				for _, key := range keys {
					msgs = append(msgs, ProviderSyncMessage{AccountID: "acc", FolderID: "folder", InternetMessageID: key, ProviderMessageID: key, Subject: "changedsubject", DateSent: now})
				}
				_, err = db.UpsertProviderSyncMessages(ctx, msgs)
			} else {
				var msgs []SyncMessage
				for i, key := range keys {
					msgs = append(msgs, SyncMessage{AccountID: "acc", FolderID: "folder", MessageID: key, Subject: "changedsubject", RemoteUID: uint32(i + 1), DateSent: now})
				}
				err = db.UpsertSyncMessages(ctx, msgs)
			}
			if inserts != 2 || !errors.Is(err, indexErr) {
				t.Fatalf("indexed %d documents, error = %v", inserts, err)
			}
			if searchMutationSnapshot(t, db) != before {
				t.Fatal("part of the mail batch or search batch persisted after indexing failed")
			}
		})
	}
}
func (c *searchFailureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	// Deleting a draft used a direct statement before the atomicity fix.
	if strings.HasPrefix(strings.TrimSpace(query), "DELETE FROM message_search") {
		if fail, ok := ctx.Value(searchFailureKey{}).(func() error); ok {
			return nil, fail()
		}
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *searchFailureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func useSearchFailureWriter(t *testing.T, db *DB) {
	t.Helper()
	if err := db.write.Close(); err != nil {
		t.Fatal(err)
	}
	db.write = sql.OpenDB(searchFailureConnector{db.path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)&_texttotime=true"})
	db.write.SetMaxOpenConns(1)
}

func searchMutationSnapshot(t *testing.T, db *DB) string {
	t.Helper()
	snapshot := make(map[string][][]any)
	for _, table := range []string{"messages", "message_search", "message_recipients", "attachments", "message_folder_state", "threads", "message_references", "unresolved_references"} {
		rows, err := db.Write().Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMessageSearchMutationAtomicity(t *testing.T) {
	for _, failure := range []string{"index-error", "cancellation", "cancellation-after-index-delete"} {
		t.Run(failure, func(t *testing.T) {
			for _, operation := range []string{"draft-insert", "draft-update", "draft-delete", "imap-insert", "imap-update", "provider-insert", "provider-update", "body", "clear-body", "clear-data", "headers", "thread-headers", "recipients", "insert-attachments", "replace-attachments", "html-path", "owned-html-path"} {
				t.Run(operation, func(t *testing.T) {
					db := newContactsTestDB(t)
					ctx := t.Context()
					if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES ('acc','default','me@example.com')`); err != nil {
						t.Fatal(err)
					}
					if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "folder", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
						t.Fatal(err)
					}
					draft := DraftMessageInput{AccountID: "acc", FolderID: "folder", InternetMessageID: "<original@example.com>", Subject: "originalsubject", FromEmail: "me@example.com", Snippet: "originalsnippet", ToRecipients: []Recipient{{Email: "original@example.com"}}, Date: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
					id, err := db.SaveDraftMessage(ctx, draft)
					if err != nil {
						t.Fatal(err)
					}
					bodyPath := filepath.Join(t.TempDir(), "body.txt")
					htmlPath := filepath.Join(t.TempDir(), "body.html")
					if err := os.WriteFile(bodyPath, []byte("originalbody"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(htmlPath, []byte("<p>changedhtml</p>"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := db.UpdateMessageBodyInternal(ctx, id, bodyPath, "", "", "originalsnippet"); err != nil {
						t.Fatal(err)
					}
					if err := db.InsertAttachmentsInternal(ctx, id, []AttachmentRow{{Filename: "originalattachment.txt"}}); err != nil {
						t.Fatal(err)
					}
					useSearchFailureWriter(t, db)
					before := searchMutationSnapshot(t, db)
					canceled, cancel := context.WithCancel(ctx)
					defer cancel()
					indexErr := errors.New("injected index failure")
					wantErr := indexErr
					if strings.HasPrefix(failure, "cancellation") {
						wantErr = context.Canceled
					}
					fired := false
					ctx = context.WithValue(canceled, searchFailureKey{}, func() error {
						fired = true
						if failure == "cancellation" {
							cancel()
							return canceled.Err()
						}
						return indexErr
					})
					if failure == "cancellation-after-index-delete" {
						ctx = context.WithValue(canceled, searchAfterDeleteKey{}, func() {
							fired = true
							cancel()
						})
					}
					draft.Subject = "changedsubject"
					draft.ToRecipients = []Recipient{{Email: "changed@example.com"}}
					messageKey := draft.InternetMessageID
					if strings.HasSuffix(operation, "-insert") {
						messageKey = "<new@example.com>"
						draft.InternetMessageID = messageKey
					}
					mutate := func(ctx context.Context) error {
						var err error
						switch operation {
						case "draft-insert", "draft-update":
							_, err = db.SaveDraftMessage(ctx, draft)
						case "draft-delete":
							_, err = db.DeleteDraftMessage(ctx, "acc", messageKey)
						case "imap-insert", "imap-update":
							err = db.UpsertSyncMessages(ctx, []SyncMessage{{AccountID: "acc", FolderID: "folder", MessageID: messageKey, Subject: "changedsubject", FromEmail: "me@example.com", RemoteUID: 100, DateSent: draft.Date, ToRecipients: draft.ToRecipients}})
						case "provider-insert", "provider-update":
							_, err = db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{AccountID: "acc", FolderID: "folder", InternetMessageID: messageKey, ProviderMessageID: "remote-1", Subject: "changedsubject", FromEmail: "me@example.com", DateSent: draft.Date, ToRecipients: draft.ToRecipients}})
						case "body":
							err = db.UpdateMessageBodyInternal(ctx, id, "", htmlPath, "raw-path", "changedsnippet")
						case "clear-body":
							err = db.ClearEmailBody(ctx, id)
						case "clear-data":
							err = db.ClearEmailDataForUser(ctx, id, "default")
						case "headers":
							err = db.UpdateMessageHeaders(ctx, id, "changedsubject", "Changed", "changed@example.com", "changedsnippet")
						case "thread-headers":
							err = db.UpdateMessageThreadHeadersInternal(ctx, id, "acc", "<parent@example.com>", "<parent@example.com>", "changedsubject")
						case "recipients":
							err = db.UpsertRecipientsInternal(ctx, id, draft.ToRecipients, []Recipient{{Email: "cc@example.com"}})
						case "insert-attachments":
							err = db.InsertAttachmentsInternal(ctx, id, []AttachmentRow{{Filename: "changedattachment.txt"}})
						case "replace-attachments":
							err = db.ReplaceAttachmentsInternal(ctx, id, []AttachmentRow{{Filename: "changedattachment.txt"}})
						case "html-path":
							err = db.UpdateMessageBodyHTMLPath(ctx, id, htmlPath)
						case "owned-html-path":
							err = db.UpdateMessageBodyHTMLPathForUser(ctx, id, htmlPath, "default")
						}
						return err
					}
					err = mutate(ctx)
					// database/sql can finish its automatic rollback before Commit
					// observes the canceled context, returning ErrTxDone instead.
					validErr := errors.Is(err, wantErr) || (strings.HasPrefix(failure, "cancellation") && canceled.Err() != nil && errors.Is(err, sql.ErrTxDone))
					if !fired || !validErr {
						t.Fatalf("injection fired = %v, error = %v, want %v", fired, err, wantErr)
					}
					if after := searchMutationSnapshot(t, db); after != before {
						t.Fatal("mail data or search index changed despite failed indexing")
					}
					if err := mutate(t.Context()); err != nil {
						t.Fatalf("retry mutation: %v", err)
					}
					var missing, orphaned int
					if err := db.Write().QueryRow(`SELECT count(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM message_search s WHERE s.rowid=m.id)`).Scan(&missing); err != nil {
						t.Fatal(err)
					}
					if err := db.Write().QueryRow(`SELECT count(*) FROM message_search s WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id=s.rowid)`).Scan(&orphaned); err != nil {
						t.Fatal(err)
					}
					if missing != 0 || orphaned != 0 {
						t.Fatalf("after retry: %d missing documents, %d orphaned documents", missing, orphaned)
					}
					if _, err := db.Write().Exec(`INSERT INTO message_search(message_search) VALUES ('integrity-check')`); err != nil {
						t.Fatalf("FTS integrity after retry: %v", err)
					}
				})
			}
		})
	}
}
