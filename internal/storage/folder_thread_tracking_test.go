package storage

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newFolderThreadTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users(id, username, username_normalized) VALUES ('reader', 'reader', 'reader');
		INSERT INTO accounts(id, user_id, email_address) VALUES ('acct', 'reader', 'reader@example.test');
		INSERT INTO folders(id, account_id, remote_id, name, role) VALUES
			('box-inbox', 'acct', 'INBOX', 'Inbox', 'inbox'),
			('box-archive', 'acct', 'Archive', 'Archive', 'archive'),
			('box-trash', 'acct', 'Trash', 'Trash', 'trash');`); err != nil {
		t.Fatal(err)
	}
	return db
}

type folderThreadRow struct {
	Folder, Key, Account, Last          string
	Head, Count, Read, Starred, Attachs int64
}

func folderThreadRows(t *testing.T, db *DB) []folderThreadRow {
	t.Helper()
	rows, err := db.Read().Query(`SELECT folder_id, thread_key, head_message_id, account_id, COALESCE(CAST(last_message_at AS TEXT), ''),
		thread_count, thread_is_read, thread_is_starred, thread_has_attachments
		FROM folder_thread_state ORDER BY folder_id, thread_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []folderThreadRow
	for rows.Next() {
		var r folderThreadRow
		if err := rows.Scan(&r.Folder, &r.Key, &r.Head, &r.Account, &r.Last, &r.Count, &r.Read, &r.Starred, &r.Attachs); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rebuiltFolderThreadRows computes every folder from scratch in a transaction
// that is rolled back, leaving the incremental state untouched.
func rebuiltFolderThreadRows(t *testing.T, db *DB) []folderThreadRow {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, folder := range []string{"box-inbox", "box-archive", "box-trash"} {
		if err := rebuildFolderThreadStateTx(ctx, tx, folder); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.Query(`SELECT folder_id, thread_key, head_message_id, account_id, COALESCE(CAST(last_message_at AS TEXT), ''),
		thread_count, thread_is_read, thread_is_starred, thread_has_attachments
		FROM folder_thread_state ORDER BY folder_id, thread_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []folderThreadRow
	for rows.Next() {
		var r folderThreadRow
		if err := rows.Scan(&r.Folder, &r.Key, &r.Head, &r.Account, &r.Last, &r.Count, &r.Read, &r.Starred, &r.Attachs); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFolderThreadStateIncrementalMatchesFullRebuild(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	random := rand.New(rand.NewSource(7))
	folders := []string{"box-inbox", "box-archive", "box-trash"}
	threads := []string{"", "", "thread-a", "thread-b", "thread-c"}
	nextID := int64(1)
	var ids []int64
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Write().Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	pick := func() int64 { return ids[random.Intn(len(ids))] }

	for step := 0; step < 600; step++ {
		var action string
		switch op := random.Intn(10); {
		case op < 3 || len(ids) < 5:
			action = "new message"
			id := nextID
			nextID++
			ids = append(ids, id)
			exec(`INSERT INTO messages(id, account_id, internet_message_id, thread_id, subject, date_received, has_attachments)
				VALUES (?, 'acct', ?, NULLIF(?, ''), 'Hello', datetime('2024-01-01', '+' || ? || ' minutes'), ?)`,
				id, fmt.Sprint("<", id, "@example.test>"), threads[random.Intn(len(threads))], random.Intn(500), random.Intn(2))
			exec(`INSERT INTO message_folder_state(message_id, folder_id, remote_uid, is_read, is_starred)
				VALUES (?, ?, ?, ?, ?)`, id, folders[random.Intn(len(folders))], id, random.Intn(2), random.Intn(2))
		case op == 3:
			action = "read flag"
			exec(`UPDATE message_folder_state SET is_read = 1 - is_read WHERE message_id = ?`, pick())
		case op == 4:
			action = "star flag"
			exec(`UPDATE message_folder_state SET is_starred = 1 - is_starred WHERE message_id = ?`, pick())
		case op == 5:
			action = "move"
			exec(`UPDATE OR REPLACE message_folder_state SET folder_id = ? WHERE message_id = ?`, folders[random.Intn(len(folders))], pick())
		case op == 6:
			action = "hide or restore"
			exec(`UPDATE message_folder_state SET is_deleted = 1 - is_deleted WHERE message_id = ?`, pick())
		case op == 7:
			action = "regroup conversation"
			exec(`UPDATE messages SET thread_id = NULLIF(?, '') WHERE id = ?`, threads[random.Intn(len(threads))], pick())
		case op == 8:
			action = "copy to another folder"
			exec(`INSERT OR REPLACE INTO message_folder_state(message_id, folder_id, is_read) VALUES (?, ?, ?)`, pick(), folders[random.Intn(len(folders))], random.Intn(2))
		default:
			action = "delete message"
			id := pick()
			exec(`DELETE FROM messages WHERE id = ?`, id)
			for i, existing := range ids {
				if existing == id {
					ids = append(ids[:i], ids[i+1:]...)
					break
				}
			}
		}
		if random.Intn(3) == 0 {
			continue // Let changes accumulate across several writes.
		}
		if err := db.RefreshFolderThreadState(ctx, folders[random.Intn(len(folders))]); err != nil {
			t.Fatal(err)
		}
		got, want := folderThreadRows(t, db), rebuiltFolderThreadRows(t, db)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d (%s): incremental summaries differ from a rebuild\n got: %+v\nwant: %+v", step, action, got, want)
		}
	}
}

func TestFolderThreadStateUpgradeRebuildsEachFolderOnFirstUse(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(withoutFolderThreadTracking + `;
		INSERT INTO messages(id, account_id, thread_id, date_received) VALUES
			(1, 'acct', 'thread-a', '2024-01-01 10:00:00'),
			(2, 'acct', 'thread-a', '2024-01-02 10:00:00'),
			(3, 'acct', NULL, '2024-01-03 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id, is_read) VALUES (1, 'box-inbox', 1), (2, 'box-inbox', 0), (3, 'box-inbox', 1);
		-- A summary left behind by an older release that is no longer right.
		INSERT INTO folder_thread_state(folder_id, thread_key, head_message_id, account_id, thread_count)
			VALUES ('box-inbox', 'thread-a', 1, 'acct', 1);
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES (107);`); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// A new message in a folder that was never rebuilt must not make its
	// stale summary look complete.
	if _, err := db.Write().Exec(`INSERT INTO messages(id, account_id, date_received) VALUES (4, 'acct', '2024-01-04 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id) VALUES (4, 'box-inbox');`); err != nil {
		t.Fatal(err)
	}
	page, err := db.GetEmailsRange(ctx, "box-inbox", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 3 || len(page.Emails) != 3 {
		t.Fatalf("inbox after upgrade lists %d of %d conversations, want 3", len(page.Emails), page.TotalCount)
	}
	if got, want := folderThreadRows(t, db), rebuiltFolderThreadRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("upgraded summaries differ from a rebuild\n got: %+v\nwant: %+v", got, want)
	}
	pending, err := db.folderThreadStatePending(ctx, []string{"box-inbox"})
	if err != nil || pending {
		t.Fatalf("inbox still pending after its first use: %v %v", pending, err)
	}
	// Folders never opened keep waiting, without making reads write.
	pending, err = db.folderThreadStatePending(ctx, nil)
	if err != nil || pending {
		t.Fatalf("unopened folders make every read flush: %v %v", pending, err)
	}
}

func TestFolderThreadStateReadsDoNotWriteWhenCurrent(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO messages(id, account_id, date_received) VALUES (1, 'acct', '2024-01-01 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id) VALUES (1, 'box-inbox');`); err != nil {
		t.Fatal(err)
	}
	pending, err := db.folderThreadStatePending(ctx, nil)
	if err != nil || !pending {
		t.Fatalf("new message not recorded: %v %v", pending, err)
	}
	if _, err := db.GetEmailsRange(ctx, "box-inbox", 0, 50); err != nil {
		t.Fatal(err)
	}
	pending, err = db.folderThreadStatePending(ctx, []string{"box-inbox"})
	if err != nil || pending {
		t.Fatalf("listing left changes pending: %v %v", pending, err)
	}
	// Timestamps-only sync updates are not conversation changes.
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET synced_at = CURRENT_TIMESTAMP, remote_uid = 9`); err != nil {
		t.Fatal(err)
	}
	pending, err = db.folderThreadStatePending(ctx, nil)
	if err != nil || pending {
		t.Fatalf("sync bookkeeping recorded as a change: %v %v", pending, err)
	}
}

func TestSchemaFileHasFolderThreadTracking(t *testing.T) {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), strings.TrimSpace(folderThreadTrackingSchema)) {
		t.Fatal("schema.sql and folderThreadTrackingSchema differ; new databases and upgraded ones must match")
	}
}

// A conversation counts all of its messages wherever they are filed, as the
// reading pane shows it: a received message and the reply in Sent are two in
// the Inbox list, and the Conversations filter agrees.
func TestConversationCountIncludesMessagesInOtherFolders(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	exec := func(query string) {
		t.Helper()
		if _, err := db.Write().Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO folders(id, account_id, remote_id, name, role) VALUES ('box-sent', 'acct', 'Sent', 'Sent', 'sent');
		INSERT INTO messages(id, account_id, thread_id, subject, date_received) VALUES
			(1, 'acct', 'thread-a', 'Plans', '2024-01-01 10:00:00'),
			(3, 'acct', NULL, 'Alone', '2024-01-03 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id) VALUES (1, 'box-inbox'), (3, 'box-inbox');`)

	// Lists answered from summaries, and from the general query that a size
	// sort needs.
	lists := []models.EmailFilters{{}, {SortBy: "size", SortOrder: "desc"}}
	check := func(step string, want int) {
		t.Helper()
		for _, filters := range lists {
			page, err := db.GetEmailsRangeFiltered(ctx, "box-inbox", 0, 50, filters)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, email := range page.Emails {
				counts[email.ID] = max(email.ThreadCount, 1)
			}
			if counts["1"] != want || counts["3"] != 1 {
				t.Fatalf("%s, %+v: inbox conversation counts %v, want 1:%d 3:1", step, filters, counts, want)
			}
			filters.ThreadsOnly = true
			conversations, err := db.GetFolderEmailCountFiltered(ctx, "box-inbox", filters)
			if err != nil {
				t.Fatal(err)
			}
			if wantConversations := map[bool]int{true: 1, false: 0}[want > 1]; conversations != wantConversations {
				t.Fatalf("%s, %+v: Conversations filter counts %d, want %d", step, filters, conversations, wantConversations)
			}
		}
		email, err := db.GetEmailByIDForFolder(ctx, "1", "box-inbox")
		if err != nil {
			t.Fatal(err)
		}
		if got := max(email.ThreadCount, 1); got != want {
			t.Fatalf("%s: reading pane counts %d, want %d like the list", step, got, want)
		}
	}

	check("before the reply", 1)
	exec(`INSERT INTO messages(id, account_id, thread_id, subject, date_received) VALUES (2, 'acct', 'thread-a', 'Re: Plans', '2024-01-02 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id) VALUES (2, 'box-sent');`)
	check("after the reply in Sent", 2)
	exec(`UPDATE message_folder_state SET is_deleted = 1 WHERE message_id = 2`)
	check("after the reply is removed", 1)
	// The reading pane leaves it out too, as the count does.
	if thread, err := db.GetThreadMessages(ctx, "acct", "thread-a"); err != nil || len(thread) != 1 {
		t.Fatalf("thread with the reply removed = %d messages, %v; want 1", len(thread), err)
	}
	exec(`UPDATE message_folder_state SET is_deleted = 0 WHERE message_id = 2`)
	check("after the reply is back", 2)
	exec(`UPDATE messages SET thread_id = 'thread-b' WHERE id = 2`)
	check("after the reply leaves the conversation", 1)
	exec(`UPDATE messages SET thread_id = 'thread-a' WHERE id = 2`)
	check("after the reply rejoins the conversation", 2)
	exec(`DELETE FROM messages WHERE id = 2`)
	check("after the reply is deleted", 1)
}

// Summaries from schema 144 counted only the folder's messages; after the
// upgrade each folder is rebuilt on first use with whole conversations.
func TestFolderThreadStateUpgradeCountsWholeConversations(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO messages(id, account_id, thread_id, date_received) VALUES
			(1, 'acct', 'thread-a', '2024-01-01 10:00:00'),
			(2, 'acct', 'thread-a', '2024-01-02 10:00:00');
		INSERT INTO message_folder_state(message_id, folder_id) VALUES (1, 'box-inbox'), (2, 'box-archive');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetEmailsRange(ctx, "box-inbox", 0, 50); err != nil {
		t.Fatal(err)
	}
	// What schema 144 left: the inbox counted only its own message.
	if _, err := db.Write().Exec(withoutConversationTracking() + `;
		UPDATE folder_thread_state SET thread_count = 1;
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES (144);`); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	page, err := db.GetEmailsRange(ctx, "box-inbox", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Emails) != 1 || page.Emails[0].ThreadCount != 2 {
		t.Fatalf("inbox after upgrade = %+v, want one conversation of 2", page.Emails)
	}
	if _, err := db.GetEmailsRange(ctx, "box-archive", 0, 50); err != nil {
		t.Fatal(err)
	}
	if got, want := folderThreadRows(t, db), rebuiltFolderThreadRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("upgraded summaries differ from a rebuild\n got: %+v\nwant: %+v", got, want)
	}
}

// folderThreadTrackingV144 is folderThreadTrackingSchema as schema 144 had
// it: folder changes only, without the changed conversations.
func folderThreadTrackingV144() string {
	schema := regexp.MustCompile(`(?s)-- Conversations whose set.*?WITHOUT ROWID;\n\n`).ReplaceAllString(folderThreadTrackingSchema, "")
	return regexp.MustCompile(`(?s)    INSERT INTO folder_conversation_dirty.*?ON CONFLICT DO NOTHING;\n`).ReplaceAllString(schema, "")
}

// withoutConversationTracking turns a current database into the schema 144
// shape of its conversation summaries.
func withoutConversationTracking() string {
	drops := ""
	for _, name := range folderThreadTriggers {
		drops += "DROP TRIGGER " + name + ";\n"
	}
	return drops + "DROP TABLE folder_conversation_dirty;\n" + folderThreadTrackingV144()
}
