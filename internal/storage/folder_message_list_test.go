package storage

import (
	"reflect"
	"strings"
	"testing"
)

type folderMessageRow struct {
	Message     int64
	Folder      string
	Date        string
	Attachments int64
}

func folderMessageRows(t *testing.T, db *DB) []folderMessageRow {
	t.Helper()
	rows, err := db.Read().Query(`SELECT message_id, folder_id, COALESCE(CAST(message_date AS TEXT), ''), message_has_attachments
		FROM message_folder_state ORDER BY message_id, folder_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []folderMessageRow
	for rows.Next() {
		var row folderMessageRow
		if err := rows.Scan(&row.Message, &row.Folder, &row.Date, &row.Attachments); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Folder states from before schema 125 get their message's date and
// attachment flag, and the index they replace goes.
func TestFolderMessageListUpgradeCopiesMessageDates(t *testing.T) {
	db := newFolderThreadTestDB(t)
	if _, err := db.Write().Exec(`
		DROP TRIGGER message_folder_state_copy_message_insert;
		DROP TRIGGER message_folder_state_copy_message_update;
		DROP TRIGGER message_folder_state_follow_message;
		DROP INDEX idx_message_folder_state_list;
		DROP INDEX idx_message_folder_state_flags;
		CREATE INDEX idx_folder_state_folder_unread ON message_folder_state(folder_id, is_deleted, is_read);
		ALTER TABLE message_folder_state DROP COLUMN message_date;
		ALTER TABLE message_folder_state DROP COLUMN message_has_attachments;
		INSERT INTO messages(id, account_id, date_received, has_attachments) VALUES
			(1, 'acct', '2024-01-01 10:00:00', 0),
			(2, 'acct', '2024-01-02 10:00:00', 1),
			(3, 'acct', NULL, 0);
		INSERT INTO message_folder_state(message_id, folder_id) VALUES
			(1, 'box-inbox'), (2, 'box-inbox'), (2, 'box-archive'), (3, 'box-inbox');
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES (124);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV124ToV125(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	want := []folderMessageRow{
		{1, "box-inbox", "2024-01-01 10:00:00", 0},
		{2, "box-archive", "2024-01-02 10:00:00", 1},
		{2, "box-inbox", "2024-01-02 10:00:00", 1},
		{3, "box-inbox", "", 0},
	}
	if got := folderMessageRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("after upgrade\n got: %+v\nwant: %+v", got, want)
	}
	var indexes []string
	rows, err := db.Read().Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'message_folder_state' AND name IN
		('idx_folder_state_folder_unread', 'idx_message_folder_state_list', 'idx_message_folder_state_flags') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	if want := []string{"idx_message_folder_state_flags", "idx_message_folder_state_list"}; !reflect.DeepEqual(indexes, want) {
		t.Fatalf("indexes = %v, want %v", indexes, want)
	}
}

// A folder state keeps its message's date and attachment flag through
// every write that can change them.
func TestFolderMessageListFollowsMessages(t *testing.T) {
	db := newFolderThreadTestDB(t)
	for step, tt := range []struct {
		action string
		want   []folderMessageRow
	}{
		{`INSERT INTO messages(id, account_id, date_received, has_attachments) VALUES (1, 'acct', '2024-01-01 10:00:00', 1);
		  INSERT INTO message_folder_state(message_id, folder_id) VALUES (1, 'box-inbox')`,
			[]folderMessageRow{{1, "box-inbox", "2024-01-01 10:00:00", 1}}},
		{`INSERT INTO message_folder_state(message_id, folder_id) VALUES (1, 'box-archive')
		  ON CONFLICT(message_id, folder_id) DO UPDATE SET is_read = 1`,
			[]folderMessageRow{{1, "box-archive", "2024-01-01 10:00:00", 1}, {1, "box-inbox", "2024-01-01 10:00:00", 1}}},
		{`UPDATE messages SET date_received = '2024-02-01 10:00:00', has_attachments = 0 WHERE id = 1`,
			[]folderMessageRow{{1, "box-archive", "2024-02-01 10:00:00", 0}, {1, "box-inbox", "2024-02-01 10:00:00", 0}}},
		{`INSERT INTO messages(id, account_id, date_received, has_attachments) VALUES (2, 'acct', '2024-03-01 10:00:00', 1);
		  UPDATE message_folder_state SET message_id = 2 WHERE message_id = 1 AND folder_id = 'box-archive'`,
			[]folderMessageRow{{1, "box-inbox", "2024-02-01 10:00:00", 0}, {2, "box-archive", "2024-03-01 10:00:00", 1}}},
		{`INSERT OR REPLACE INTO message_folder_state(message_id, folder_id, is_read) VALUES (2, 'box-archive', 1)`,
			[]folderMessageRow{{1, "box-inbox", "2024-02-01 10:00:00", 0}, {2, "box-archive", "2024-03-01 10:00:00", 1}}},
	} {
		if _, err := db.Write().Exec(tt.action); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if got := folderMessageRows(t, db); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("step %d\n got: %+v\nwant: %+v", step, got, tt.want)
		}
	}
}

// A page of a folder's messages, newest first, and its filtered counts are
// read from the folder states' indexes without sorting the folder.
func TestFolderMessageListIsReadFromIndexes(t *testing.T) {
	db := newFolderThreadTestDB(t)
	for _, query := range []string{
		`SELECT message_id FROM message_folder_state WHERE folder_id = 'box-inbox' AND is_deleted = 0
		 ORDER BY message_date DESC, message_id DESC LIMIT 50 OFFSET 100`,
		`SELECT COUNT(*) FROM message_folder_state WHERE folder_id = 'box-inbox' AND is_deleted = 0
		 AND is_read = 0 AND message_has_attachments = 1`,
	} {
		rows, err := db.Read().Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		text := strings.Join(plan, "\n")
		if !strings.Contains(text, "USING COVERING INDEX idx_message_folder_state_") || strings.Contains(text, "TEMP B-TREE") {
			t.Fatalf("query plan for\n%s\n%s", query, text)
		}
	}
}
