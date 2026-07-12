package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestReconcileDiscoveredFoldersMarksMissingAndRecovers(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_projects", AccountID: "acc", RemoteID: "Projects", Name: "Projects", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	if err := db.SetIdleFoldersAll(ctx, "default", map[string][]string{"acc": {"acc_inbox", "acc_projects"}}); err != nil {
		t.Fatalf("seed idle settings: %v", err)
	}

	first := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)
	if result, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX", "Projects"}, first); err != nil || result.Changed() {
		t.Fatalf("initial reconciliation = %#v, %v; want no lifecycle changes", result, err)
	}
	missingAt := first.Add(time.Hour)
	result, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX"}, missingAt)
	if err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	if len(result.MissingIDs) != 1 || result.MissingIDs[0] != "acc_projects" || len(result.RemovedIDs) != 0 {
		t.Fatalf("missing result = %#v, want only acc_projects missing", result)
	}
	var state string
	var selectable int
	var missingSince sql.NullTime
	if err := db.Read().QueryRowContext(ctx, `SELECT discovery_state, selectable, missing_since FROM folders WHERE id = 'acc_projects'`).Scan(&state, &selectable, &missingSince); err != nil {
		t.Fatalf("query missing folder: %v", err)
	}
	if state != "missing" || selectable != 0 || !missingSince.Valid {
		t.Fatalf("missing folder state=%q selectable=%d missing_since=%v", state, selectable, missingSince)
	}
	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() while missing: %v", err)
	}
	if len(folders) != 1 || folders[0].ID != "acc_inbox" {
		t.Fatalf("visible folders while missing = %#v, want only inbox", folders)
	}
	if ids := db.GetIdleFolderIDsForAccount(ctx, "default", "acc"); ids["acc_projects"] {
		t.Fatalf("missing folder remained in effective IDLE IDs: %#v", ids)
	}
	setting, err := db.GetSetting(ctx, "default", "idle_folders")
	if err != nil || !strings.Contains(setting, "acc_projects") {
		t.Fatalf("idle setting = %q, %v; want preserved missing folder selection", setting, err)
	}

	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_projects", AccountID: "acc", RemoteID: "Projects", Name: "Projects", Role: "custom", Selectable: true}}); err != nil {
		t.Fatalf("restore discovered folder metadata: %v", err)
	}
	result, err = db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX", "Projects"}, missingAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("recover folder: %v", err)
	}
	if len(result.RecoveredIDs) != 1 || result.RecoveredIDs[0] != "acc_projects" {
		t.Fatalf("recovery result = %#v, want acc_projects", result)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT discovery_state, selectable, missing_since FROM folders WHERE id = 'acc_projects'`).Scan(&state, &selectable, &missingSince); err != nil {
		t.Fatalf("query recovered folder: %v", err)
	}
	if state != "active" || selectable != 1 || missingSince.Valid {
		t.Fatalf("recovered folder state=%q selectable=%d missing_since=%v", state, selectable, missingSince)
	}
	if ids := db.GetIdleFolderIDsForAccount(ctx, "default", "acc"); !ids["acc_projects"] {
		t.Fatalf("recovered folder did not return to effective IDLE IDs: %#v", ids)
	}
}

func TestReconcileDiscoveredFoldersConfirmsRemovalAndFailsPendingWork(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('other', 'default', 'imap', 'other@example.com')`); err != nil {
		t.Fatalf("insert other account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_old", AccountID: "acc", RemoteID: "Old", Name: "Old", Role: "custom", Selectable: true},
		{ID: "acc_child", AccountID: "acc", ParentID: "acc_old", RemoteID: "Old/Child", Name: "Child", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'acc', '<orphan@example.com>', 'Orphan', 'sender@example.com'),
		       (2, 'acc', '<shared@example.com>', 'Shared', 'sender@example.com'),
		       (3, 'other', '<other-orphan@example.com>', 'Other orphan', 'sender@example.com');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid) VALUES (1, 'acc_old', 1), (2, 'acc_old', 2), (2, 'acc_inbox', 3);
		INSERT INTO folder_thread_state (folder_id, thread_key, head_message_id, account_id) VALUES ('acc_old', 'thread:1', 1, 'acc');
		INSERT INTO message_mutations (id, account_id, message_id, folder_id, provider_type, kind, target_value, destination_folder_id)
		VALUES ('move-source', 'acc', 1, 'acc_old', 'imap', 'move', 1, 'acc_inbox'),
		       ('move-dest', 'acc', 2, 'acc_inbox', 'imap', 'move', 1, 'acc_old');
		INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, operation, label_name)
		VALUES ('acc', 2, 'acc_old', 'imap', 'add', 'Old');
		INSERT INTO imap_draft_states (account_id, draft_key, local_message_id, folder_id, folder_remote_name)
		VALUES ('acc', 'draft-1', 1, 'acc_old', 'Old');
		INSERT INTO imap_draft_operations (id, account_id, draft_key, kind, mime_data, message_date)
		VALUES ('draft-op', 'acc', 'draft-1', 'upsert', X'01', CURRENT_TIMESTAMP);
	`); err != nil {
		t.Fatalf("seed pending work: %v", err)
	}

	first := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)
	if _, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX", "Old", "Old/Child"}, first); err != nil {
		t.Fatalf("initial reconciliation: %v", err)
	}
	if _, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX", "Old/Child"}, first.Add(time.Hour)); err != nil {
		t.Fatalf("first omission: %v", err)
	}
	result, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"INBOX", "Old/Child"}, first.Add(25*time.Hour))
	if err != nil {
		t.Fatalf("confirmed omission: %v", err)
	}
	if len(result.RemovedIDs) != 1 || result.RemovedIDs[0] != "acc_old" {
		t.Fatalf("removal result = %#v, want acc_old removed", result)
	}

	var state string
	if err := db.Read().QueryRowContext(ctx, `SELECT discovery_state FROM folders WHERE id = 'acc_old'`).Scan(&state); err != nil {
		t.Fatalf("query removed folder state: %v", err)
	}
	if state != "removed" {
		t.Fatalf("removed folder state = %q, want removed", state)
	}
	var childState, childParent string
	if err := db.Read().QueryRowContext(ctx, `SELECT discovery_state, COALESCE(parent_id, '') FROM folders WHERE id = 'acc_child'`).Scan(&childState, &childParent); err != nil {
		t.Fatalf("query child lifecycle: %v", err)
	}
	if childState != "active" || childParent != "" {
		t.Fatalf("active child lifecycle = state:%q parent:%q, want active/root after parent removal", childState, childParent)
	}
	var mutationStatus, mutationError string
	if err := db.Read().QueryRowContext(ctx, `SELECT status, last_error FROM message_mutations WHERE id = 'move-source'`).Scan(&mutationStatus, &mutationError); err != nil {
		t.Fatalf("query source mutation: %v", err)
	}
	if mutationStatus != MessageMutationFailed || !strings.Contains(mutationError, "folder removed remotely") {
		t.Fatalf("source mutation status=%q error=%q", mutationStatus, mutationError)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT status, last_error FROM message_mutations WHERE id = 'move-dest'`).Scan(&mutationStatus, &mutationError); err != nil {
		t.Fatalf("query destination mutation: %v", err)
	}
	if mutationStatus != MessageMutationFailed || !strings.Contains(mutationError, "folder removed remotely") {
		t.Fatalf("destination mutation status=%q error=%q", mutationStatus, mutationError)
	}
	var labelError string
	if err := db.Read().QueryRowContext(ctx, `SELECT last_error FROM label_mutation_queue WHERE message_id = 2`).Scan(&labelError); err != nil {
		t.Fatalf("query label mutation: %v", err)
	}
	if !strings.Contains(labelError, "folder removed remotely") {
		t.Fatalf("label mutation error = %q", labelError)
	}
	var draftStatus, draftError string
	if err := db.Read().QueryRowContext(ctx, `SELECT status, last_error FROM imap_draft_operations WHERE id = 'draft-op'`).Scan(&draftStatus, &draftError); err != nil {
		t.Fatalf("query draft operation: %v", err)
	}
	if draftStatus != "failed" || !strings.Contains(draftError, "folder removed remotely") {
		t.Fatalf("draft operation status=%q error=%q", draftStatus, draftError)
	}
	var removedMemberships, removedThreads, orphanMessage, sharedMembership, otherOrphan int
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE folder_id = 'acc_old'`).Scan(&removedMemberships)
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = 'acc_old'`).Scan(&removedThreads)
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id = 1`).Scan(&orphanMessage)
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = 2 AND folder_id = 'acc_inbox'`).Scan(&sharedMembership)
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id = 3`).Scan(&otherOrphan)
	if removedMemberships != 0 || removedThreads != 0 || orphanMessage != 1 || sharedMembership != 1 || otherOrphan != 1 {
		t.Fatalf("removed state memberships=%d threads=%d orphanMessage=%d sharedMembership=%d otherOrphan=%d", removedMemberships, removedThreads, orphanMessage, sharedMembership, otherOrphan)
	}
	visible, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount after removal: %v", err)
	}
	visibleIDs := make(map[string]bool, len(visible))
	for _, folder := range visible {
		visibleIDs[folder.ID] = true
	}
	if len(visible) != 2 || !visibleIDs["acc_inbox"] || !visibleIDs["acc_child"] {
		t.Fatalf("visible folders after removal = %#v, want inbox and surviving child", visible)
	}
}

func TestReconcileDiscoveredFoldersDoesNotGuessAnIMAPRename(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_old", AccountID: "acc", RemoteID: "Old", Name: "Old", Role: "custom", Selectable: true}}); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	first := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)
	if _, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"Old"}, first); err != nil {
		t.Fatalf("initial reconciliation: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_new", AccountID: "acc", RemoteID: "New", Name: "New", Role: "custom", Selectable: true}}); err != nil {
		t.Fatalf("seed renamed folder: %v", err)
	}
	result, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"New"}, first.Add(time.Hour))
	if err != nil {
		t.Fatalf("rename reconciliation: %v", err)
	}
	if len(result.MissingIDs) != 1 || result.MissingIDs[0] != "acc_old" || len(result.RecoveredIDs) != 0 {
		t.Fatalf("rename result = %#v, want old missing and no guessed recovery", result)
	}
	var oldState, newState string
	_ = db.Read().QueryRowContext(ctx, `SELECT discovery_state FROM folders WHERE id = 'acc_old'`).Scan(&oldState)
	_ = db.Read().QueryRowContext(ctx, `SELECT discovery_state FROM folders WHERE id = 'acc_new'`).Scan(&newState)
	if oldState != "missing" || newState != "active" {
		t.Fatalf("rename states old=%q new=%q", oldState, newState)
	}
}
