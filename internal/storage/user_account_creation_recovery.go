package storage

import (
	"context"
	"database/sql"
	"errors"
)

type AccountCreationRecovery struct {
	State   AccountRouteState
	Changed bool
}

// PendingAccountCreationPage is trusted startup discovery. Disabled owners are
// retained, but management identities and accepted user deletions are excluded.
// Pages contain directory metadata only; discovery never opens a user file.
func (r *AccountRouting) PendingAccountCreationPage(ctx context.Context, after string, limit int) ([]AccountRoute, error) {
	if limit < 1 || limit > 64 {
		return nil, ErrAccountRoute
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT d.account_id,d.user_id,d.state
 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 WHERE d.state='creating' AND d.account_id>?
 AND u.user_type='webmail' AND u.is_admin=0 AND u.deletion_pending=0
 ORDER BY d.account_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []AccountRoute
	for rows.Next() {
		var entry AccountRoute
		if err := rows.Scan(&entry.AccountID, &entry.UserID, &entry.State); err != nil {
			return nil, err
		}
		page = append(page, entry)
	}
	return page, rows.Err()
}

// RecoverPendingAccountCreation reconciles an existing local commit with its
// central reservation, without replaying a request or contacting a provider.
// CreateAccount opens the owner's store before reserving an ID. A missing file
// is therefore data loss, never evidence of an empty reservation. Only a verified
// absent local row may become a permanent canceled tombstone.
//
// This shares creation/deletion serialization and holds the local writer
// connection while publishing central metadata. No local rows are changed and
// no cross-file transaction is implied. A failed publication remains retryable.
func (r *AccountRouting) RecoverPendingAccountCreation(ctx context.Context, owner, id string) (result AccountCreationRecovery, err error) {
	return r.reconcileAccountCreation(ctx, owner, id, false)
}

func (r *AccountRouting) reconcileAccountCreation(ctx context.Context, owner, id string, cancelOnly bool) (result AccountCreationRecovery, err error) {
	if owner == "" || id == "" {
		return result, ErrAccountRoute
	}
	err = r.transition(ctx, id, func(*accountRouteScope) error {
		state, err := r.AccountStateForUser(ctx, owner, id)
		if err != nil {
			return err
		}
		if state != AccountCreating && state != AccountActive && state != AccountDeleted {
			return ErrAccountRoute
		}
		if cancelOnly && state != AccountCreating {
			return ErrAccountRoute
		}
		return r.withStartupStore(ctx, owner, func(db *DB) error {
			local, err := db.Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer local.Rollback()
			var actualOwner string
			if err := local.QueryRowContext(ctx, `SELECT user_id FROM gofer_user_store WHERE singleton=1 AND layout_version=1`).Scan(&actualOwner); err != nil {
				return err
			}
			if actualOwner != owner {
				return ErrUserStoreIdentity
			}
			var deleting int
			err = local.QueryRowContext(ctx, `SELECT user_id,COALESCE(is_deleting,0) FROM accounts WHERE id=?`, id).Scan(&actualOwner, &deleting)
			next := AccountActive
			if errors.Is(err, sql.ErrNoRows) {
				next = AccountDeleted
			} else if err != nil {
				return err
			} else if actualOwner != owner || deleting != 0 {
				return ErrAccountRoute
			}
			if cancelOnly && next != AccountDeleted {
				return ErrAccountRoute
			}
			if state != AccountCreating && state != next {
				return ErrAccountRoute
			}
			tx, err := r.System().Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := retainedCreationRoute(ctx, tx, owner, id, state); err != nil {
				return err
			}
			if next == AccountDeleted {
				if err := noPendingUserCredentials(ctx, tx, owner, id); err != nil {
					return err
				}
			} else if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision) VALUES(?,0,0) ON CONFLICT(user_id) DO NOTHING`, owner); err != nil {
				return err
			}
			if state == AccountCreating {
				changed, err := tx.ExecContext(ctx, `UPDATE gofer_account_directory SET state=?,updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND user_id=? AND state='creating'`, next, id, owner)
				if err != nil {
					return err
				}
				if n, err := changed.RowsAffected(); err != nil {
					return err
				} else if n != 1 {
					return ErrAccountRoute
				}
			}
			// Recheck final state and discovery guarantees before publication,
			// including any effects of database triggers inside this transaction.
			if err := retainedCreationRoute(ctx, tx, owner, id, next); err != nil {
				return err
			}
			if next == AccountActive {
				var hint bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_contact_queue_schedule WHERE user_id=?)`, owner).Scan(&hint); err != nil {
					return err
				}
				if !hint {
					return ErrAccountRoute
				}
			} else if err := noPendingUserCredentials(ctx, tx, owner, id); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			result = AccountCreationRecovery{State: next, Changed: state == AccountCreating}
			return nil
		})
	})
	return result, err
}

func retainedCreationRoute(ctx context.Context, tx *sql.Tx, owner, id string, state AccountRouteState) error {
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND user_type='webmail' AND is_admin=0 AND deletion_pending=0)`, owner).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrUserStoreOwner
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE account_id=? AND user_id=? AND state=?) AND EXISTS(SELECT 1 FROM gofer_user_store_directory WHERE user_id=? AND state='present')`, id, owner, state, owner).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrAccountRoute
	}
	return nil
}
