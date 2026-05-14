package storage

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

type SenderAvatarRecord struct {
	EmailHash        string
	Email            string
	Source           string
	Status           string
	ContentType      string
	ImageData        []byte
	StoragePath      string
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
	Total           int                         `json:"total"`
	Pending         int                         `json:"pending"`
	Found           int                         `json:"found"`
	Missing         int                         `json:"missing"`
	Error           int                         `json:"error"`
	Due             int                         `json:"due"`
	GravatarChecked int                         `json:"gravatar_checked"`
	GravatarFound   int                         `json:"gravatar_found"`
	GravatarMissing int                         `json:"gravatar_missing"`
	GravatarError   int                         `json:"gravatar_error"`
	BIMIChecked     int                         `json:"bimi_checked"`
	BIMIFound       int                         `json:"bimi_found"`
	BIMIMissing     int                         `json:"bimi_missing"`
	BIMIError       int                         `json:"bimi_error"`
	BIMISkipped     int                         `json:"bimi_skipped"`
	OtherFound      int                         `json:"other_found"`
	ProviderStats   []SenderAvatarProviderStats `json:"provider_stats"`
}

type SenderAvatarProviderStats struct {
	Provider string `json:"provider"`
	InUse    int    `json:"in_use"`
	Checked  int    `json:"checked"`
	Found    int    `json:"found"`
	Missing  int    `json:"missing"`
	Skipped  int    `json:"skipped"`
	Error    int    `json:"error"`
}

type SenderAvatarAttemptLog struct {
	Email     string
	Provider  string
	Status    string
	Message   string
	CreatedAt time.Time
}

type SenderAvatarAttemptLogFilter struct {
	ErrorsOnly bool
	Query      string
	Provider   string
	Status     string
	Limit      int
	Offset     int
}

type SenderAvatarProviderState struct {
	Provider  string
	Status    string
	Message   string
	CheckedAt time.Time
	Checked   bool
}

type SenderAvatarRow struct {
	Email            string
	EmailHash        string
	Status           string
	Source           string
	ContentType      string
	ImageData        []byte
	StoragePath      string
	Error            string
	FetchedAt        time.Time
	FetchedAtValid   bool
	ExpiresAt        time.Time
	ExpiresAtValid   bool
	NextRetryAt      time.Time
	NextRetryAtValid bool
	UpdatedAt        time.Time
	Providers        []SenderAvatarProviderState
}

type SenderAvatarRowFilter struct {
	Query      string
	Status     string
	Source     string
	Provider   string
	ErrorsOnly bool
	Limit      int
	Offset     int
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
		`SELECT email_hash, email, source, status, content_type, image_data, storage_path, expires_at, next_retry_at, error
		 FROM sender_avatars
		 WHERE email_hash = ?`, strings.ToLower(strings.TrimSpace(hash))).Scan(
		&rec.EmailHash,
		&rec.Email,
		&rec.Source,
		&rec.Status,
		&rec.ContentType,
		&rec.ImageData,
		&rec.StoragePath,
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

func (db *DB) GetReusableDomainIconAvatar(ctx context.Context, hash, email string) (*SenderAvatarRecord, error) {
	domain := avatarresolver.EmailDomain(email)
	if domain == "" || avatarresolver.IsPublicMailboxDomain(domain) {
		return nil, nil
	}
	domainSuffix := "@" + domain

	var rec SenderAvatarRecord
	var expiresAt, nextRetryAt sql.NullTime
	err := db.Read().QueryRowContext(ctx,
		`SELECT email_hash, email, source, status, content_type, image_data, storage_path, expires_at, next_retry_at, error
		 FROM sender_avatars
		 WHERE status = 'found'
		  AND source = 'domain_icon'
		  AND email_hash != ?
		  AND substr(lower(trim(email)), -length(?)) = ?
		  AND expires_at IS NOT NULL
		  AND expires_at > CURRENT_TIMESTAMP
		  AND (storage_path != '' OR image_data IS NOT NULL)
		 ORDER BY fetched_at DESC, updated_at DESC
		 LIMIT 1`, strings.ToLower(strings.TrimSpace(hash)), domainSuffix, domainSuffix).Scan(
		&rec.EmailHash,
		&rec.Email,
		&rec.Source,
		&rec.Status,
		&rec.ContentType,
		&rec.ImageData,
		&rec.StoragePath,
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
	if !expiresAt.Valid {
		return nil, nil
	}
	rec.ExpiresAt = expiresAt.Time
	rec.ExpiresAtValid = true
	if nextRetryAt.Valid {
		rec.NextRetryAt = nextRetryAt.Time
		rec.NextRetryAtValid = true
	}
	return &rec, nil
}

func (db *DB) hydrateContactAvatar(ctx context.Context, contact *models.Contact) {
	if contact == nil || contact.AvatarHash == "" {
		return
	}
	rec, err := db.GetSenderAvatarByHash(ctx, contact.AvatarHash)
	if err != nil || rec == nil {
		contact.AvatarStatus = "unknown"
		return
	}
	contact.AvatarStatus = rec.Status
	contact.AvatarSource = rec.Source
	if rec.Status == "found" && (rec.StoragePath != "" || len(rec.ImageData) > 0) && (!rec.ExpiresAtValid || time.Now().Before(rec.ExpiresAt)) {
		contact.AvatarURL = senderAvatarURL(rec.EmailHash, rec.FetchedAtVersion())
	}
}

func (r SenderAvatarRecord) FetchedAtVersion() int64 {
	if r.ExpiresAtValid {
		return r.ExpiresAt.Unix()
	}
	return 0
}

func senderAvatarURL(hash string, version int64) string {
	if version > 0 {
		return "/api/avatars/" + strings.ToLower(strings.TrimSpace(hash)) + "?v=" + strconv.FormatInt(version, 10)
	}
	return "/api/avatars/" + strings.ToLower(strings.TrimSpace(hash))
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

func (db *DB) GetAllSenderAvatarCandidates(ctx context.Context, limit, offset int) ([]SenderAvatarCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Read().QueryContext(ctx,
		`SELECT email_hash, email
		 FROM sender_avatars
		 WHERE trim(email) != ''
		 ORDER BY email_hash ASC
		 LIMIT ? OFFSET ?`, limit, offset)
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

func (db *DB) GetSenderAvatarDomainCounts(ctx context.Context) (map[string]int, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT lower(substr(trim(email), instr(trim(email), '@') + 1)) AS domain, COUNT(*)
		 FROM sender_avatars
		 WHERE trim(email) != '' AND instr(trim(email), '@') > 1
		 GROUP BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var domain string
		var count int
		if err := rows.Scan(&domain, &count); err != nil {
			return nil, err
		}
		domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
		if domain != "" {
			counts[domain] = count
		}
	}
	return counts, rows.Err()
}

func normalizeProviderStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "found", "missing", "error", "skipped":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return "unchecked"
	}
}

func (db *DB) RecordSenderAvatarAttempt(ctx context.Context, hash, email, provider, status, message string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	status = normalizeProviderStatus(status)
	if provider == "" || status == "unchecked" {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO avatar_attempt_logs (email_hash, email, provider, status, message)
		 VALUES (?, ?, ?, ?, ?)`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), provider, status, message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO avatar_provider_states (email_hash, email, provider, status, message, checked_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(email_hash, provider) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE avatar_provider_states.email END,
		 	status = excluded.status,
		 	message = excluded.message,
		 	checked_at = CURRENT_TIMESTAMP,
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), provider, status, message); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CountSenderAvatarAttemptsSince(ctx context.Context, hash string, since time.Time) (int, error) {
	var count int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM avatar_attempt_logs
		 WHERE email_hash = ?
		  AND status IN ('found', 'missing', 'error')
		  AND created_at >= ?`, strings.ToLower(strings.TrimSpace(hash)), since).Scan(&count)
	return count, err
}

func (db *DB) SaveSenderAvatarFound(ctx context.Context, hash, email, source, contentType, storagePath string, data []byte, expiresAt time.Time, gravatarStatus, bimiStatus string) error {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "unknown"
	}
	gravatarStatus = normalizeProviderStatus(gravatarStatus)
	bimiStatus = normalizeProviderStatus(bimiStatus)
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, gravatar_status, gravatar_checked_at, bimi_status, bimi_checked_at, status, content_type, image_data, storage_path, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, ?, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, 'found', ?, ?, ?, CURRENT_TIMESTAMP, ?, NULL, '')
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = excluded.source,
		 	gravatar_status = excluded.gravatar_status,
		 	gravatar_checked_at = excluded.gravatar_checked_at,
		 	bimi_status = excluded.bimi_status,
		 	bimi_checked_at = excluded.bimi_checked_at,
		 	status = 'found',
		 	content_type = excluded.content_type,
		 	image_data = excluded.image_data,
		 	storage_path = excluded.storage_path,
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = excluded.expires_at,
		 	next_retry_at = NULL,
		 	error = '',
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), source, gravatarStatus, gravatarStatus, bimiStatus, bimiStatus, contentType, data, storagePath, expiresAt)
	return err
}

func SenderAvatarURL(hash string, expiresAt time.Time) string {
	version := int64(0)
	if !expiresAt.IsZero() {
		version = expiresAt.Unix()
	}
	return senderAvatarURL(hash, version)
}

func (db *DB) SaveSenderAvatarMissing(ctx context.Context, hash, email, source string, expiresAt time.Time, gravatarStatus, bimiStatus string) error {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "none"
	}
	gravatarStatus = normalizeProviderStatus(gravatarStatus)
	bimiStatus = normalizeProviderStatus(bimiStatus)
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, gravatar_status, gravatar_checked_at, bimi_status, bimi_checked_at, status, content_type, image_data, storage_path, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, ?, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, 'missing', '', NULL, '', CURRENT_TIMESTAMP, ?, NULL, '')
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = excluded.source,
		 	gravatar_status = excluded.gravatar_status,
		 	gravatar_checked_at = excluded.gravatar_checked_at,
		 	bimi_status = excluded.bimi_status,
		 	bimi_checked_at = excluded.bimi_checked_at,
		 	status = 'missing',
		 	content_type = '',
		 	image_data = NULL,
		 	storage_path = '',
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = excluded.expires_at,
		 	next_retry_at = NULL,
		 	error = '',
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), source, gravatarStatus, gravatarStatus, bimiStatus, bimiStatus, expiresAt)
	return err
}

func (db *DB) SaveSenderAvatarError(ctx context.Context, hash, email, source, message string, nextRetryAt time.Time, gravatarStatus, bimiStatus string) error {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "unknown"
	}
	gravatarStatus = normalizeProviderStatus(gravatarStatus)
	bimiStatus = normalizeProviderStatus(bimiStatus)
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO sender_avatars (email_hash, email, source, gravatar_status, gravatar_checked_at, bimi_status, bimi_checked_at, status, content_type, image_data, storage_path, fetched_at, expires_at, next_retry_at, error)
		 VALUES (?, ?, ?, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, ?, CASE WHEN ? IN ('found', 'missing', 'error') THEN CURRENT_TIMESTAMP ELSE NULL END, 'error', '', NULL, '', CURRENT_TIMESTAMP, NULL, ?, ?)
		 ON CONFLICT(email_hash) DO UPDATE SET
		 	email = CASE WHEN excluded.email != '' THEN excluded.email ELSE sender_avatars.email END,
		 	source = excluded.source,
		 	gravatar_status = excluded.gravatar_status,
		 	gravatar_checked_at = excluded.gravatar_checked_at,
		 	bimi_status = excluded.bimi_status,
		 	bimi_checked_at = excluded.bimi_checked_at,
		 	status = 'error',
		 	content_type = '',
		 	image_data = NULL,
		 	storage_path = '',
		 	fetched_at = CURRENT_TIMESTAMP,
		 	expires_at = NULL,
		 	next_retry_at = excluded.next_retry_at,
		 	error = excluded.error,
		 	updated_at = CURRENT_TIMESTAMP`, strings.ToLower(strings.TrimSpace(hash)), strings.ToLower(strings.TrimSpace(email)), source, gravatarStatus, gravatarStatus, bimiStatus, bimiStatus, nextRetryAt, message)
	return err
}

func (db *DB) GetRecentSenderAvatarAttemptLogs(ctx context.Context, limit int) ([]SenderAvatarAttemptLog, error) {
	logs, _, err := db.GetSenderAvatarAttemptLogs(ctx, SenderAvatarAttemptLogFilter{Limit: limit})
	return logs, err
}

func (db *DB) GetRecentSenderAvatarErrorLogs(ctx context.Context, limit int) ([]SenderAvatarAttemptLog, error) {
	logs, _, err := db.GetSenderAvatarAttemptLogs(ctx, SenderAvatarAttemptLogFilter{ErrorsOnly: true, Limit: limit})
	return logs, err
}

func (db *DB) GetSenderAvatarAttemptLogs(ctx context.Context, filter SenderAvatarAttemptLogFilter) ([]SenderAvatarAttemptLog, int, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	clauses := []string{}
	args := []any{}
	if filter.ErrorsOnly {
		clauses = append(clauses, "status = 'error'")
	}
	if filter.Query = strings.ToLower(strings.TrimSpace(filter.Query)); filter.Query != "" {
		clauses = append(clauses, "lower(email) LIKE ?")
		args = append(args, "%"+filter.Query+"%")
	}
	if filter.Provider = strings.ToLower(strings.TrimSpace(filter.Provider)); filter.Provider != "" && filter.Provider != "all" {
		clauses = append(clauses, "provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.Status = strings.ToLower(strings.TrimSpace(filter.Status)); filter.Status != "" && filter.Status != "all" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}

	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM avatar_attempt_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT email, provider, status, message, created_at
		 FROM avatar_attempt_logs` + where + `
		 ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), filter.Limit, filter.Offset)
	rows, err := db.Read().QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []SenderAvatarAttemptLog
	for rows.Next() {
		var entry SenderAvatarAttemptLog
		if err := rows.Scan(&entry.Email, &entry.Provider, &entry.Status, &entry.Message, &entry.CreatedAt); err != nil {
			return nil, 0, err
		}
		logs = append(logs, entry)
	}
	return logs, total, rows.Err()
}

func (db *DB) GetSenderAvatarRows(ctx context.Context, filter SenderAvatarRowFilter) ([]SenderAvatarRow, int, error) {
	if filter.Limit <= 0 {
		filter.Limit = 80
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	clauses := []string{}
	args := []any{}
	if filter.Query = strings.ToLower(strings.TrimSpace(filter.Query)); filter.Query != "" {
		clauses = append(clauses, "lower(sa.email) LIKE ?")
		args = append(args, "%"+filter.Query+"%")
	}
	if filter.Status = strings.ToLower(strings.TrimSpace(filter.Status)); filter.Status != "" && filter.Status != "all" {
		clauses = append(clauses, "sa.status = ?")
		args = append(args, filter.Status)
	}
	if filter.Source = strings.ToLower(strings.TrimSpace(filter.Source)); filter.Source != "" && filter.Source != "all" {
		clauses = append(clauses, "sa.source = ?")
		args = append(args, filter.Source)
	}
	if filter.Provider = strings.ToLower(strings.TrimSpace(filter.Provider)); filter.Provider != "" && filter.Provider != "all" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM avatar_provider_states aps WHERE aps.email_hash = sa.email_hash AND aps.provider = ?)")
		args = append(args, filter.Provider)
	}
	if filter.ErrorsOnly {
		clauses = append(clauses, "(sa.status = 'error' OR EXISTS (SELECT 1 FROM avatar_provider_states aps2 WHERE aps2.email_hash = sa.email_hash AND aps2.status = 'error'))")
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM sender_avatars sa`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT sa.email_hash, sa.email, sa.status, sa.source, sa.content_type, sa.image_data, sa.storage_path, sa.error, sa.fetched_at, sa.expires_at, sa.next_retry_at, sa.updated_at
		 FROM sender_avatars sa` + where + `
		 ORDER BY sa.updated_at DESC, sa.email ASC
		 LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), filter.Limit, filter.Offset)
	rows, err := db.Read().QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	results := []SenderAvatarRow{}
	for rows.Next() {
		var row SenderAvatarRow
		var fetchedAt, expiresAt, nextRetryAt sql.NullTime
		if err := rows.Scan(&row.EmailHash, &row.Email, &row.Status, &row.Source, &row.ContentType, &row.ImageData, &row.StoragePath, &row.Error, &fetchedAt, &expiresAt, &nextRetryAt, &row.UpdatedAt); err != nil {
			return nil, 0, err
		}
		if fetchedAt.Valid {
			row.FetchedAt = fetchedAt.Time
			row.FetchedAtValid = true
		}
		if expiresAt.Valid {
			row.ExpiresAt = expiresAt.Time
			row.ExpiresAtValid = true
		}
		if nextRetryAt.Valid {
			row.NextRetryAt = nextRetryAt.Time
			row.NextRetryAtValid = true
		}
		providers, err := db.GetSenderAvatarProviderStates(ctx, row.EmailHash)
		if err != nil {
			return nil, 0, err
		}
		row.Providers = providers
		results = append(results, row)
	}
	return results, total, rows.Err()
}

func (db *DB) GetSenderAvatarProviderStates(ctx context.Context, hash string) ([]SenderAvatarProviderState, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT provider, status, message, checked_at
		 FROM avatar_provider_states
		 WHERE email_hash = ?
		 ORDER BY provider ASC`, strings.ToLower(strings.TrimSpace(hash)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	states := []SenderAvatarProviderState{}
	for rows.Next() {
		var state SenderAvatarProviderState
		var checkedAt sql.NullTime
		if err := rows.Scan(&state.Provider, &state.Status, &state.Message, &checkedAt); err != nil {
			return nil, err
		}
		if checkedAt.Valid {
			state.CheckedAt = checkedAt.Time
			state.Checked = true
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (db *DB) GetAvatarProviderNames(ctx context.Context) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT provider
		 FROM avatar_provider_states
		 GROUP BY provider
		 ORDER BY provider ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	providers := []string{}
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

func (db *DB) GetSenderAvatarStats(ctx context.Context) (SenderAvatarStats, error) {
	var stats SenderAvatarStats
	providerStats := map[string]*SenderAvatarProviderStats{}
	providerStat := func(provider string) *SenderAvatarProviderStats {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			provider = "unknown"
		}
		if existing := providerStats[provider]; existing != nil {
			return existing
		}
		entry := &SenderAvatarProviderStats{Provider: provider}
		providerStats[provider] = entry
		return entry
	}
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

	rows, err = db.Read().QueryContext(ctx, `SELECT source, COUNT(*) FROM sender_avatars WHERE status = 'found' GROUP BY source`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var source string
		var count int
		if err := rows.Scan(&source, &count); err != nil {
			return stats, err
		}
		source = strings.ToLower(strings.TrimSpace(source))
		providerStat(source).InUse = count
		switch source {
		case "gravatar":
			stats.GravatarFound = count
		case "libravatar", "domain_icon":
		case "bimi":
			stats.BIMIFound = count
		default:
			stats.OtherFound += count
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	rows, err = db.Read().QueryContext(ctx, `SELECT provider, status, COUNT(*) FROM avatar_provider_states GROUP BY provider, status`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var provider, status string
		var count int
		if err := rows.Scan(&provider, &status, &count); err != nil {
			return stats, err
		}
		entry := providerStat(provider)
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "found":
			entry.Checked += count
			entry.Found = count
		case "missing":
			entry.Checked += count
			entry.Missing = count
		case "error":
			entry.Checked += count
			entry.Error = count
		case "skipped":
			entry.Skipped = count
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	rows, err = db.Read().QueryContext(ctx, `SELECT gravatar_status, COUNT(*) FROM sender_avatars GROUP BY gravatar_status`)
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
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "found":
			stats.GravatarChecked += count
			stats.GravatarFound = count
		case "missing":
			stats.GravatarChecked += count
			stats.GravatarMissing = count
		case "error":
			stats.GravatarChecked += count
			stats.GravatarError = count
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	rows, err = db.Read().QueryContext(ctx, `SELECT bimi_status, COUNT(*) FROM sender_avatars GROUP BY bimi_status`)
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
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "found":
			stats.BIMIChecked += count
			stats.BIMIFound = count
		case "missing":
			stats.BIMIChecked += count
			stats.BIMIMissing = count
		case "error":
			stats.BIMIChecked += count
			stats.BIMIError = count
		case "skipped":
			stats.BIMISkipped = count
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
	if err != nil {
		return stats, err
	}
	for _, entry := range providerStats {
		stats.ProviderStats = append(stats.ProviderStats, *entry)
	}
	sort.Slice(stats.ProviderStats, func(i, j int) bool {
		return stats.ProviderStats[i].Provider < stats.ProviderStats[j].Provider
	})
	return stats, err
}
