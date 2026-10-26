package storage

import (
	"maps"
	"path/filepath"
	"testing"
)

// Folders from before have no role chosen, and only roles that can be chosen,
// each on a different listed folder of the owner's IMAP account, are kept.
func TestFolderRoleChoices(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('other','other','other');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES
  ('acc_a','owner','imap','ana@example.test'),('acc_b','owner','imap','ben@example.test'),
  ('acc_g','owner','gmail','gil@example.test'),('acc_o','other','imap','olga@example.test');
 INSERT INTO folders(id,account_id,remote_id,name,role,selectable) VALUES
  ('f_inbox','acc_a','INBOX','Inbox','inbox',1),
  ('f_bin','acc_a','Corbeille','Corbeille','custom',1),
  ('f_old','acc_a','Old deleted','Old deleted','custom',1),
  ('f_parent','acc_a','Projects','Projects','custom',0),
  ('f_b','acc_b','Corbeille','Corbeille','custom',1),
  ('f_g','acc_g','[Gmail]/Trash','Trash','trash',1);
 ALTER TABLE folders DROP COLUMN role_choice; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(129);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV129ToV130(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	choices := func() map[string]string {
		t.Helper()
		got, err := db.FolderRoleChoices(ctx, "acc_a")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := choices(); len(got) != 0 {
		t.Fatalf("after migration = %v", got)
	}

	if err := db.SetFolderRoleChoices(ctx, "owner", "acc_a", map[string]string{"trash": "f_bin", "archive": "f_old"}); err != nil {
		t.Fatal(err)
	}
	if got := choices(); !maps.Equal(got, map[string]string{"Corbeille": "trash", "Old deleted": "archive"}) {
		t.Fatalf("after choosing = %v", got)
	}

	for name, refused := range map[string]struct {
		user, account string
		choices       map[string]string
	}{
		"the inbox":           {"owner", "acc_a", map[string]string{"inbox": "f_old"}},
		"an unknown role":     {"owner", "acc_a", map[string]string{"starred": "f_old"}},
		"one folder twice":    {"owner", "acc_a", map[string]string{"trash": "f_old", "archive": "f_old"}},
		"another's folder":    {"owner", "acc_a", map[string]string{"trash": "f_b"}},
		"no such folder":      {"owner", "acc_a", map[string]string{"trash": "f_gone"}},
		"a folder of folders": {"owner", "acc_a", map[string]string{"trash": "f_parent"}},
		"another's account":   {"owner", "acc_o", map[string]string{}},
		"not IMAP":            {"owner", "acc_g", map[string]string{"trash": "f_g"}},
	} {
		if err := db.SetFolderRoleChoices(ctx, refused.user, refused.account, refused.choices); err != ErrFolderRoleChoice {
			t.Fatalf("%s: SetFolderRoleChoices = %v", name, err)
		}
	}
	if got := choices(); !maps.Equal(got, map[string]string{"Corbeille": "trash", "Old deleted": "archive"}) {
		t.Fatalf("after refused choices = %v", got)
	}

	if err := db.SetFolderRoleChoices(ctx, "owner", "acc_a", map[string]string{"trash": "f_old"}); err != nil {
		t.Fatal(err)
	}
	if got := choices(); !maps.Equal(got, map[string]string{"Old deleted": "trash"}) {
		t.Fatalf("after choosing again = %v", got)
	}
	if err := db.SetFolderRoleChoices(ctx, "owner", "acc_a", nil); err != nil {
		t.Fatal(err)
	}
	if got := choices(); len(got) != 0 {
		t.Fatalf("after clearing = %v", got)
	}
}
