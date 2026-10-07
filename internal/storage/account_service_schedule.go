package storage

import (
	"context"
	"errors"
	"time"
)

// Service schedules are central worker metadata, never a copy of local service
// configuration. Independent revisions prevent a completion undoing a wake.
type ScheduledAccountService string

const (
	ScheduledContacts ScheduledAccountService = "contacts"
	ScheduledCalendar ScheduledAccountService = "calendar"
)

func validScheduledService(service ScheduledAccountService) bool {
	return service == ScheduledContacts || service == ScheduledCalendar
}

func (r *AccountRouting) ListDueServiceAccounts(ctx context.Context, service ScheduledAccountService, after string, now time.Time, limit int, startup bool) ([]AccountRoute, error) {
	if !validScheduledService(service) || limit < 1 || limit > 64 {
		return nil, errors.New("service discovery needs a known service and a page of at most 64 accounts")
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT d.account_id,d.user_id,d.state FROM gofer_account_directory d
 JOIN users u ON u.id=d.user_id
 LEFT JOIN gofer_account_service_schedule p ON p.account_id=d.account_id AND p.service=?
 LEFT JOIN gofer_account_provider_retry retry ON retry.account_id=d.account_id
 WHERE d.state='active' AND d.account_id>? AND (? OR COALESCE(p.next_due_ms,0)<=?)
 AND COALESCE(retry.retry_until_ms,0)<=? AND u.status='active' AND u.deletion_pending=0
 AND u.user_type='webmail' AND u.is_admin=0 ORDER BY d.account_id LIMIT ?`, service, after, startup, now.UnixMilli(), now.UnixMilli(), limit)
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

func (r *AccountRouting) ReserveServiceAccount(ctx context.Context, owner, id string, service ScheduledAccountService, now, until time.Time, startup bool) (int64, bool, error) {
	revision, reserved, _, err := r.ReserveServiceAccountWithWake(ctx, owner, id, service, now, until, startup)
	return revision, reserved, err
}

// A zero deadline is a configuration/startup wake. Capture it inside the same
// central writer transaction as the reservation, so a concurrent wake cannot
// disappear between discovery and claiming work.
func (r *AccountRouting) ReserveServiceAccountWithWake(ctx context.Context, owner, id string, service ScheduledAccountService, now, until time.Time, startup bool) (int64, bool, bool, error) {
	if !validScheduledService(service) || !until.After(now) {
		return 0, false, false, errors.New("service reservation needs a known service and a future deadline")
	}
	tx, err := r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback()
	var forced bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT next_due_ms FROM gofer_account_service_schedule WHERE account_id=? AND service=?),0)=0`, id, service).Scan(&forced); err != nil {
		return 0, false, false, err
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO gofer_account_service_schedule(account_id,service,next_due_ms,revision)
 SELECT d.account_id,?,?,1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 LEFT JOIN gofer_account_provider_retry retry ON retry.account_id=d.account_id
 WHERE d.account_id=? AND d.user_id=? AND d.state='active' AND u.status='active' AND u.deletion_pending=0
 AND u.user_type='webmail' AND u.is_admin=0 AND COALESCE(retry.retry_until_ms,0)<=?
 ON CONFLICT(account_id,service) DO UPDATE SET next_due_ms=excluded.next_due_ms,revision=revision+1
 WHERE ? OR next_due_ms<=?`, service, until.UnixMilli(), id, owner, now.UnixMilli(), startup, now.UnixMilli())
	if err != nil {
		return 0, false, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return 0, false, false, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM gofer_account_service_schedule WHERE account_id=? AND service=?`, id, service).Scan(&revision); err != nil {
		return 0, false, false, err
	}
	err = tx.Commit()
	return revision, err == nil, forced, err
}

func (r *AccountRouting) CompleteServiceAccount(ctx context.Context, owner, id string, service ScheduledAccountService, revision int64, next time.Time) (bool, error) {
	if !validScheduledService(service) || next.IsZero() {
		return false, errors.New("service completion needs a known service and a deadline")
	}
	result, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_service_schedule SET next_due_ms=?
 WHERE account_id=? AND service=? AND revision=? AND EXISTS(SELECT 1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 WHERE d.account_id=? AND d.user_id=? AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)`, next.UnixMilli(), id, service, revision, id, owner)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// Empty id resets this owner's service deadlines after a preferences change.
// A reset never clears the separate provider cooldown.
func (r *AccountRouting) ResetServiceAccounts(ctx context.Context, owner, id string, service ScheduledAccountService) error {
	if !validScheduledService(service) {
		return errors.New("unknown scheduled service")
	}
	if err := r.ValidateUser(ctx, owner); err != nil {
		return err
	}
	result, err := r.System().Write().ExecContext(ctx, `INSERT INTO gofer_account_service_schedule(account_id,service,next_due_ms,revision)
 SELECT d.account_id,?,0,1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 WHERE d.user_id=? AND (?='' OR d.account_id=?) AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
 ON CONFLICT(account_id,service) DO UPDATE SET next_due_ms=0,revision=revision+1`, service, owner, id, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 && id != "" {
		return ErrAccountRoute
	}
	return err
}

// An owner with no account and no queued-work hint needs no database probe.
// A migrator must seed hints for copied pending queues without active accounts.
func (r *AccountRouting) ListDueContactQueueOwners(ctx context.Context, after string, now time.Time, limit int, startup bool) ([]string, error) {
	if limit < 1 || limit > 64 {
		return nil, errors.New("contact queue discovery pages must contain at most 64 owners")
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT u.id FROM users u LEFT JOIN gofer_contact_queue_schedule p ON p.user_id=u.id
 WHERE u.id>? AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
 AND (p.user_id IS NOT NULL OR EXISTS(SELECT 1 FROM gofer_account_directory d WHERE d.user_id=u.id AND d.state='active'))
 AND (? OR COALESCE(p.next_due_ms,0)<=?) ORDER BY u.id LIMIT ?`, after, startup, now.UnixMilli(), limit)
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

func (r *AccountRouting) ReserveContactQueue(ctx context.Context, owner string, now, until time.Time, startup bool) (int64, bool, error) {
	if !until.After(now) {
		return 0, false, errors.New("contact queue reservation needs a future deadline")
	}
	tx, err := r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision)
 SELECT id,?,1 FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0
 ON CONFLICT(user_id) DO UPDATE SET next_due_ms=excluded.next_due_ms,revision=revision+1
 WHERE ? OR next_due_ms<=?`, until.UnixMilli(), owner, startup, now.UnixMilli())
	if err != nil {
		return 0, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return 0, false, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM gofer_contact_queue_schedule WHERE user_id=?`, owner).Scan(&revision); err != nil {
		return 0, false, err
	}
	err = tx.Commit()
	return revision, err == nil, err
}

func (r *AccountRouting) CompleteContactQueue(ctx context.Context, owner string, revision int64, next time.Time) (bool, error) {
	if next.IsZero() {
		return false, errors.New("contact queue completion needs a deadline")
	}
	result, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_contact_queue_schedule SET next_due_ms=? WHERE user_id=? AND revision=?
 AND EXISTS(SELECT 1 FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0)`, next.UnixMilli(), owner, revision, owner)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (r *AccountRouting) ResetContactQueue(ctx context.Context, owner string) error {
	result, err := r.System().Write().ExecContext(ctx, `INSERT INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision)
 SELECT id,0,1 FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0
 ON CONFLICT(user_id) DO UPDATE SET next_due_ms=0,revision=revision+1`, owner)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrUserStoreOwner
	}
	return err
}

// Startup performs one central reset, rather than forcing reservations during
// discovery. Completed jobs then remain deferred even if a wake restarts paging.
func (r *AccountRouting) ResetContactSchedulesForStartup(ctx context.Context) error {
	tx, err := r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE gofer_account_service_schedule SET next_due_ms=0,revision=revision+1 WHERE service='contacts'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gofer_contact_queue_schedule SET next_due_ms=0,revision=revision+1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *AccountRouting) ResetCalendarSchedulesForStartup(ctx context.Context) error {
	_, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_service_schedule SET next_due_ms=0,revision=revision+1 WHERE service='calendar'`)
	return err
}
