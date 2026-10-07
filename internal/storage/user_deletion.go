package storage

import (
	"context"
	"database/sql"
	"errors"
)

var ErrUserDeletionIncomplete = errors.New("user deletion cleanup is incomplete")

func pendingUserDeletion(ctx context.Context, tx *sql.Tx, owner string, removed bool) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN gofer_user_store_directory s ON s.user_id=u.id
 WHERE u.id=? AND u.status='disabled' AND u.deletion_pending=1 AND u.user_type='webmail' AND u.is_admin=0
 AND s.state IN ('removing','removed') AND (?=0 OR s.state='removed'))`, owner, removed).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrUserDeletionIncomplete
	}
	return nil
}

// PreparePendingUserDeletionTx participates in authentication's central
// transaction after it establishes confirmed pending intent. It never opens
// local stores; failed marking also rolls back the identity and audit changes.
func (r *AccountRouting) PreparePendingUserDeletionTx(ctx context.Context, tx *sql.Tx, owner string) ([]string, error) {
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND status='disabled' AND deletion_pending=1 AND user_type='webmail' AND is_admin=0)`, owner).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrUserStoreOwner
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_user_store_directory(user_id,state) VALUES(?,'removing')
 ON CONFLICT(user_id) DO UPDATE SET state='removing',updated_at=CURRENT_TIMESTAMP WHERE gofer_user_store_directory.state='present'`, owner); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gofer_account_directory SET state='deleting',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND state!='deleted'`, owner); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM gofer_account_directory WHERE user_id=? AND state!='deleted' ORDER BY account_id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *AccountRouting) pendingAccountDeletion(ctx context.Context, owner, id string, removed bool) error {
	tx, err := r.System().Read().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pendingUserDeletion(ctx, tx, owner, removed); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE account_id=? AND user_id=? AND state IN ('deleting','deleted'))`, id, owner).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrAccountRoute
	}
	return tx.Commit()
}

// DrainPendingUserAccount waits for account callbacks and external activities
// under the same coordinator used for routing. Unlike individual mailbox
// deletion it does not open a database whose entire owner is being removed.
// Drain all accounts before acquiring exclusive owner-file access.
func (r *AccountRouting) DrainPendingUserAccount(ctx context.Context, owner, id string) error {
	return r.transition(ctx, id, func(scope *accountRouteScope) error {
		if err := r.pendingAccountDeletion(ctx, owner, id, false); err != nil {
			return err
		}
		r.mu.Lock()
		for scope.running != 0 {
			changed := scope.changed
			r.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
			r.mu.Lock()
		}
		r.mu.Unlock()
		return r.pendingAccountDeletion(ctx, owner, id, false)
	})
}

// RemovePendingUserAccount serializes trusted, idempotent external cleanup
// after the whole user's database is removed and account activities drained.
// Its callback must remove local blobs and central mailbox credentials only,
// with no provider deletion and no local database lease. Failures keep deleting
// durable. The directory tombstone follows successful cleanup and credential
// absence; schedule hints are removed in that same central transaction.
func (r *AccountRouting) RemovePendingUserAccount(ctx context.Context, owner, id string, cleanup func(context.Context) error) error {
	if cleanup == nil {
		return ErrUserDeletionIncomplete
	}
	return r.transition(ctx, id, func(scope *accountRouteScope) error {
		if err := r.pendingAccountDeletion(ctx, owner, id, true); err != nil {
			return err
		}
		r.mu.Lock()
		busy := scope.running != 0
		r.mu.Unlock()
		if busy {
			return ErrUserDeletionIncomplete
		}
		if err := cleanup(ctx); err != nil {
			return err
		}
		tx, err := r.System().Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := pendingUserDeletion(ctx, tx, owner, true); err != nil {
			return err
		}
		if err := noPendingUserCredentials(ctx, tx, owner, id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE gofer_account_directory SET state='deleted',updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND user_id=? AND state IN ('deleting','deleted')`, id, owner)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrAccountRoute
		}
		for _, query := range []string{
			`DELETE FROM gofer_account_poll_schedule WHERE account_id=?`,
			`DELETE FROM gofer_account_active_poll WHERE account_id=?`,
			`DELETE FROM gofer_account_provider_retry WHERE account_id=?`,
			`DELETE FROM gofer_account_service_schedule WHERE account_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, query, id); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func noPendingUserCredentials(ctx context.Context, tx *sql.Tx, owner, account string) error {
	// The optional provider credential repository creates its own extension.
	// IMAP-only installations have no such table, but any existing credentials
	// must be explicitly cleaned before account or identity completion.
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='gofer_mailbox_credentials' AND type='table')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var remaining bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_mailbox_credentials WHERE (?='' AND user_id=?) OR (?<>'' AND account_id=?))`, account, owner, account, account).Scan(&remaining); err != nil {
		return err
	}
	if remaining {
		return ErrUserDeletionIncomplete
	}
	return nil
}

func pendingUserAccountsRemoved(ctx context.Context, tx *sql.Tx, owner string) error {
	if err := pendingUserDeletion(ctx, tx, owner, true); err != nil {
		return err
	}
	var remaining bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id=? AND state!='deleted')`, owner).Scan(&remaining); err != nil {
		return err
	}
	if remaining {
		return ErrUserDeletionIncomplete
	}
	return noPendingUserCredentials(ctx, tx, owner, "")
}

// CompletePendingUserCleanup records the final external-cleanup receipt after
// every account tombstone and the database removal receipt. Its trusted callback
// must drain/remove the owner's compose files under exclusive file access and
// sync the file removals before returning. No SQLite lease/transaction is held
// during that callback. Authentication completion requires this durable receipt.
func (r *AccountRouting) CompletePendingUserCleanup(ctx context.Context, owner string, cleanup func(context.Context) error) error {
	if cleanup == nil {
		return ErrUserDeletionIncomplete
	}
	tx, err := r.System().Read().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	err = pendingUserAccountsRemoved(ctx, tx, owner)
	err = errors.Join(err, tx.Rollback())
	if err != nil {
		return err
	}
	if err := cleanup(ctx); err != nil {
		return err
	}
	tx, err = r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pendingUserAccountsRemoved(ctx, tx, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_user_cleanup_receipts(user_id) VALUES(?) ON CONFLICT(user_id) DO NOTHING`, owner); err != nil {
		return err
	}
	return tx.Commit()
}

// ValidatePendingUserCleanupTx runs inside authentication's completion
// transaction. Empty shared mailbox tables alone are never completion proof.
func (r *AccountRouting) ValidatePendingUserCleanupTx(ctx context.Context, tx *sql.Tx, owner string) error {
	if err := pendingUserAccountsRemoved(ctx, tx, owner); err != nil {
		return err
	}
	var complete bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_user_cleanup_receipts WHERE user_id=?)`, owner).Scan(&complete); err != nil {
		return err
	}
	if !complete {
		return ErrUserDeletionIncomplete
	}
	return nil
}
