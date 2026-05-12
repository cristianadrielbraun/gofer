package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
)

type SenderAvatarRecord struct {
	EmailHash        string
	Email            string
	Status           string
	ContentType      string
	ImageData        []byte
	ExpiresAt        time.Time
	ExpiresAtValid   bool
	NextRetryAt      time.Time
	NextRetryAtValid bool
	Error            string
}

type SenderAvatarCandidate struct {
	EmailHash string
	Email     string
}

type SenderAvatarStats struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
	Found   int `json:"found"`
	Missing int `json:"missing"`
	Error   int `json:"error"`
	Due     int `json:"due"`
}

func (db *DB) EnsureSenderAvatarCandidates(ctx context.Context) (int, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT DISTINCT lower(trim(from_email))
		 FROM messages
		 WHERE trim(from_email) != '' AND instr(from_email, '@') > 1
		 ORDER BY 1`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return count, err
		}
		if err := db.UpsertSenderAvatarCandidate(ctx, email); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

func (db *DB) UpsertSenderAvatarCandidate(ctx context.Context, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	hash := avatarresolver.GravatarHash(email)
	if hash == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, status)
		 VALUES (?, ?, 'pending')
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN sender_avatars.email = '' THEN excluded.email ELSE sender_avatars.email END,
		 	updated_at = CURRENT_TIMESTAMP`, hash, email)
	return err
}

func (db *DB) GetSenderAvatarByHash(ctx context.Context, hash string) (*SenderAvatarRecord, error) {
	var rec SenderAvatarRecord
	var expiresAt, nextRetryAt sql.NullTime
	err := db.Read().QueryRowContext(ctx,
		`SELECT email_hash, email, status, content_type, image_data, expires_at, next_retry_at, error
		 FROM sender_avatars
		 WHERE email_hash = ?`, strings.ToLower(strings.TrimSpace(hash))).Scan(
		&rec.EmailHash,
		&rec.Email,
		&rec.Status,
		&rec.ContentType,
		&rec.ImageData,
		&expiresAt,
		&nextRetryAt,
		&rec.Error,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		rec.ExpiresAt = expiresAt.Time
		rec.ExpiresAtValid = true
	}
	if nextRetryAt.Valid {
		rec.NextRetryAt = nextRetryAt.Time
		rec.NextRetryAtValid = true
	}
	return &rec, nil
}

func (db *DB) GetDueSenderAvatarCandidates(ctx context.Context, limit int) ([]SenderAvatarCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Read().QueryContext(ctx,
		`SELECT email_hash, email
		 FROM sender_avatars
		 WHERE status = 'pending'
		 	OR (status = 'error' AND (next_retry_at IS NULL OR next_retry_at <= CURRENT_TIMESTAMP))
		 	OR (status IN ('found', 'missing') AND (expires_at IS NULL OR expires_at <= CURRENT_TIMESTAMP))
		 ORDER BY updated_at ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []SenderAvatarCandidate
	for rows.Next() {
		var c SenderAvatarCandidate
		if err := rows.Scan(&c.EmailHash, &c.Email); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (db *DB) SaveSenderAvatarFound(ctx context.Context, hash, email, contentType string, data []byte, expiresAt time.Time) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, status, content_type, image_data, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, 'gravatar', 'found', ?, ?, CURRENT_TIMESTAMP, ?, NULL, '')
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = 'gravatar',
		 	status = 'found',
		 	content_type = excluded.content_type,
		 	image_data = excluded.image_data,
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = excluded.expires_at,
		 	next_retry_at = NULL,
		 	error = '',
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), contentType, data, expiresAt)
	return err
}

func (db *DB) SaveSenderAvatarMissing(ctx context.Context, hash, email string, expiresAt time.Time) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, status, content_type, image_data, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, 'gravatar', 'missing', '', NULL, CURRENT_TIMESTAMP, ?, NULL, '')
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = 'gravatar',
		 	status = 'missing',
		 	content_type = '',
		 	image_data = NULL,
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = excluded.expires_at,
		 	next_retry_at = NULL,
		 	error = '',
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), expiresAt)
	return err
}

func (db *DB) SaveSenderAvatarError(ctx context.Context, hash, email, message string, nextRetryAt time.Time) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, status, content_type, image_data, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, 'gravatar', 'error', '', NULL, CURRENT_TIMESTAMP, NULL, ?, ?)
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = 'gravatar',
		 	status = 'error',
		 	content_type = '',
		 	image_data = NULL,
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = NULL,
		 	next_retry_at = excluded.next_retry_at,
		 	error = excluded.error,
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), nextRetryAt, message)
	return err
}

func (db *DB) GetSenderAvatarStats(ctx context.Context) (SenderAvatarStats, error) {
	var stats SenderAvatarStats
	rows, err := db.Read().QueryContext(ctx, `SELECT status, COUNT(*) FROM sender_avatars GROUP BY status`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return stats, err
		}
		stats.Total += count
		switch status {
		case "pending":
			stats.Pending = count
		case "found":
			stats.Found = count
		case "missing":
			stats.Missing = count
		case "error":
			stats.Error = count
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	err = db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM sender_avatars
		 WHERE status = 'pending'
		 	OR (status = 'error' AND (next_retry_at IS NULL OR next_retry_at <= CURRENT_TIMESTAMP))
		 	OR (status IN ('found', 'missing') AND (expires_at IS NULL OR expires_at <= CURRENT_TIMESTAMP))`).Scan(&stats.Due)
	return stats, err
}
