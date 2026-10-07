package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ReadUserSuppressedContacts copies the list and total from the same read
// snapshot. Rendering must happen after the repository lease is released.
func (db *DB) ReadUserSuppressedContacts(ctx context.Context, owner string, guard func(*sql.Tx) error) (contacts []models.Contact, count int, err error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, 0, err
	}
	contacts, err = listSuppressedContacts(ctx, tx, owner, 200)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_observations WHERE user_id=? AND is_suppressed=1 AND suppress_auto_create=1`, owner).Scan(&count); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return contacts, count, nil
}

// A nil ID clears the entire suppression list. A non-nil ID must match at
// least one suppressed observation belonging to this owner.
func (db *DB) ClearUserContactSuppression(ctx context.Context, owner string, id *string, guard func(*sql.Tx) error) (int64, error) {
	if id != nil && strings.TrimSpace(*id) == "" {
		return 0, sql.ErrNoRows
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return 0, err
	}
	query := `DELETE FROM contact_observations WHERE user_id=? AND is_suppressed=1 AND suppress_auto_create=1`
	args := []any{owner}
	if id != nil {
		query += ` AND (profile_id=? OR id=?)`
		args = append(args, strings.TrimSpace(*id), strings.TrimSpace(*id))
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if id != nil && count == 0 {
		return 0, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// DeleteUserObservedContacts keeps the existing local-only discovered-contact
// action. Policy, targets, tombstones, suppression, queue cancellation and
// activity commit together, with owner authorization rechecked after writer wait.
func (db *DB) DeleteUserObservedContacts(ctx context.Context, owner string, guard func(*sql.Tx) error) (int64, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return 0, err
	}
	prevent, err := contactDeletePolicyTx(ctx, tx, owner)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT co.profile_id FROM contact_observations co
 WHERE co.user_id=? AND co.is_suppressed=0 AND co.profile_id!=''
 AND NOT EXISTS(SELECT 1 FROM contact_fields cf WHERE cf.user_id=co.user_id AND cf.profile_id=co.profile_id AND cf.source='manual')
 ORDER BY co.profile_id`, owner)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	// One profile per statement avoids a variable-count limit for large lists.
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET is_deleted=1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=?`, owner, id)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if count != 1 {
			return 0, ErrContactEditChanged
		}
		if prevent {
			_, err = tx.ExecContext(ctx, `UPDATE contact_observations SET is_suppressed=1,suppress_auto_create=1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND profile_id=?`, owner, id)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM contact_observations WHERE user_id=? AND profile_id=?`, owner, id)
		}
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_operations SET status='done',locked_at=NULL,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND contact_id=? AND status IN ('pending','running')`, owner, id); err != nil {
			return 0, err
		}
	}
	var event ContactActivityNotification
	if len(ids) > 0 {
		message := "Discovered contacts deleted"
		if prevent {
			message += " and suppressed"
		}
		event = ContactActivityNotification{UserID: owner, EventType: "observed_contacts_deleted", Message: message, Count: len(ids)}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, owner, event.EventType, "", event.Message, event.Count); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		db.notifyContactActivity(event)
	}
	return int64(len(ids)), nil
}
