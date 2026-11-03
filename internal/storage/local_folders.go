package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// LocalFoldersName is the name of the Local folders account, the one account
// each user has whose mail stays on the Gofer server with no mail server
// behind it.
const LocalFoldersName = i18n.LocalFoldersName

// localProvider is providers.ProviderLocal, which storage can't import.
const localProvider = "local"

const localFoldersIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_local_folders ON accounts(COALESCE(user_id, '')) WHERE provider = 'local'`

// migrateV133ToV134 adds accounts.hidden, set when the user hides an account
// from the sidebar and from the places mail can be moved to, and allows one
// Local folders account per user.
func migrateV133ToV134(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 134 {
		return nil
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "accounts", "hidden")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		// Each user has one Local folders account.
		hasUser, err := columnExistsTx(tx, "accounts", "user_id")
		if err != nil {
			return err
		}
		hasProvider, err := columnExistsTx(tx, "accounts", "provider")
		if err != nil {
			return err
		}
		if hasUser && hasProvider {
			if _, err := tx.Exec(localFoldersIndex); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 134); err != nil {
		return err
	}
	return tx.Commit()
}

// LocalFoldersAccountID returns the ID of userID's Local folders account.
// Account IDs are unique across users in a shared database, so it comes from
// the user.
func LocalFoldersAccountID(userID string) string {
	digest := sha256.Sum256([]byte("local\x00" + userID))
	return "acc_local_" + hex.EncodeToString(digest[:10])
}

// LocalFolderID returns the ID of a Local folders folder named by key, which
// stays the same when the folder is renamed.
func LocalFolderID(accountID, key string) string {
	return FolderIDForIdentity(accountID, localProvider, key)
}

// LocalFoldersAccountIDForUser returns the ID of userID's Local folders
// account, or "" if the user has none yet.
func (db *DB) LocalFoldersAccountIDForUser(ctx context.Context, userID string) (string, error) {
	var id string
	err := db.Read().QueryRowContext(ctx, `SELECT id FROM accounts
		WHERE provider = ? AND user_id IS ? AND COALESCE(is_deleting, 0) = 0`, localProvider, nullableUser(userID)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CreateLocalFoldersAccount saves userID's Local folders account under id,
// with its Archive and Trash folders. A user can have only one.
func (db *DB) CreateLocalFoldersAccount(ctx context.Context, userID, id string) error {
	owner := nullableUser(userID)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, display_name, color, initials,
		  imap_host, imap_port, smtp_host, smtp_port, auth_method, email_sync_enabled, provider_push_enabled, sort_order)
		SELECT ?, ?, ?, '', ?, '#64748b', 'LF', '', 0, '', 0, 'none', 1, 0,
		       (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM accounts WHERE user_id IS ?)`,
		id, owner, localProvider, LocalFoldersName, owner); err != nil {
		return fmt.Errorf("create local folders account: %w", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: LocalFolderID(id, "archive"), AccountID: id, RemoteID: "Archive", Name: "Archive", Icon: "archive", Role: "archive", Selectable: true, SortOrder: 0, CountsKnown: true},
		{ID: LocalFolderID(id, "trash"), AccountID: id, RemoteID: "Trash", Name: "Trash", Icon: "trash", Role: "trash", Selectable: true, SortOrder: 1, CountsKnown: true},
	}); err != nil {
		return fmt.Errorf("create local folders: %w", err)
	}
	return nil
}

// EnsureLocalFoldersAccount makes userID's Local folders account in a shared
// database if the user doesn't have it yet, and returns its ID. Managed
// storage makes it through the account directory instead.
func (db *DB) EnsureLocalFoldersAccount(ctx context.Context, userID string) (string, error) {
	if id, err := db.LocalFoldersAccountIDForUser(ctx, userID); err != nil || id != "" {
		return id, err
	}
	id := LocalFoldersAccountID(userID)
	if err := db.CreateLocalFoldersAccount(ctx, userID, id); err != nil {
		// Another request may have made it first.
		if existing, findErr := db.LocalFoldersAccountIDForUser(ctx, userID); findErr == nil && existing != "" {
			return existing, nil
		}
		return "", err
	}
	return id, nil
}

func nullableUser(userID string) any {
	if userID == "" {
		return nil
	}
	return userID
}

// localFoldersCreation keeps two requests from reserving two Local folders
// accounts for the same user at once.
var localFoldersCreation sync.Mutex

// EnsureLocalFolders makes userID's Local folders account in managed storage
// if the user doesn't have it yet, reserving its ID in the account directory
// like any other account. Users without a mailbox, such as admins, get none.
func (r *AccountRouting) EnsureLocalFolders(ctx context.Context, userID string) error {
	find := func() (string, error) {
		var id string
		err := r.WithUser(ctx, userID, func(db *DB) error {
			var err error
			id, err = db.LocalFoldersAccountIDForUser(ctx, userID)
			return err
		})
		return id, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		id, err := find()
		if errors.Is(err, ErrUserStoreOwner) {
			return nil
		}
		if err != nil || id != "" {
			return err
		}
		if attempt == 0 {
			localFoldersCreation.Lock()
			defer localFoldersCreation.Unlock()
		}
	}
	entry, err := r.ReserveAccount(ctx, userID)
	if errors.Is(err, ErrUserStoreOwner) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.CompleteAccountCreation(ctx, userID, entry.AccountID, func(db *DB) error {
		return db.CreateLocalFoldersAccount(ctx, userID, entry.AccountID)
	}); err != nil {
		return fmt.Errorf("create local folders account %s: %w", entry.AccountID, err)
	}
	return nil
}

// moveLocalMessageTx moves a message between two Local folders: its copy in
// the source goes and one in the destination keeps its read and starred
// state. Nothing is queued, since no server holds it.
func moveLocalMessageTx(ctx context.Context, tx *sql.Tx, messageID int64, sourceFolderID, destinationFolderID string, isRead, isStarred int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, messageID, sourceFolderID); err != nil {
		return fmt.Errorf("leave source folder for message %d: %w", messageID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_starred, is_flagged, is_draft, is_deleted, synced_at)
		VALUES (?, ?, NULL, ?, ?, 0, 0, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(message_id, folder_id) DO UPDATE SET
			is_read = excluded.is_read, is_starred = excluded.is_starred, is_deleted = 0, synced_at = CURRENT_TIMESTAMP`,
		messageID, destinationFolderID, isRead, isStarred); err != nil {
		return fmt.Errorf("show destination folder for message %d: %w", messageID, err)
	}
	return nil
}

// deleteFolderlessMessagesTx removes those of ids that are in no folder any
// more, with their search entries and thread totals. Their files go with the
// next file cleanup, which removes files no message refers to.
func (db *DB) deleteFolderlessMessagesTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.account_id, COALESCE(m.thread_id, '') FROM messages m
		WHERE m.id IN (`+sqlPlaceholders(len(ids))+`)
		  AND NOT EXISTS (SELECT 1 FROM message_folder_state mfs WHERE mfs.message_id = m.id)`, args...)
	if err != nil {
		return err
	}
	var gone []int64
	threads := make(pendingThreadTotals)
	for rows.Next() {
		var id int64
		var thread threadRef
		if err := rows.Scan(&id, &thread.accountID, &thread.threadID); err != nil {
			rows.Close()
			return err
		}
		gone = append(gone, id)
		if thread.threadID != "" {
			threads[thread] = struct{}{}
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(gone) == 0 {
		return nil
	}
	goneArgs := make([]any, len(gone))
	for i, id := range gone {
		goneArgs[i] = id
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id IN (`+sqlPlaceholders(len(gone))+`)`, goneArgs...); err != nil {
		return fmt.Errorf("delete local messages: %w", err)
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, gone); err != nil {
		return err
	}
	return db.flushThreadTotalsTx(ctx, tx, threads)
}

// FolderIDByName returns the ID of the folder named name, in any case, under
// parentID ("" for the top) of an account, or "" if there is none.
func (db *DB) FolderIDByName(ctx context.Context, accountID, parentID, name string) (string, error) {
	var id string
	err := db.Read().QueryRowContext(ctx, `SELECT id FROM folders
		WHERE account_id = ? AND COALESCE(parent_id, '') = ? AND lower(name) = lower(?)
		  AND COALESCE(discovery_state, 'active') = 'active'
		ORDER BY id LIMIT 1`, accountID, parentID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
