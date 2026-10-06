package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Polling deadlines are worker metadata. Preferences remain authoritative in
// the user store; a revision prevents a running sync undoing a settings reset.
func (r *AccountRouting) PollRevision(ctx context.Context, owner, id string) (int64, error) {
	_, err := r.System().Write().ExecContext(ctx, `INSERT OR IGNORE INTO gofer_account_poll_schedule(account_id)
		SELECT account_id FROM gofer_account_directory WHERE account_id=? AND user_id=? AND state='active'`, id, owner)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = r.System().Read().QueryRowContext(ctx, `SELECT p.revision FROM gofer_account_poll_schedule p
		JOIN gofer_account_directory d ON d.account_id=p.account_id
		WHERE d.account_id=? AND d.user_id=? AND d.state='active'`, id, owner).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrAccountRoute
	}
	return revision, err
}

func (r *AccountRouting) DeferAccountPoll(ctx context.Context, owner, id string, revision int64, next time.Time) (bool, error) {
	result, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_poll_schedule SET next_due_ms=?
		WHERE account_id=? AND revision=? AND EXISTS (SELECT 1 FROM gofer_account_directory d
		JOIN users u ON u.id=d.user_id WHERE d.account_id=? AND d.user_id=? AND d.state='active'
		AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)`, next.UnixMilli(), id, revision, id, owner)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (r *AccountRouting) ResetUserPolling(ctx context.Context, owner string) error {
	if err := r.ValidateUser(ctx, owner); err != nil {
		return err
	}
	_, err := r.System().Write().ExecContext(ctx, `INSERT INTO gofer_account_poll_schedule(account_id, next_due_ms, revision)
		SELECT d.account_id,0,1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
		WHERE d.user_id=? AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
		ON CONFLICT(account_id) DO UPDATE SET next_due_ms=0, revision=revision+1`, owner)
	return err
}

// ListDueAccounts pages only central metadata, including unscheduled accounts.
func (r *AccountRouting) ListDueAccounts(ctx context.Context, after string, now time.Time, limit int) ([]AccountRoute, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("account page size must be between 1 and 1000")
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT d.account_id,d.user_id,d.state FROM gofer_account_directory d
		JOIN users u ON u.id=d.user_id LEFT JOIN gofer_account_poll_schedule p ON p.account_id=d.account_id
		WHERE d.state='active' AND d.account_id>? AND COALESCE(p.next_due_ms,0)<=?
		AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
		ORDER BY d.account_id LIMIT ?`, after, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []AccountRoute
	for rows.Next() {
		var account AccountRoute
		if err := rows.Scan(&account.AccountID, &account.UserID, &account.State); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}
