package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Retained store repair is trusted runtime work, not an owner authorization
// path. It allows disabled webmail owners and excludes pending deletion.
func (r *AccountRouting) validateStartupStoreOwner(ctx context.Context, owner string) error {
	var allowed bool
	if err := r.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND user_type='webmail' AND is_admin=0 AND deletion_pending=0)`, owner).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrUserStoreOwner
	}
	return ctx.Err()
}

// ListStartupStoreOwners discovers central identities without opening files.
// Unused owners are harmless; a missing known store must remain an error.
func (r *AccountRouting) ListStartupStoreOwners(ctx context.Context, after string, limit int) ([]string, error) {
	if limit <= 0 || limit > 64 {
		limit = 64
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT id FROM users WHERE user_type='webmail' AND is_admin=0 AND deletion_pending=0 AND id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

func (r *AccountRouting) withStartupStore(ctx context.Context, owner string, fn func(*DB) error) error {
	return r.withRetainedExistingStore(ctx, owner, func() error { return r.validateStartupStoreOwner(ctx, owner) }, fn)
}

// EnsureUserThreadingForStartup preserves the existing ordered-ID snapshot and
// rereads current message data before each repair. It retains only int64 IDs for
// one owner, rather than full message records; snapshot memory is proportional
// to that owner's message count. Every 500-message transaction releases its
// store before progress callbacks and the next admission. No database handle,
// rows, transaction or DB-local progress state escapes its lease.
func (r *AccountRouting) EnsureUserThreadingForStartup(ctx context.Context, owner string, progress func(ThreadingState)) error {
	var ids []int64
	if err := r.withStartupStore(ctx, owner, func(db *DB) error {
		var needs bool
		if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND (COALESCE(m.thread_id,'')='' OR COALESCE(m.message_id_normalized,'')=''))
 OR (EXISTS(SELECT 1 FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0)
 AND NOT EXISTS(SELECT 1 FROM threads t JOIN accounts a ON a.id=t.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0))`, owner, owner, owner).Scan(&needs); err != nil {
			return err
		}
		if !needs {
			return nil
		}
		rows, err := db.Read().QueryContext(ctx, `SELECT m.id FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0 ORDER BY m.date_received,m.id`, owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	state := ThreadingState{InProgress: len(ids) > 0, Total: len(ids)}
	if progress != nil {
		progress(state)
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		if err := r.withStartupStore(ctx, owner, func(db *DB) error {
			tx, err := db.Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := r.validateStartupStoreOwner(ctx, owner); err != nil {
				return err
			}
			accounts := map[string]AccountRouteState{}
			for _, id := range ids[start:end] {
				var m threadingRepairMessage
				err := tx.QueryRowContext(ctx, `SELECT m.id,m.account_id,COALESCE(m.internet_message_id,''),m.in_reply_to,m."references",m.subject,m.date_received FROM messages m JOIN accounts a ON a.id=m.account_id WHERE m.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0`, id, owner).Scan(&m.id, &m.accountID, &m.msgID, &m.inReplyTo, &m.refs, &m.subject, &m.sentAt)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				status, known := accounts[m.accountID]
				if !known {
					status, err = r.AccountStateForUser(ctx, owner, m.accountID)
					if err != nil {
						return err
					}
					accounts[m.accountID] = status
				}
				if status != AccountActive {
					continue
				}
				if err := db.reconcileThreadingRepairMessageTx(ctx, tx, m); err != nil {
					return err
				}
			}
			// Account deletion or creation can race the local writer wait. A missing
			// directory entry is never permission to repair an untracked mailbox.
			for account, state := range accounts {
				current, err := r.AccountStateForUser(ctx, owner, account)
				if err != nil {
					return err
				}
				if current != state {
					return ErrAccountRoute
				}
			}
			if err := r.validateStartupStoreOwner(ctx, owner); err != nil {
				return err
			}
			return tx.Commit()
		}); err != nil {
			return err
		}
		state.Processed = end
		state.InProgress = end < len(ids)
		if progress != nil {
			progress(state)
		}
	}
	return nil
}
