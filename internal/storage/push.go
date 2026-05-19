package storage

import (
	"context"
	"fmt"
)

type WebPushSubscription struct {
	Endpoint  string
	UserID    string
	P256DH    string
	Auth      string
	UserAgent string
	LastError string
}

func (db *DB) SaveWebPushSubscription(ctx context.Context, sub WebPushSubscription) error {
	if sub.Endpoint == "" || sub.UserID == "" || sub.P256DH == "" || sub.Auth == "" {
		return fmt.Errorf("invalid web push subscription")
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO web_push_subscriptions (endpoint, user_id, p256dh, auth, user_agent, last_error)
		VALUES (?, ?, ?, ?, ?, '')
		ON CONFLICT(endpoint) DO UPDATE SET
			user_id = excluded.user_id,
			p256dh = excluded.p256dh,
			auth = excluded.auth,
			user_agent = excluded.user_agent,
			last_error = '',
			updated_at = CURRENT_TIMESTAMP`,
		sub.Endpoint, sub.UserID, sub.P256DH, sub.Auth, sub.UserAgent)
	return err
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
		SELECT endpoint, user_id, p256dh, auth, user_agent, last_error
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
		if err := rows.Scan(&sub.Endpoint, &sub.UserID, &sub.P256DH, &sub.Auth, &sub.UserAgent, &sub.LastError); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
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
