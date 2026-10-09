package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
)

// This directory is durable owner/file history, not a second account repository.
// Removed IDs remain reserved. It also protects contacts/preferences-only owners
// from silently replacing a missing file when they have no mailbox history.
func ensureUserStoreDirectory(system *DB) error {
	var local bool
	if err := system.Read().QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='gofer_user_store')`).Scan(&local); err != nil {
		return err
	}
	if local {
		return ErrUserStoreIdentity
	}
	return ensureLayoutSchema(context.Background(), system, centralLayoutSchema)
}

const userStoreDirectorySchema = `CREATE TABLE IF NOT EXISTS gofer_user_store_directory (
 user_id TEXT PRIMARY KEY CHECK(user_id<>''),
 state TEXT NOT NULL CHECK(state IN ('present','removing','removed')),
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
 )`

func (m *UserStores) recordOpenedStore(ctx context.Context, owner string) error {
	var state string
	err := m.system.Read().QueryRowContext(ctx, `SELECT state FROM gofer_user_store_directory WHERE user_id=?`, owner).Scan(&state)
	if err == nil {
		if state != "present" {
			return ErrUserStoreOwner
		}
		return ctx.Err()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	result, err := m.system.Write().ExecContext(ctx, `INSERT INTO gofer_user_store_directory(user_id,state)
 SELECT id,'present' FROM users WHERE id=? AND user_type='webmail' AND is_admin=0
 ON CONFLICT(user_id) DO UPDATE SET updated_at=CURRENT_TIMESTAMP WHERE gofer_user_store_directory.state='present'`, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrUserStoreOwner
	}
	return nil
}

// RemovePendingUserStore is trusted deletion maintenance. A disabled ordinary
// webmail identity must already have durable deletion intent. The removing
// directory state rejects new leases across retries/restarts. Opening handles,
// active leases and physical SQLite close drain before any unlink; no cache
// mutex is held during filesystem work. A deadline/failure never records removed.
// Callers must separately drain external work and blob-file pins before invoking
// this and must finish blob/credential cleanup before deleting the central user.
func (m *UserStores) RemovePendingUserStore(ctx context.Context, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := m.system.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND status='disabled' AND deletion_pending=1 AND user_type='webmail' AND is_admin=0)`, owner).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrUserStoreOwner
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_user_store_directory(user_id,state) VALUES(?,'removing')
 ON CONFLICT(user_id) DO UPDATE SET state='removing',updated_at=CURRENT_TIMESTAMP WHERE gofer_user_store_directory.state='present'`, owner); err != nil {
		return err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM gofer_user_store_directory WHERE user_id=?`, owner).Scan(&state); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := m.drainUserStore(ctx, owner); err != nil {
		return err
	}
	path := m.userPath(owner)
	storeExists := false
	// Validate every path before removing any. Symlinks and directories cannot
	// stand in for an owned SQLite file or sidecar. Missing files are retryable
	// only because the central deletion intent and removing receipt persist.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrUserStoreIdentity
		}
		if state == "removed" {
			return ErrUserStoreIdentity
		}
		if suffix == "" {
			storeExists = true
		}
	}
	if storeExists {
		// Removal only needs the owner; a store left at an older schema by an
		// interrupted deletion is removed, not upgraded first.
		if err := checkUserStoreFile(path, owner, false); err != nil {
			return err
		}
	}
	if state == "removed" {
		return ctx.Err()
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		remove := m.removeStoreFile
		if remove == nil {
			remove = os.Remove
		}
		if err := remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned database: %w", err)
		}
	}
	// Directory sync makes the unlinks durable before publishing the SQL receipt
	// on Unix. Windows does not expose portable directory fsync through os.File.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(m.directory)
		if err != nil {
			return err
		}
		err = errors.Join(dir.Sync(), dir.Close())
		if err != nil {
			return err
		}
	}
	result, err := m.system.Write().ExecContext(ctx, `UPDATE gofer_user_store_directory SET state='removed',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND state IN ('removing','removed')
 AND EXISTS(SELECT 1 FROM users WHERE id=? AND status='disabled' AND deletion_pending=1 AND user_type='webmail' AND is_admin=0)`, owner, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrUserStoreOwner
	}
	return nil
}

func (m *UserStores) drainUserStore(ctx context.Context, owner string) error {
	var observed *userStoreEntry
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return ErrUserStoresClosed
		}
		entry := m.entries[owner]
		if entry == nil {
			var err error
			if observed != nil {
				err = observed.closeErr
			}
			m.mu.Unlock()
			return err
		}
		observed = entry
		if !entry.opening && !entry.closing && entry.refs == 0 {
			m.closeEntryLocked(owner, entry)
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// PendingUserStoreRemoved is a copied central receipt, not evidence about blobs
// or provider/account activity. Completion must check those separately.
func (r *AccountRouting) PendingUserStoreRemoved(ctx context.Context, owner string) (bool, error) {
	var state string
	err := r.System().Read().QueryRowContext(ctx, `SELECT state FROM gofer_user_store_directory WHERE user_id=?`, owner).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return state == "removed", err
}

func (r *AccountRouting) RemovePendingUserStore(ctx context.Context, owner string) error {
	return r.stores.RemovePendingUserStore(ctx, owner)
}
