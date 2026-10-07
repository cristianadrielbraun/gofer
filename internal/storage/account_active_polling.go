package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ActivePollOwner is an in-process browser-session boundary. Neither sessions
// nor provider/mailbox configuration are copied into the directory.
type ActivePollOwner struct {
	UserID string
	Since  time.Time
}
type ActivePollReservation struct {
	Revision int64
	Failures int
}

// ListActivePollAccounts reads central scheduling metadata for a bounded batch
// of connected owners. A new browser session can advance a successful deadline,
// but never an error backoff or a provider Retry-After.
func (r *AccountRouting) ListActivePollAccounts(ctx context.Context, owners []ActivePollOwner, after string, now time.Time, limit int) ([]AccountRoute, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	if len(owners) > 64 || limit < 1 || limit > 64 {
		return nil, errors.New("active polling pages must contain at most 64 owners and accounts")
	}
	values := make([]string, 0, len(owners))
	args := make([]any, 0, 2*len(owners)+4)
	seen := make(map[string]bool, len(owners))
	for _, owner := range owners {
		if owner.UserID == "" || owner.Since.IsZero() || seen[owner.UserID] {
			return nil, errors.New("active polling requires distinct owners and session boundaries")
		}
		seen[owner.UserID] = true
		values = append(values, "(?,?)")
		args = append(args, owner.UserID, owner.Since.UnixNano())
	}
	args = append(args, after, now.UnixMilli(), now.UnixMilli(), limit)
	rows, err := r.System().Read().QueryContext(ctx, `WITH connected(user_id,since_ns) AS (VALUES `+strings.Join(values, ",")+`)
 SELECT d.account_id,d.user_id,d.state FROM connected c JOIN gofer_account_directory d ON d.user_id=c.user_id
 JOIN users u ON u.id=d.user_id LEFT JOIN gofer_account_active_poll p ON p.account_id=d.account_id
 LEFT JOIN gofer_account_provider_retry retry ON retry.account_id=d.account_id
 WHERE d.state='active' AND d.account_id>?
 AND (COALESCE(p.next_due_ms,0)<=? OR (COALESCE(p.failures,0)=0 AND COALESCE(p.last_attempt_ns,0)<c.since_ns))
 AND COALESCE(retry.retry_until_ms,0)<=? AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
 ORDER BY d.account_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AccountRoute
	for rows.Next() {
		var entry AccountRoute
		if err := rows.Scan(&entry.AccountID, &entry.UserID, &entry.State); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ReserveActivePoll atomically excludes duplicate checks, even across session
// reconnects. The revision prevents late completion undoing a settings reset.
func (r *AccountRouting) ReserveActivePoll(ctx context.Context, owner, id string, since, now, next time.Time) (ActivePollReservation, bool, error) {
	tx, err := r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return ActivePollReservation{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO gofer_account_active_poll(account_id,next_due_ms,last_attempt_ns,revision)
 SELECT d.account_id,?,?,1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 LEFT JOIN gofer_account_provider_retry retry ON retry.account_id=d.account_id
 WHERE d.account_id=? AND d.user_id=? AND d.state='active' AND u.status='active' AND u.deletion_pending=0
 AND u.user_type='webmail' AND u.is_admin=0 AND COALESCE(retry.retry_until_ms,0)<=?
 ON CONFLICT(account_id) DO UPDATE SET next_due_ms=excluded.next_due_ms,last_attempt_ns=excluded.last_attempt_ns,revision=revision+1
 WHERE next_due_ms<=? OR (failures=0 AND last_attempt_ns<?)`, next.UnixMilli(), now.UnixNano(), id, owner, now.UnixMilli(), now.UnixMilli(), since.UnixNano())
	if err != nil {
		return ActivePollReservation{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return ActivePollReservation{}, false, err
	}
	if count == 0 {
		return ActivePollReservation{}, false, nil
	}
	var reservation ActivePollReservation
	if err := tx.QueryRowContext(ctx, `SELECT revision,failures FROM gofer_account_active_poll WHERE account_id=?`, id).Scan(&reservation.Revision, &reservation.Failures); err != nil {
		return reservation, false, err
	}
	err = tx.Commit()
	return reservation, err == nil, err
}

func (r *AccountRouting) CompleteActivePoll(ctx context.Context, owner, id string, revision int64, next time.Time, failures int) (bool, error) {
	if failures < 0 || failures > 4 {
		return false, errors.New("active polling backoff must be between zero and four")
	}
	result, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_active_poll SET next_due_ms=?,failures=?
 WHERE account_id=? AND revision=? AND EXISTS(SELECT 1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 WHERE d.account_id=? AND d.user_id=? AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)`, next.UnixMilli(), failures, id, revision, id, owner)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// ResetActivePolling runs after local preferences/account configuration commit.
// Empty id resets all of the owner's active account deadlines.
func (r *AccountRouting) ResetActivePolling(ctx context.Context, owner, id string) error {
	if err := r.ValidateUser(ctx, owner); err != nil {
		return err
	}
	result, err := r.System().Write().ExecContext(ctx, `INSERT INTO gofer_account_active_poll(account_id,next_due_ms,last_attempt_ns,failures,revision)
 SELECT account_id,0,0,0,1 FROM gofer_account_directory WHERE user_id=? AND state='active' AND (?='' OR account_id=?)
 ON CONFLICT(account_id) DO UPDATE SET next_due_ms=0,last_attempt_ns=0,failures=0,revision=revision+1`, owner, id, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && id != "" && count != 1 {
		return ErrAccountRoute
	}
	return err
}

// ResetActivePollingForAccount is for trusted account lifecycle hooks.
func (r *AccountRouting) ResetActivePollingForAccount(ctx context.Context, id string) error {
	var owner string
	err := r.System().Read().QueryRowContext(ctx, `SELECT user_id FROM gofer_account_directory WHERE account_id=? AND state='active'`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountRoute
	}
	if err != nil {
		return err
	}
	return r.ResetActivePolling(ctx, owner, id)
}
