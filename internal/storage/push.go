package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

type WebPushSubscription struct {
	Endpoint  string
	UserID    string
	P256DH    string
	Auth      string
	UserAgent string
	LastError string
	Revision  string
}

func (db *DB) SaveWebPushSubscription(ctx context.Context, sub WebPushSubscription) error {
	if sub.Endpoint == "" || sub.UserID == "" || sub.P256DH == "" || sub.Auth == "" {
		return fmt.Errorf("invalid web push subscription")
	}
	result, err := db.Write().ExecContext(ctx, `
		INSERT INTO web_push_subscriptions (endpoint, user_id, p256dh, auth, user_agent, last_error, revision)
		VALUES (?, ?, ?, ?, ?, '', ?)
		ON CONFLICT(endpoint) DO UPDATE SET
			user_id = excluded.user_id,
			p256dh = excluded.p256dh,
			auth = excluded.auth,
			user_agent = excluded.user_agent,
			last_error = '',
			revision = excluded.revision,
			updated_at = CURRENT_TIMESTAMP
		WHERE web_push_subscriptions.user_id = excluded.user_id`,
		sub.Endpoint, sub.UserID, sub.P256DH, sub.Auth, sub.UserAgent, uuid.NewString())
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) DeleteWebPushSubscription(ctx context.Context, userID, endpoint string) error {
	if userID == "" || endpoint == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint)
	return err
}

func (db *DB) DeleteWebPushSubscriptionEndpoint(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

func (db *DB) ListWebPushSubscriptions(ctx context.Context, userID string) ([]WebPushSubscription, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT endpoint, user_id, p256dh, auth, user_agent, last_error, revision
		FROM web_push_subscriptions
		WHERE user_id = ?
		ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []WebPushSubscription
	for rows.Next() {
		var sub WebPushSubscription
		if err := rows.Scan(&sub.Endpoint, &sub.UserID, &sub.P256DH, &sub.Auth, &sub.UserAgent, &sub.LastError, &sub.Revision); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// Delivery acknowledgements may arrive after an endpoint has been renewed or
// reassigned. They must only affect the exact registration that was sent to.
func (db *DB) DeleteWebPushSubscriptionIfCurrent(ctx context.Context, sub WebPushSubscription) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE endpoint = ? AND user_id = ? AND revision = ?`, sub.Endpoint, sub.UserID, sub.Revision)
	return err
}

func (db *DB) SetWebPushSubscriptionErrorIfCurrent(ctx context.Context, sub WebPushSubscription, message string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE web_push_subscriptions SET last_error = ?, updated_at = CURRENT_TIMESTAMP WHERE endpoint = ? AND user_id = ? AND revision = ?`, message, sub.Endpoint, sub.UserID, sub.Revision)
	return err
}

func migrateV105ToV106(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Some historical/partial schemas do not have the v37 push table. Restore
	// the current table before upgrading existing registrations.
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS web_push_subscriptions (
		endpoint TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		p256dh TEXT NOT NULL,
		auth TEXT NOT NULL,
		user_agent TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		revision TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_web_push_subscriptions_user ON web_push_subscriptions(user_id)`); err != nil {
		return err
	}
	hasRevision, err := columnExistsTx(tx, "web_push_subscriptions", "revision")
	if err != nil {
		return err
	}
	if !hasRevision {
		if _, err := tx.Exec(`ALTER TABLE web_push_subscriptions ADD COLUMN revision TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE web_push_subscriptions SET revision = lower(hex(randomblob(16))) WHERE revision = ''`); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 106); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) SetWebPushSubscriptionError(ctx context.Context, endpoint, errText string) error {
	if endpoint == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE web_push_subscriptions
		SET last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE endpoint = ?`, errText, endpoint)
	return err
}
