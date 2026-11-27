package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestAccountGroupsCRUDHierarchyAndContainerOrdering(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users(id, username, username_normalized) VALUES
			('owner', 'owner', 'owner'),
			('other', 'other', 'other');
		INSERT INTO accounts(id, user_id, provider, email_address, display_name, sort_order) VALUES
			('acc_root', 'owner', 'imap', 'root@example.test', 'Root', 0),
			('acc_work', 'owner', 'imap', 'work@example.test', 'Work Mail', 1),
			('acc_sub', 'owner', 'imap', 'sub@example.test', 'Sub Mail', 2),
			('acc_local', 'owner', 'local', 'local@gofer.local', 'Local folders', 3),
			('acc_other', 'other', 'imap', 'other@example.test', 'Other', 0);
	`); err != nil {
		t.Fatal(err)
	}

	work, err := db.CreateAccountGroup(ctx, "owner", "", "Work", "proxy_work")
	if err != nil {
		t.Fatalf("CreateAccountGroup(Work) error = %v", err)
	}
	if _, err := db.CreateAccountGroup(ctx, "owner", "", "work", ""); !errors.Is(err, ErrAccountGroupNameTaken) {
		t.Fatalf("duplicate root group error = %v, want ErrAccountGroupNameTaken", err)
	}
	if _, err := db.CreateAccountGroup(ctx, "owner", "", "   ", ""); !errors.Is(err, ErrAccountGroupNameEmpty) {
		t.Fatalf("empty name error = %v, want ErrAccountGroupNameEmpty", err)
	}

	eng, err := db.CreateAccountGroup(ctx, "owner", work.ID, "Engineering", "")
	if err != nil {
		t.Fatalf("CreateAccountGroup(Engineering) error = %v", err)
	}
	// Same name under a different parent is allowed; create a colliding top-level sibling too
	// so deleting Work proves lifted subgroups disambiguate colliding names.
	childWork, err := db.CreateAccountGroup(ctx, "owner", work.ID, "Work", "")
	if err != nil {
		t.Fatalf("CreateAccountGroup(child Work) error = %v", err)
	}
	topDup, err := db.CreateAccountGroup(ctx, "owner", "", "Engineering", "")
	if err != nil {
		t.Fatalf("CreateAccountGroup(top Engineering) error = %v", err)
	}

	// Cycle detection: Work cannot move inside itself or its child Engineering.
	if _, _, err := db.UpdateAccountGroup(ctx, "owner", work.ID, "Work", work.ID); !errors.Is(err, ErrAccountGroupCycle) {
		t.Fatalf("self parent error = %v, want ErrAccountGroupCycle", err)
	}
	if _, _, err := db.UpdateAccountGroup(ctx, "owner", work.ID, "Work", eng.ID); !errors.Is(err, ErrAccountGroupCycle) {
		t.Fatalf("descendant parent error = %v, want ErrAccountGroupCycle", err)
	}

	// Cross-user isolation.
	if _, err := db.GetAccountGroup(ctx, "other", work.ID); !errors.Is(err, ErrAccountGroupNotFound) {
		t.Fatalf("other user GetAccountGroup = %v, want ErrAccountGroupNotFound", err)
	}
	if err := db.MoveAccountToGroup(ctx, "other", "acc_other", work.ID); !errors.Is(err, ErrAccountGroupNotFound) {
		t.Fatalf("other user MoveAccountToGroup = %v, want ErrAccountGroupNotFound", err)
	}
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_local", work.ID); err == nil {
		t.Fatal("MoveAccountToGroup allowed moving Local folders into a group")
	}

	// Move accounts into groups and verify depth-first ordering + GroupPath.
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_work", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_sub", eng.ID); err != nil {
		t.Fatal(err)
	}

	// Reorder root container so Work group comes before acc_root.
	if err := db.SetAccountContainerOrder(ctx, "owner", "", []string{"group:" + work.ID, "group:" + topDup.ID, "account:acc_root", "account:acc_local"}); err != nil {
		t.Fatalf("SetAccountContainerOrder(root) error = %v", err)
	}

	accounts, err := db.GetAccounts(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	for _, a := range accounts {
		gotIDs = append(gotIDs, a.ID)
	}
	// Inside Work: Engineering (sort 0), child Work (sort 1), acc_work (sort 2).
	if !slices.Equal(gotIDs, []string{"acc_sub", "acc_work", "acc_root", "acc_local"}) {
		t.Fatalf("depth-first account order = %v, want [acc_sub acc_work acc_root acc_local]", gotIDs)
	}
	if len(accounts[0].GroupPath) != 2 || accounts[0].GroupPath[0].Name != "Work" || accounts[0].GroupPath[1].Name != "Engineering" {
		t.Fatalf("acc_sub GroupPath = %+v, want [Work Engineering]", accounts[0].GroupPath)
	}

	owner, chain, err := db.GetAccountProxyChain(ctx, "acc_sub")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "owner" || !slices.Equal(chain, []string{"", "", "proxy_work"}) {
		t.Fatalf("GetAccountProxyChain(acc_sub) = %q, %v, want owner & [\"\" \"\" \"proxy_work\"]", owner, chain)
	}

	// Deleting Work lifts its child groups and direct accounts to top level.
	affected, err := db.DeleteAccountGroup(ctx, "owner", work.ID)
	if err != nil {
		t.Fatalf("DeleteAccountGroup(Work) error = %v", err)
	}
	if !slices.Contains(affected, "acc_work") || !slices.Contains(affected, "acc_sub") {
		t.Fatalf("DeleteAccountGroup affected = %v, want acc_work and acc_sub", affected)
	}
	engAfter, err := db.GetAccountGroup(ctx, "owner", eng.ID)
	if err != nil {
		t.Fatal(err)
	}
	if engAfter.ParentID != "" || engAfter.Name != "Engineering (2)" {
		t.Fatalf("Engineering after deleting Work = %+v, want top-level Name='Engineering (2)'", engAfter)
	}
	childWorkAfter, err := db.GetAccountGroup(ctx, "owner", childWork.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Work itself is being deleted, so child Work keeps the name "Work" at top level.
	if childWorkAfter.ParentID != "" || childWorkAfter.Name != "Work" {
		t.Fatalf("child Work after deleting Work = %+v, want top-level Name='Work'", childWorkAfter)
	}
}

func TestAccountGroupsMigrationPreflightAndCrossOwnerGuard(t *testing.T) {
	ctx := t.Context()

	t.Run("valid-v156-and-v155-historical", func(t *testing.T) {
		db := sharedMigrationFixture(t)
		migrationFixtureSQL(t, db, `
			INSERT INTO account_groups(id, user_id, name, sort_order, proxy_id) VALUES
				('alice-root', 'alice', 'Work', 0, 'direct'),
				('bob-root', 'bob', 'Personal', 0, '');
			INSERT INTO account_groups(id, user_id, parent_id, name, sort_order) VALUES
				('alice-sub', 'alice', 'alice-root', 'Team', 0);
			UPDATE accounts SET group_id = 'alice-sub' WHERE id = 'alice-mail';
		`)
		report, err := inspectUserStorageMigration(ctx, db)
		if err != nil {
			t.Fatalf("inspectUserStorageMigration(v156) error = %v", err)
		}
		if report.SchemaVersion != 156 {
			t.Fatalf("SchemaVersion = %d, want 156", report.SchemaVersion)
		}

		db155 := sharedMigrationFixture(t)
		migrationFixtureSQL(t, db155, `DROP INDEX idx_account_groups_user_parent; DROP TABLE account_groups; DROP INDEX idx_accounts_user_group; ALTER TABLE accounts DROP COLUMN group_id; ALTER TABLE accounts DROP COLUMN background_sync_enabled; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(155)`)
		report155, err := inspectUserStorageMigration(ctx, db155)
		if err != nil {
			t.Fatalf("inspectUserStorageMigration(v155) error = %v", err)
		}
		if report155.SchemaVersion != 155 || report155.TargetSchemaVersion != 156 {
			t.Fatalf("v155 report = (%d -> %d), want (155 -> 156)", report155.SchemaVersion, report155.TargetSchemaVersion)
		}
	})

	t.Run("rejects-cross-owner-account-group-reference", func(t *testing.T) {
		db := sharedMigrationFixture(t)
		migrationFixtureSQL(t, db, `
			INSERT INTO account_groups(id, user_id, name) VALUES ('bob-group', 'bob', 'Bob Group');
			UPDATE accounts SET group_id = 'bob-group' WHERE id = 'alice-mail';
		`)
		if _, err := inspectUserStorageMigration(ctx, db); !errors.Is(err, ErrUserStorageMigrationSource) {
			t.Fatalf("cross-owner accounts.group_id error = %v, want ErrUserStorageMigrationSource", err)
		}
	})

	t.Run("rejects-cross-owner-parent-group-reference", func(t *testing.T) {
		db := sharedMigrationFixture(t)
		migrationFixtureSQL(t, db, `
			INSERT INTO account_groups(id, user_id, name) VALUES ('bob-group', 'bob', 'Bob Group');
			INSERT INTO account_groups(id, user_id, parent_id, name) VALUES ('alice-child', 'alice', 'bob-group', 'Alice Child');
		`)
		if _, err := inspectUserStorageMigration(ctx, db); !errors.Is(err, ErrUserStorageMigrationSource) {
			t.Fatalf("cross-owner account_groups.parent_id error = %v, want ErrUserStorageMigrationSource", err)
		}
	})
}

func TestCreateLocalFoldersAccountOrdersAfterTopLevelGroups(t *testing.T) {
	ctx := t.Context()
	db, err := New(filepath.Join(t.TempDir(), "local_folders_groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users(id, username, username_normalized) VALUES ('owner', 'owner', 'owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAccountGroup(ctx, "owner", "", "Alpha", ""); err != nil {
		t.Fatal(err)
	}
	beta, err := db.CreateAccountGroup(ctx, "owner", "", "Beta", "")
	if err != nil {
		t.Fatal(err)
	}
	localID, err := db.EnsureLocalFoldersAccount(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	var sortOrder int
	if err := db.Read().QueryRowContext(ctx, `SELECT sort_order FROM accounts WHERE id = ?`, localID).Scan(&sortOrder); err != nil {
		t.Fatal(err)
	}
	if sortOrder <= beta.SortOrder {
		t.Fatalf("Local folders SortOrder = %d, want > top-level group SortOrder %d", sortOrder, beta.SortOrder)
	}
}

func TestMoveAccountsToGroupBatchAtomicityAndOrder(t *testing.T) {
	ctx := t.Context()
	db, err := New(filepath.Join(t.TempDir(), "batch_move.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users(id, username, username_normalized) VALUES
			('owner', 'owner', 'owner'),
			('other', 'other', 'other');
		INSERT INTO accounts(id, user_id, provider, email_address, display_name, sort_order) VALUES
			('acc_stay', 'owner', 'imap', 'stay@example.test', 'Stay', 0),
			('acc_one', 'owner', 'imap', 'one@example.test', 'One', 1),
			('acc_two', 'owner', 'imap', 'two@example.test', 'Two', 2),
			('acc_local', 'owner', 'local', 'local@gofer.local', 'Local folders', 3),
			('acc_foreign', 'other', 'imap', 'other@example.test', 'Foreign', 0);
	`); err != nil {
		t.Fatal(err)
	}

	target, err := db.CreateAccountGroup(ctx, "owner", "", "Target", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_stay", target.ID); err != nil {
		t.Fatal(err)
	}

	// Foreign or local account in batch rolls back the whole transaction.
	if _, err := db.MoveAccountsToGroup(ctx, "owner", target.ID, []string{"acc_one", "acc_foreign"}); err == nil {
		t.Fatal("MoveAccountsToGroup with foreign account succeeded, want error")
	}
	if _, err := db.MoveAccountsToGroup(ctx, "owner", target.ID, []string{"acc_one", "acc_local"}); err == nil {
		t.Fatal("MoveAccountsToGroup with local account succeeded, want error")
	}

	// Valid batch moves only accounts not already in target, preserving order.
	moved, err := db.MoveAccountsToGroup(ctx, "owner", target.ID, []string{"acc_stay", "acc_two", "acc_one", "acc_two"})
	if err != nil {
		t.Fatalf("MoveAccountsToGroup error = %v", err)
	}
	if !slices.Equal(moved, []string{"acc_two", "acc_one"}) {
		t.Fatalf("moved = %v, want [acc_two acc_one]", moved)
	}

	accounts, err := db.GetAccounts(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	var inTarget []string
	for _, a := range accounts {
		if a.GroupID == target.ID {
			inTarget = append(inTarget, a.ID)
		}
	}
	if !slices.Equal(inTarget, []string{"acc_stay", "acc_two", "acc_one"}) {
		t.Fatalf("accounts in target order = %v, want [acc_stay acc_two acc_one]", inTarget)
	}
}

func TestMigrateV155ToV156AddsAccountGroups(t *testing.T) {
	ctx := t.Context()
	db, err := New(filepath.Join(t.TempDir(), "migrate_156.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Write().ExecContext(ctx, `
		DROP INDEX idx_account_groups_user_parent;
		DROP TABLE account_groups;
		DROP INDEX idx_accounts_user_group;
		ALTER TABLE accounts DROP COLUMN group_id;
		ALTER TABLE accounts DROP COLUMN background_sync_enabled;
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (155);
		INSERT INTO users (id, username, username_normalized) VALUES ('default', 'default', 'default');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'alex@example.com');`); err != nil {
		t.Fatalf("set up schema 155: %v", err)
	}
	for range 2 {
		if err := migrateV155ToV156(db.Write()); err != nil {
			t.Fatalf("migrateV155ToV156() error = %v", err)
		}
	}
	var groupID string
	var bgSyncEnabled int
	if err := db.Read().QueryRowContext(ctx, `SELECT group_id, background_sync_enabled FROM accounts WHERE id = 'acc'`).Scan(&groupID, &bgSyncEnabled); err != nil {
		t.Fatalf("accounts columns after migration: %v", err)
	}
	if groupID != "" || bgSyncEnabled != 1 {
		t.Fatalf("group_id = %q, background_sync_enabled = %d; want empty, 1", groupID, bgSyncEnabled)
	}
	var version int
	if err := db.Read().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 156 {
		t.Fatalf("schema version = %d, %v; want 156", version, err)
	}

	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP); INSERT INTO schema_version (version) VALUES (155)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV155ToV156(raw); err != nil {
			t.Fatalf("migrateV155ToV156(without accounts) error = %v", err)
		}
	}
}

func TestMigrateV155ToV156WithoutAnAccountOwnerColumn(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY);
		INSERT INTO schema_version VALUES (155);
		CREATE TABLE accounts (id TEXT PRIMARY KEY);
		INSERT INTO accounts VALUES ('acc')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV155ToV156(raw); err != nil {
			t.Fatal(err)
		}
	}
	var groupID string
	var syncEnabled bool
	if err := raw.QueryRow(`SELECT group_id, background_sync_enabled FROM accounts WHERE id='acc'`).Scan(&groupID, &syncEnabled); err != nil || groupID != "" || !syncEnabled {
		t.Fatalf("legacy account defaults = %q, %t, %v; want ungrouped with sync enabled", groupID, syncEnabled, err)
	}
}

func TestBackgroundSyncStorageAndRecursiveGroupBatchToggle(t *testing.T) {
	ctx := t.Context()
	db, err := New(filepath.Join(t.TempDir(), "bg_sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users(id, username, username_normalized, status) VALUES
			('owner', 'owner', 'owner', 'active'),
			('other', 'other', 'other', 'active');
		INSERT INTO accounts(id, user_id, provider, email_address, display_name, sort_order) VALUES
			('acc_top', 'owner', 'imap', 'top@example.test', 'Top', 0),
			('acc_work', 'owner', 'gmail', 'work@gmail.com', 'Work', 1),
			('acc_sub', 'owner', 'imap', 'sub@example.test', 'Sub', 2),
			('acc_local', 'owner', 'local', 'local@gofer.local', 'Local folders', 3),
			('acc_other', 'other', 'imap', 'other@example.test', 'Other', 0);
		INSERT INTO calendar_sources(id, user_id, account_id, provider, remote_id, name, is_selected, is_deleted) VALUES
			('cal_work', 'owner', 'acc_work', 'gmail', 'primary', 'Work Cal', 1, 0),
			('cal_top', 'owner', 'acc_top', 'caldav', 'primary', 'Top Cal', 1, 0);
	`); err != nil {
		t.Fatal(err)
	}

	work, err := db.CreateAccountGroup(ctx, "owner", "", "Work", "")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := db.CreateAccountGroup(ctx, "owner", work.ID, "Sub", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_work", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveAccountToGroup(ctx, "owner", "acc_sub", sub.ID); err != nil {
		t.Fatal(err)
	}

	if !db.IsBackgroundSyncEnabled(ctx, "acc_work") || !db.IsBackgroundSyncEnabled(ctx, "acc_sub") {
		t.Fatal("new accounts should have background sync enabled by default")
	}

	// Cross-owner isolation.
	if err := db.SetAccountBackgroundSyncEnabled(ctx, "other", "acc_work", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign SetAccountBackgroundSyncEnabled = %v, want sql.ErrNoRows", err)
	}
	if _, err := db.SetAccountGroupBackgroundSyncEnabled(ctx, "other", work.ID, false); !errors.Is(err, ErrAccountGroupNotFound) {
		t.Fatalf("foreign SetAccountGroupBackgroundSyncEnabled = %v, want ErrAccountGroupNotFound", err)
	}

	// Recursive batch pause on Work pauses acc_work and acc_sub, leaving acc_top enabled.
	affected, err := db.SetAccountGroupBackgroundSyncEnabled(ctx, "owner", work.ID, false)
	if err != nil {
		t.Fatalf("SetAccountGroupBackgroundSyncEnabled(false) error = %v", err)
	}
	if !slices.Contains(affected, "acc_work") || !slices.Contains(affected, "acc_sub") || len(affected) != 2 {
		t.Fatalf("affected = %v, want [acc_work, acc_sub]", affected)
	}
	if db.IsBackgroundSyncEnabled(ctx, "acc_work") || db.IsBackgroundSyncEnabled(ctx, "acc_sub") {
		t.Fatal("grouped accounts should have background sync paused")
	}
	if !db.IsBackgroundSyncEnabled(ctx, "acc_top") {
		t.Fatal("top-level account should still have background sync enabled")
	}

	// Background sync queries skip paused accounts while regular EmailSyncAccountIDs keeps them for manual sync.
	bgIDs, err := db.GetBackgroundEmailSyncAccountIDs(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bgIDs, []string{"acc_top"}) {
		t.Fatalf("GetBackgroundEmailSyncAccountIDs = %v, want [acc_top]", bgIDs)
	}
	allOwnerIDs, err := db.GetEmailSyncAccountIDs(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(allOwnerIDs) != 3 {
		t.Fatalf("GetEmailSyncAccountIDs = %v, want all 3 connected mail accounts", allOwnerIDs)
	}
	gmailBgIDs, err := db.GetProviderBackgroundEmailSyncAccountIDs(ctx, "owner", "gmail")
	if err != nil || len(gmailBgIDs) != 0 {
		t.Fatalf("GetProviderBackgroundEmailSyncAccountIDs(gmail) = %v, %v; want empty", gmailBgIDs, err)
	}

	// Instance background calendar sources skip paused accounts, while UI selected sources keep all.
	uiCals, err := db.ListSelectedCalendarSources(ctx, "owner")
	if err != nil || len(uiCals) != 2 {
		t.Fatalf("ListSelectedCalendarSources = %d, %v; want 2", len(uiCals), err)
	}
	bgCals, err := db.ListInstanceCalendarSyncSources(ctx)
	if err != nil || len(bgCals) != 1 || bgCals[0].AccountID != "acc_top" {
		t.Fatalf("ListInstanceCalendarSyncSources = %+v, %v; want only acc_top", bgCals, err)
	}

	// Individual account inside group can be toggled independently, then group batch-enables all.
	if err := db.SetAccountBackgroundSyncEnabled(ctx, "owner", "acc_sub", true); err != nil {
		t.Fatal(err)
	}
	if !db.IsBackgroundSyncEnabled(ctx, "acc_sub") || db.IsBackgroundSyncEnabled(ctx, "acc_work") {
		t.Fatal("acc_sub should be enabled while acc_work stays paused")
	}
	if _, err := db.SetAccountGroupBackgroundSyncEnabled(ctx, "owner", work.ID, true); err != nil {
		t.Fatal(err)
	}
	if !db.IsBackgroundSyncEnabled(ctx, "acc_work") || !db.IsBackgroundSyncEnabled(ctx, "acc_sub") {
		t.Fatal("batch enable should re-enable all accounts in Work and Sub")
	}
}
