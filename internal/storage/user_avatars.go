package storage

import (
	"context"
	"strings"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
)

// ListUserSenderAvatarEmails preserves the legacy automatic sender-only scan.
// Contacts can still request visible avatars through the browser warmup route.
// This does not join the central cache or hold storage across provider calls.
func (db *DB) ListUserSenderAvatarEmails(ctx context.Context, owner, after string, limit int) ([]string, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT DISTINCT lower(trim(m.from_email)) email FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND lower(trim(m.from_email))>? AND instr(m.from_email,'@')>1 ORDER BY email LIMIT ?`, owner, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		emails = append(emails, email)
	}
	return emails, rows.Err()
}

// RecordUserAvatarInterests stores discovery hints, not authorization. Every
// image request and update event must still check current owned visibility.
func (r *AccountRouting) RecordUserAvatarInterests(ctx context.Context, owner string, emails []string) error {
	if err := r.ValidateUser(ctx, owner); err != nil {
		return err
	}
	tx, err := r.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0)`, owner).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrAccountRoute
	}
	for _, email := range emails {
		email = strings.ToLower(strings.TrimSpace(email))
		hash := avatarresolver.GravatarHash(email)
		if hash == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sender_avatars(email_hash,email,status) VALUES(?,?,'pending') ON CONFLICT(email_hash) DO NOTHING`, hash, email); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO gofer_avatar_interests(email_hash,user_id) SELECT ?,id FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0`, hash, owner); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *AccountRouting) ListAvatarInterestedUsers(ctx context.Context, hash, after string, limit int) ([]string, error) {
	if limit <= 0 || limit > 64 {
		limit = 64
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT i.user_id FROM gofer_avatar_interests i JOIN users u ON u.id=i.user_id WHERE i.email_hash=? AND i.user_id>? AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0 ORDER BY i.user_id LIMIT ?`, hash, after, limit)
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
