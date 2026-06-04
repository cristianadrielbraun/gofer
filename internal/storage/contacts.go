package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

type ContactSettings struct {
	AutoCreateObserved     bool
	PreventRecreateDeleted bool
	ObserveSenders         bool
	ObserveRecipients      bool
}

type ContactSource struct {
	ContactID     string
	UserID        string
	Provider      string
	AccountID     string
	AddressBookID string
	RemoteID      string
	Etag          string
	SyncToken     string
}

type ContactSyncOperationPayload struct {
	Contact  models.Contact  `json:"contact"`
	Previous *models.Contact `json:"previous,omitempty"`
}

type ContactSyncOperation struct {
	ID           string
	UserID       string
	ContactID    string
	Email        string
	Payload      ContactSyncOperationPayload
	Status       string
	AttemptCount int
	LastError    string
	LockedAt     time.Time
	NextAttempt  time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func normalizeContactEmail(email string) string {
	email = strings.TrimSpace(strings.TrimPrefix(email, "mailto:"))
	email = strings.Trim(email, "<>")
	email = strings.ToLower(email)
	if email == "" || !strings.Contains(email, "@") {
		return ""
	}
	return email
}

func contactDisplayName(name, email string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(email)
}

func boolSetting(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}

func (db *DB) GetContactSettings(ctx context.Context, userID string) ContactSettings {
	settings := db.GetUISettings(ctx, userID)
	sources := uiSettingCSV(settings["contacts_observed_sources"], "senders,recipients")
	return ContactSettings{
		AutoCreateObserved:     boolSetting(settings["contacts_auto_create_observed"], true),
		PreventRecreateDeleted: boolSetting(settings["contacts_prevent_recreate_deleted"], true),
		ObserveSenders:         sources["senders"],
		ObserveRecipients:      sources["recipients"],
	}
}

func (db *DB) LogContactActivity(ctx context.Context, userID, eventType, email, message string, count int) error {
	if userID == "" || eventType == "" {
		return nil
	}
	if count < 0 {
		count = 0
	}
	email = strings.TrimSpace(email)
	message = strings.TrimSpace(message)
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO contact_activity_events (user_id, event_type, email, message, event_count)
		VALUES (?, ?, ?, ?, ?)`, userID, eventType, email, message, count)
	if err == nil {
		db.notifyContactActivity(ContactActivityNotification{UserID: userID, EventType: eventType, Email: email, Message: message, Count: count, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	return err
}

func (db *DB) GetContactAdminStatus(ctx context.Context, userID string) (models.ContactAdminStatus, error) {
	var status models.ContactAdminStatus
	counts := []struct {
		dest  *int
		query string
	}{
		{&status.Total, `SELECT COUNT(*) FROM contacts WHERE user_id = ? AND is_deleted = 0`},
		{&status.Manual, `SELECT COUNT(*) FROM contacts WHERE user_id = ? AND is_deleted = 0 AND is_manual = 1`},
		{&status.Observed, `SELECT COUNT(*) FROM contacts WHERE user_id = ? AND is_deleted = 0 AND is_manual = 0`},
		{&status.Suppressed, `SELECT COUNT(*) FROM contacts WHERE user_id = ? AND is_deleted = 1 AND suppress_auto_create = 1`},
		{&status.AddedToday, `SELECT COUNT(*) FROM contact_activity_events WHERE user_id = ? AND event_type IN ('manual_contact_added', 'observed_contact_added') AND created_at >= datetime('now', '-1 day')`},
		{&status.DeletedToday, `SELECT COALESCE(SUM(CASE WHEN event_count > 0 THEN event_count ELSE 1 END), 0) FROM contact_activity_events WHERE user_id = ? AND event_type IN ('contact_deleted', 'observed_contacts_deleted') AND created_at >= datetime('now', '-1 day')`},
	}
	for _, item := range counts {
		if err := db.Read().QueryRowContext(ctx, item.query, userID).Scan(item.dest); err != nil {
			return status, err
		}
	}

	var lastBackfillRaw sql.NullString
	if err := db.Read().QueryRowContext(ctx, `
		SELECT MAX(created_at)
		FROM contact_activity_events
		WHERE user_id = ? AND event_type = 'backfill_completed'`, userID).Scan(&lastBackfillRaw); err != nil {
		return status, err
	}
	if lastBackfillRaw.Valid {
		if t, ok := parseSQLiteDateTime(lastBackfillRaw.String); ok {
			status.LastBackfill = t
		}
	}

	rows, err := db.Read().QueryContext(ctx, `
		SELECT event_type, email, message, event_count, created_at
		FROM contact_activity_events
		WHERE user_id = ?
		ORDER BY created_at DESC
		LIMIT 50`, userID)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	for rows.Next() {
		var event models.ContactActivityEvent
		var createdAt string
		if err := rows.Scan(&event.Type, &event.Email, &event.Message, &event.Count, &createdAt); err != nil {
			return status, err
		}
		if t, ok := parseSQLiteDateTime(createdAt); ok {
			event.CreatedAt = t
		}
		status.RecentEvents = append(status.RecentEvents, event)
	}
	if err := rows.Err(); err != nil {
		return status, err
	}

	accountSync, err := db.ListContactSyncStatuses(ctx, userID)
	if err != nil {
		return status, err
	}
	status.AccountSync = accountSync
	return status, nil
}

func (db *DB) ListContactSyncStatuses(ctx context.Context, userID string) ([]models.ContactSyncStatus, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT a.id,
		       COALESCE(NULLIF(a.display_name, ''), a.email_address) AS account_name,
		       a.email_address,
		       CASE WHEN a.provider IN ('gmail', 'outlook') THEN a.provider ELSE COALESCE(acc.provider, '') END AS contact_provider,
		       CASE WHEN a.provider IN ('gmail', 'outlook') THEN COALESCE(acc.enabled, 1) ELSE COALESCE(acc.enabled, 0) END AS enabled,
		       CASE WHEN a.provider IN ('gmail', 'outlook') OR acc.account_id IS NOT NULL THEN 1 ELSE 0 END AS capable,
		       acc.last_started_at,
		       acc.last_success_at,
		       COALESCE(acc.last_import_count, 0),
		       COALESCE(acc.last_error, '')
		FROM accounts a
		LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		WHERE a.user_id = ?
		  AND COALESCE(a.is_deleting, 0) = 0
		  AND (a.provider IN ('gmail', 'outlook') OR acc.account_id IS NOT NULL)
		ORDER BY a.email_address COLLATE NOCASE`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []models.ContactSyncStatus
	for rows.Next() {
		var status models.ContactSyncStatus
		var enabled, capable int
		var lastStarted, lastSuccess sql.NullString
		if err := rows.Scan(&status.AccountID, &status.AccountName, &status.AccountEmail, &status.Provider, &enabled, &capable, &lastStarted, &lastSuccess, &status.LastImportCount, &status.LastError); err != nil {
			return nil, err
		}
		status.Enabled = enabled == 1
		status.Capable = capable == 1
		if lastStarted.Valid {
			if t, ok := parseSQLiteDateTime(lastStarted.String); ok {
				status.LastStartedAt = t
			}
		}
		if lastSuccess.Valid {
			if t, ok := parseSQLiteDateTime(lastSuccess.String); ok {
				status.LastSuccessAt = t
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func (db *DB) MarkContactSyncStarted(ctx context.Context, userID, accountID, provider string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "carddav"
	}
	enabled := 0
	if isBuiltinContactSyncProvider(provider) {
		enabled = 1
		_ = db.Read().QueryRowContext(ctx, `SELECT COALESCE(enabled, 1) FROM account_contact_sync_configs WHERE account_id = ? AND user_id = ?`, accountID, userID).Scan(&enabled)
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled, last_started_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(account_id) DO UPDATE SET
			user_id = excluded.user_id,
			provider = excluded.provider,
			enabled = account_contact_sync_configs.enabled,
			last_started_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP`, accountID, userID, provider, enabled)
	return err
}

func (db *DB) MarkContactSyncSuccess(ctx context.Context, userID, accountID, provider, syncToken string, importCount int) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "carddav"
	}
	if importCount < 0 {
		importCount = 0
	}
	enabled := 0
	if isBuiltinContactSyncProvider(provider) {
		enabled = 1
		_ = db.Read().QueryRowContext(ctx, `SELECT COALESCE(enabled, 1) FROM account_contact_sync_configs WHERE account_id = ? AND user_id = ?`, accountID, userID).Scan(&enabled)
	}
	syncToken = strings.TrimSpace(syncToken)
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled, last_sync_token, last_success_at, last_import_count, last_error)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, '')
		ON CONFLICT(account_id) DO UPDATE SET
			user_id = excluded.user_id,
			provider = excluded.provider,
			enabled = account_contact_sync_configs.enabled,
			last_sync_token = CASE WHEN excluded.last_sync_token != '' THEN excluded.last_sync_token ELSE account_contact_sync_configs.last_sync_token END,
			last_success_at = CURRENT_TIMESTAMP,
			last_import_count = excluded.last_import_count,
			last_error = '',
			updated_at = CURRENT_TIMESTAMP`, accountID, userID, provider, enabled, syncToken, importCount)
	return err
}

func (db *DB) MarkContactSyncError(ctx context.Context, userID, accountID, provider, message string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "carddav"
	}
	enabled := 0
	if isBuiltinContactSyncProvider(provider) {
		enabled = 1
		_ = db.Read().QueryRowContext(ctx, `SELECT COALESCE(enabled, 1) FROM account_contact_sync_configs WHERE account_id = ? AND user_id = ?`, accountID, userID).Scan(&enabled)
	}
	message = strings.TrimSpace(message)
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled, last_error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			user_id = excluded.user_id,
			provider = excluded.provider,
			enabled = account_contact_sync_configs.enabled,
			last_error = excluded.last_error,
			updated_at = CURRENT_TIMESTAMP`, accountID, userID, provider, enabled, message)
	return err
}

func isBuiltinContactSyncProvider(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "gmail", "outlook":
		return true
	default:
		return false
	}
}

func uiSettingCSV(value, fallback string) map[string]bool {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	result := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result[part] = true
		}
	}
	return result
}

func (db *DB) ListContacts(ctx context.Context, userID string, filters models.ContactFilters, limit, offset int) ([]models.Contact, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	profileContacts, err := db.listProfileContacts(ctx, userID, filters)
	if err != nil {
		return nil, err
	}
	legacyContacts, err := db.listLegacyContacts(ctx, userID, filters)
	if err != nil {
		return nil, err
	}
	contacts := append(profileContacts, legacyContacts...)
	sortContactsForList(contacts)
	if offset >= len(contacts) {
		return nil, nil
	}
	end := offset + limit
	if end > len(contacts) {
		end = len(contacts)
	}
	return contacts[offset:end], nil
}

func (db *DB) listLegacyContacts(ctx context.Context, userID string, filters models.ContactFilters) ([]models.Contact, error) {
	where, args := contactLegacyFilterSQL(userID, filters)

	rows, err := db.Read().QueryContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE `+where+`
		ORDER BY COALESCE(ce.last_seen_at, c.updated_at) DESC, c.display_name COLLATE NOCASE
		LIMIT 1000`, args...)
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	loc := timezoneLocationFromContext(ctx)
	for rows.Next() {
		c, err := scanContactRow(rows, loc)
		if err != nil {
			return nil, err
		}
		db.hydrateContactAvatar(ctx, &c)
		_ = db.hydrateContactAddressBooks(ctx, userID, &c)
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (db *DB) CountContacts(ctx context.Context, userID string, filters models.ContactFilters) (int, error) {
	profileCount, err := db.countProfileContacts(ctx, userID, filters)
	if err != nil {
		return 0, err
	}
	where, args := contactLegacyFilterSQL(userID, filters)
	var legacyCount int
	err = db.Read().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT c.id)
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id
		WHERE `+where, args...).Scan(&legacyCount)
	return profileCount + legacyCount, err
}

func (db *DB) ListContactsForExport(ctx context.Context, userID string) ([]models.Contact, error) {
	profiles, err := db.listProfileContacts(ctx, userID, models.ContactFilters{})
	if err != nil {
		return nil, err
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE c.user_id = ? AND c.is_deleted = 0
		ORDER BY c.display_name COLLATE NOCASE, ce.email COLLATE NOCASE`, userID)
	if err != nil {
		return nil, fmt.Errorf("query export contacts: %w", err)
	}
	defer rows.Close()

	contacts := profiles
	loc := timezoneLocationFromContext(ctx)
	for rows.Next() {
		c, err := scanContactRow(rows, loc)
		if err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func contactLegacyFilterSQL(userID string, filters models.ContactFilters) (string, []any) {
	query := strings.TrimSpace(filters.Query)
	where := `c.user_id = ? AND c.is_deleted = 0
		AND NOT EXISTS (
			SELECT 1
			FROM contact_identities ci
			JOIN contact_profiles cp ON cp.id = ci.profile_id AND cp.user_id = ci.user_id
			WHERE ci.user_id = c.user_id AND ci.kind = 'email' AND ci.normalized_value = ce.normalized_email AND cp.is_deleted = 0
		)`
	args := []any{userID}
	if query != "" {
		where += ` AND (c.display_name LIKE ? OR ce.email LIKE ? OR ce.normalized_email LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, strings.ToLower(like))
	}
	switch filters.Source {
	case "manual":
		where += ` AND c.is_manual = 1`
	case "observed":
		where += ` AND c.is_manual = 0`
	case "synced":
		where += ` AND c.is_manual = 0 AND c.source LIKE 'synced:%'`
	default:
		if strings.HasPrefix(filters.Source, "synced:") {
			where += ` AND c.source = ?`
			args = append(args, filters.Source)
		}
	}
	switch filters.Activity {
	case "seen":
		where += ` AND ce.message_count > 0`
	case "none":
		where += ` AND ce.message_count = 0`
	}
	saveTarget := strings.TrimSpace(filters.SaveTarget)
	if saveTarget == "local" {
		where += ` AND (NOT EXISTS (SELECT 1 FROM contact_save_targets cst WHERE cst.contact_id = c.id AND cst.user_id = c.user_id) OR EXISTS (SELECT 1 FROM contact_save_targets cst WHERE cst.contact_id = c.id AND cst.user_id = c.user_id AND cst.target = 'local'))`
	} else if saveTarget != "" {
		where += ` AND EXISTS (SELECT 1 FROM contact_save_targets cst WHERE cst.contact_id = c.id AND cst.user_id = c.user_id AND cst.target = ?)`
		args = append(args, saveTarget)
	}
	return where, args
}

func (db *DB) listProfileContacts(ctx context.Context, userID string, filters models.ContactFilters) ([]models.Contact, error) {
	if !profileContactsIncluded(filters) {
		return nil, nil
	}
	where, args := contactProfileFilterSQL(userID, filters)
	rows, err := db.Read().QueryContext(ctx, `
		SELECT p.id, p.display_name, COALESCE(email.value, p.primary_email), p.is_deleted,
		       CASE
		         WHEN EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual') THEN 'manual'
		         WHEN EXISTS (SELECT 1 FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0) THEN 'observed'
		         WHEN EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source LIKE 'synced:%') THEN 'synced'
		         ELSE 'manual'
		       END AS source,
		       CASE
		         WHEN EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual') THEN 1
		         ELSE 0
		       END AS is_manual,
		       COALESCE((SELECT SUM(co.message_count) FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0), 0) AS message_count,
		       (SELECT MAX(co.last_seen_at) FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0) AS last_seen_at,
		       p.created_at, p.updated_at
		FROM contact_profiles p
		LEFT JOIN contact_fields email ON email.profile_id = p.id AND email.user_id = p.user_id AND email.kind = 'email' AND email.is_primary = 1
		WHERE `+where+`
		ORDER BY p.updated_at DESC, p.display_name COLLATE NOCASE
		LIMIT 1000`, args...)
	if err != nil {
		return nil, fmt.Errorf("query contact profiles: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	loc := timezoneLocationFromContext(ctx)
	for rows.Next() {
		contact, err := scanProfileContactRow(rows, loc)
		if err != nil {
			return nil, err
		}
		contact.SaveTargets, _ = db.GetContactSaveTargets(ctx, userID, contact.ID)
		db.hydrateContactAvatar(ctx, &contact)
		_ = db.hydrateContactSyncState(ctx, userID, &contact)
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

func (db *DB) countProfileContacts(ctx context.Context, userID string, filters models.ContactFilters) (int, error) {
	if !profileContactsIncluded(filters) {
		return 0, nil
	}
	where, args := contactProfileFilterSQL(userID, filters)
	var count int
	err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT p.id)
		FROM contact_profiles p
		LEFT JOIN contact_fields email ON email.profile_id = p.id AND email.user_id = p.user_id AND email.kind = 'email' AND email.is_primary = 1
		WHERE `+where, args...).Scan(&count)
	return count, err
}

func profileContactsIncluded(filters models.ContactFilters) bool {
	switch {
	case filters.Source == "observed":
		return true
	case filters.Source == "synced" || strings.HasPrefix(filters.Source, "synced:"):
		return true
	default:
		return true
	}
}

func contactProfileFilterSQL(userID string, filters models.ContactFilters) (string, []any) {
	query := strings.TrimSpace(filters.Query)
	where := `p.user_id = ? AND p.is_deleted = 0`
	args := []any{userID}
	if query != "" {
		where += ` AND (p.display_name LIKE ? OR p.primary_email LIKE ? OR email.value LIKE ? OR email.normalized_value LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like, strings.ToLower(like))
	}
	switch filters.Source {
	case "manual":
		where += ` AND EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual')`
	case "observed":
		where += ` AND EXISTS (SELECT 1 FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0)
			AND NOT EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual')`
	case "synced":
		where += ` AND EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source LIKE 'synced:%')
			AND NOT EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual')`
	default:
		if strings.HasPrefix(filters.Source, "synced:") {
			where += ` AND EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = ?)
				AND NOT EXISTS (SELECT 1 FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual')`
			args = append(args, filters.Source)
		}
	}
	if filters.Activity == "none" {
		where += ` AND COALESCE((SELECT SUM(co.message_count) FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0), 0) = 0`
	} else if filters.Activity == "seen" {
		where += ` AND COALESCE((SELECT SUM(co.message_count) FROM contact_observations co WHERE co.user_id = p.user_id AND co.profile_id = p.id AND co.is_suppressed = 0), 0) > 0`
	}
	saveTarget := strings.TrimSpace(filters.SaveTarget)
	if saveTarget == "local" {
		where += ` AND EXISTS (SELECT 1 FROM contact_cards cc WHERE cc.user_id = p.user_id AND cc.profile_id = p.id AND cc.kind = 'local' AND cc.is_deleted = 0)`
	} else if accountID, ok := strings.CutPrefix(saveTarget, "account:"); ok && accountID != "" {
		where += ` AND EXISTS (SELECT 1 FROM contact_cards cc WHERE cc.user_id = p.user_id AND cc.profile_id = p.id AND cc.account_id = ? AND cc.is_deleted = 0)`
		args = append(args, accountID)
	} else if bookID, ok := strings.CutPrefix(saveTarget, "book:"); ok && bookID != "" {
		where += ` AND EXISTS (SELECT 1 FROM contact_cards cc WHERE cc.user_id = p.user_id AND cc.profile_id = p.id AND cc.address_book_id = ? AND cc.is_deleted = 0)`
		args = append(args, bookID)
	}
	return where, args
}

func scanProfileContactRow(scanner interface{ Scan(dest ...any) error }, loc *time.Location) (models.Contact, error) {
	var c models.Contact
	var isDeleted, isManual int
	var messageCount int
	var source string
	var lastSeen, createdAt, updatedAt sql.NullString
	if err := scanner.Scan(&c.ID, &c.Name, &c.Email, &isDeleted, &source, &isManual, &messageCount, &lastSeen, &createdAt, &updatedAt); err != nil {
		return c, err
	}
	c.Source = source
	c.IsManual = isManual == 1
	c.IsDeleted = isDeleted == 1
	c.MessageCount = messageCount
	c.Initials = initials(contactDisplayName(c.Name, c.Email))
	c.AvatarHash = avatarresolver.GravatarHash(c.Email)
	if lastSeen.Valid {
		c.LastSeenAt = formatContactTime(lastSeen.String, loc)
	}
	if createdAt.Valid {
		c.CreatedAt = formatContactTime(createdAt.String, loc)
	}
	if updatedAt.Valid {
		c.UpdatedAt = formatContactTime(updatedAt.String, loc)
	}
	return c, nil
}

func sortContactsForList(contacts []models.Contact) {
	sort.SliceStable(contacts, func(i, j int) bool {
		left := contacts[i].UpdatedAt
		right := contacts[j].UpdatedAt
		if left != "" && right != "" && left != right {
			return left > right
		}
		return strings.ToLower(contacts[i].Name) < strings.ToLower(contacts[j].Name)
	})
}

func (db *DB) SearchContacts(ctx context.Context, userID, query string, limit int) ([]models.Contact, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	profileContacts, err := db.listProfileContacts(ctx, userID, models.ContactFilters{Query: query})
	if err != nil {
		return nil, err
	}
	like := "%" + query + "%"
	rows, err := db.Read().QueryContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id
		WHERE `+contactLegacySearchSQL()+`
		  AND (c.display_name LIKE ? OR ce.email LIKE ? OR ce.normalized_email LIKE ?)
		ORDER BY CASE WHEN ce.normalized_email = ? THEN 0 WHEN ce.normalized_email LIKE ? THEN 1 ELSE 2 END,
		         COALESCE(ce.last_seen_at, c.updated_at) DESC,
		         c.display_name COLLATE NOCASE
		LIMIT ?`, userID, like, like, strings.ToLower(like), normalizeContactEmail(query), strings.ToLower(query)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	defer rows.Close()

	contacts := profileContacts
	loc := timezoneLocationFromContext(ctx)
	for rows.Next() {
		c, err := scanContactRow(rows, loc)
		if err != nil {
			return nil, err
		}
		db.hydrateContactAvatar(ctx, &c)
		contacts = append(contacts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortContactsForList(contacts)
	if len(contacts) > limit {
		contacts = contacts[:limit]
	}
	return contacts, nil
}

func (db *DB) GetContact(ctx context.Context, userID, contactID string) (*models.Contact, error) {
	contact, _, err := db.GetContactWithProfile(ctx, userID, contactID)
	return contact, err
}

func (db *DB) GetContactWithProfile(ctx context.Context, userID, contactID string) (*models.Contact, *models.ContactProfile, error) {
	if contactID == "" {
		return nil, nil, nil
	}
	if profile, err := db.GetContactProfile(ctx, userID, contactID); err != nil {
		return nil, nil, err
	} else if profile != nil && !profile.IsDeleted {
		contact, err := db.contactFromProfile(ctx, userID, *profile)
		if err != nil {
			return nil, nil, err
		}
		db.hydrateContactAvatar(ctx, &contact)
		contact.SaveTargets, _ = db.GetContactSaveTargets(ctx, userID, contactID)
		_ = db.hydrateContactSyncState(ctx, userID, &contact)
		return &contact, profile, nil
	}
	row := db.Read().QueryRowContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE c.user_id = ? AND c.id = ? AND c.is_deleted = 0`, userID, contactID)
	c, err := scanContactRow(row, timezoneLocationFromContext(ctx))
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	db.hydrateContactAvatar(ctx, &c)
	c.SaveTargets, _ = db.GetContactSaveTargets(ctx, userID, contactID)
	_ = db.hydrateContactAddressBooks(ctx, userID, &c)
	_ = db.hydrateContactSyncState(ctx, userID, &c)
	return &c, nil, nil
}

func contactLegacySearchSQL() string {
	return `c.user_id = ? AND c.is_deleted = 0
		AND NOT EXISTS (
			SELECT 1
			FROM contact_identities ci
			JOIN contact_profiles cp ON cp.id = ci.profile_id AND cp.user_id = ci.user_id
			WHERE ci.user_id = c.user_id AND ci.kind = 'email' AND ci.normalized_value = ce.normalized_email AND cp.is_deleted = 0
		)`
}

func (db *DB) contactFromProfile(ctx context.Context, userID string, profile models.ContactProfile) (models.Contact, error) {
	contact := models.Contact{
		ID:        profile.ID,
		Name:      profile.DisplayName,
		Email:     profile.PrimaryEmail,
		Source:    "manual",
		IsDeleted: profile.IsDeleted,
	}
	if contact.Email == "" {
		contact.Email = bestContactProfileFieldValue(profile.Fields, "email")
	}
	emailField := bestContactProfileField(profile.Fields, "email")
	if contact.Email == "" {
		contact.Email = strings.TrimSpace(emailField.Value)
	}
	contact.EmailLabel = contactStoredFieldLabel(emailField.Label, "primary")
	phoneField := bestContactProfileField(profile.Fields, "phone")
	contact.Phone = strings.TrimSpace(phoneField.Value)
	contact.PhoneLabel = contactStoredFieldLabel(phoneField.Label, "primary")
	contact.Organization = bestContactProfileFieldValue(profile.Fields, "organization")
	contact.Title = bestContactProfileFieldValue(profile.Fields, "title")
	contact.Notes = bestContactProfileFieldValue(profile.Fields, "notes")
	if contact.Notes == "" {
		contact.Notes = bestContactProfileFieldValue(profile.Fields, "note")
	}
	for _, field := range profile.Fields {
		switch field.Kind {
		case "email":
			if !sameContactValue(field.Value, contact.Email) {
				before := len(contact.AdditionalEmails)
				contact.AdditionalEmails = appendContactValue(contact.AdditionalEmails, field.Value)
				if len(contact.AdditionalEmails) > before {
					contact.AdditionalEmailLabels = append(contact.AdditionalEmailLabels, contactStoredFieldLabel(field.Label, "alternate"))
				}
			}
		case "phone":
			if !sameContactValue(field.Value, contact.Phone) {
				before := len(contact.AdditionalPhones)
				contact.AdditionalPhones = appendContactValue(contact.AdditionalPhones, field.Value)
				if len(contact.AdditionalPhones) > before {
					contact.AdditionalPhoneLabels = append(contact.AdditionalPhoneLabels, contactStoredFieldLabel(field.Label, "alternate"))
				}
			}
		}
	}
	var manualCount, syncedCount, messageCount int
	var lastSeen sql.NullString
	if err := db.Read().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM contact_fields cf WHERE cf.user_id = ? AND cf.profile_id = ? AND cf.source = 'manual'),
			(SELECT COUNT(*) FROM contact_fields cf WHERE cf.user_id = ? AND cf.profile_id = ? AND cf.source LIKE 'synced:%'),
			COALESCE((SELECT SUM(co.message_count) FROM contact_observations co WHERE co.user_id = ? AND co.profile_id = ? AND co.is_suppressed = 0), 0),
			(SELECT MAX(co.last_seen_at) FROM contact_observations co WHERE co.user_id = ? AND co.profile_id = ? AND co.is_suppressed = 0)`,
		userID, profile.ID, userID, profile.ID, userID, profile.ID, userID, profile.ID).Scan(&manualCount, &syncedCount, &messageCount, &lastSeen); err != nil {
		return contact, err
	}
	contact.IsManual = manualCount > 0
	if !contact.IsManual && messageCount > 0 {
		contact.Source = "observed"
	} else if !contact.IsManual && syncedCount > 0 {
		contact.Source = "synced"
	}
	contact.MessageCount = messageCount
	if lastSeen.Valid {
		contact.LastSeenAt = formatContactTime(lastSeen.String, timezoneLocationFromContext(ctx))
	}
	contact.Initials = initials(contactDisplayName(contact.Name, contact.Email))
	contact.AvatarHash = avatarresolver.GravatarHash(contact.Email)
	return contact, nil
}

func (db *DB) hydrateContactSyncState(ctx context.Context, userID string, contact *models.Contact) error {
	if contact == nil || strings.TrimSpace(contact.ID) == "" {
		return nil
	}
	op, err := db.LatestContactSyncOperationForContact(ctx, userID, contact.ID)
	if err != nil || op == nil {
		return err
	}
	contact.SyncStatus = op.Status
	contact.SyncError = op.LastError
	if !op.UpdatedAt.IsZero() {
		contact.SyncUpdatedAt = op.UpdatedAt.In(timezoneLocationFromContext(ctx)).Format("Jan 2, 2006 15:04")
	}
	return nil
}

func (db *DB) hydrateContactAddressBooks(ctx context.Context, userID string, contact *models.Contact) error {
	if contact == nil || strings.TrimSpace(contact.ID) == "" {
		return nil
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT DISTINCT ab.id, ab.account_id, COALESCE(NULLIF(a.display_name, ''), a.email_address), ab.name, ab.url, ab.is_default
		FROM contact_sources cs
		JOIN account_contact_address_books ab ON ab.account_id = cs.account_id
		JOIN accounts a ON a.id = ab.account_id
		WHERE cs.user_id = ?
		  AND cs.contact_id = ?
		  AND cs.provider = 'carddav'
		  AND (cs.address_book_id = ab.id OR (cs.address_book_id = '' AND cs.remote_id LIKE ab.url || '%'))
		ORDER BY a.email_address COLLATE NOCASE, ab.is_default DESC, ab.name COLLATE NOCASE, ab.url`, userID, contact.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	contact.SourceBooks = nil
	for rows.Next() {
		var book models.ContactAddressBook
		var isDefault int
		if err := rows.Scan(&book.ID, &book.AccountID, &book.AccountName, &book.Name, &book.URL, &isDefault); err != nil {
			return err
		}
		book.Selected = true
		book.Default = isDefault == 1
		contact.SourceBooks = append(contact.SourceBooks, book)
	}
	return rows.Err()
}

func (db *DB) ListContactAddressBooks(ctx context.Context, userID string) ([]models.ContactAddressBook, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT ab.id, ab.account_id, COALESCE(NULLIF(a.display_name, ''), a.email_address), ab.name, ab.url, ab.is_default, ab.last_sync_token
		FROM account_contact_address_books ab
		JOIN accounts a ON a.id = ab.account_id
		JOIN account_contact_sync_configs acc ON acc.account_id = ab.account_id AND acc.user_id = ab.user_id
		WHERE ab.user_id = ? AND acc.enabled = 1 AND COALESCE(a.is_deleting, 0) = 0
		ORDER BY a.email_address COLLATE NOCASE, ab.is_default DESC, ab.name COLLATE NOCASE, ab.url`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var books []models.ContactAddressBook
	for rows.Next() {
		var book models.ContactAddressBook
		var isDefault int
		if err := rows.Scan(&book.ID, &book.AccountID, &book.AccountName, &book.Name, &book.URL, &isDefault, &book.LastSyncToken); err != nil {
			return nil, err
		}
		book.Selected = true
		book.Default = isDefault == 1
		books = append(books, book)
	}
	return books, rows.Err()
}

func (db *DB) GetContactAddressBook(ctx context.Context, userID, bookID string) (models.ContactAddressBook, error) {
	var book models.ContactAddressBook
	var isDefault int
	err := db.Read().QueryRowContext(ctx, `
		SELECT ab.id, ab.account_id, COALESCE(NULLIF(a.display_name, ''), a.email_address), ab.name, ab.url, ab.is_default, ab.last_sync_token
		FROM account_contact_address_books ab
		JOIN accounts a ON a.id = ab.account_id
		JOIN account_contact_sync_configs acc ON acc.account_id = ab.account_id AND acc.user_id = ab.user_id
		WHERE ab.user_id = ? AND ab.id = ? AND acc.enabled = 1 AND COALESCE(a.is_deleting, 0) = 0`, userID, bookID).Scan(&book.ID, &book.AccountID, &book.AccountName, &book.Name, &book.URL, &isDefault, &book.LastSyncToken)
	if err != nil {
		return book, err
	}
	book.Selected = true
	book.Default = isDefault == 1
	return book, nil
}

func (db *DB) RecentContactEmails(ctx context.Context, userID, email string, limit int) ([]models.Email, error) {
	normalized := normalizeContactEmail(email)
	if userID == "" || normalized == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	rows, err := db.Read().QueryContext(ctx, `
		WITH matches AS (
			SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			       m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
			       mfs.folder_id, mfs.is_read, mfs.is_starred, m.thread_id,
			       ROW_NUMBER() OVER (
			         PARTITION BY m.id
			         ORDER BY CASE f.role WHEN 'inbox' THEN 0 WHEN 'sent' THEN 1 WHEN 'archive' THEN 2 ELSE 3 END, f.sort_order, f.name
			       ) AS folder_rank
			FROM messages m
			JOIN accounts a ON m.account_id = a.id
			JOIN message_folder_state mfs ON m.id = mfs.message_id
			JOIN folders f ON mfs.folder_id = f.id
			WHERE a.user_id = ?
			  AND mfs.is_deleted = 0
			  AND (
			    lower(trim(m.from_email)) = ?
			    OR EXISTS (
			      SELECT 1 FROM message_recipients mr
			      WHERE mr.message_id = m.id AND lower(trim(mr.email)) = ?
			    )
			  )
		)
		SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
		       has_attachments AS thread_has_attachments, folder_id, is_read, is_starred, thread_id, 1 AS thread_count
		FROM matches
		WHERE folder_rank = 1
		ORDER BY date_received DESC, id DESC
		LIMIT ?`, userID, normalized, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("recent contact emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

func (db *DB) GetContactSaveTargets(ctx context.Context, userID, contactID string) ([]string, error) {
	if targets, ok, err := db.getProfileContactSaveTargets(ctx, userID, contactID); err != nil {
		return nil, err
	} else if ok {
		return targets, nil
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT target
		FROM contact_save_targets
		WHERE user_id = ? AND contact_id = ?
		ORDER BY CASE WHEN target = 'local' THEN 0 ELSE 1 END, target`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		if target != "" {
			targets = append(targets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		targets = []string{"local"}
	}
	return targets, nil
}

func (db *DB) getProfileContactSaveTargets(ctx context.Context, userID, profileID string) ([]string, bool, error) {
	var exists int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, profileID).Scan(&exists); err != nil {
		return nil, false, err
	}
	if exists == 0 {
		return nil, false, nil
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT kind, account_id, address_book_id
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND is_deleted = 0
		ORDER BY CASE kind WHEN 'local' THEN 0 ELSE 1 END, account_id, address_book_id`, userID, profileID)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	var targets []string
	for rows.Next() {
		var kind, accountID, bookID string
		if err := rows.Scan(&kind, &accountID, &bookID); err != nil {
			return nil, true, err
		}
		target := ""
		switch {
		case kind == "local":
			target = "local"
		case strings.TrimSpace(bookID) != "":
			target = "book:" + strings.TrimSpace(bookID)
		case strings.TrimSpace(accountID) != "":
			target = "account:" + strings.TrimSpace(accountID)
		}
		if target != "" && !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, true, err
	}
	if len(targets) == 0 {
		targets = []string{"local"}
	}
	return targets, true, nil
}

func (db *DB) AddContactSaveTarget(ctx context.Context, userID, contactID, target string) error {
	target = strings.TrimSpace(target)
	if userID == "" || contactID == "" || target == "" {
		return nil
	}
	if profile, err := db.GetContactProfile(ctx, userID, contactID); err != nil {
		return err
	} else if profile != nil {
		card := models.ContactCard{UserID: userID, ProfileID: contactID}
		switch {
		case target == "local":
			card.Kind = "local"
		case strings.HasPrefix(target, "account:"):
			card.Kind = "target"
			card.AccountID = strings.TrimSpace(strings.TrimPrefix(target, "account:"))
		case strings.HasPrefix(target, "book:"):
			card.Kind = "target"
			card.AddressBookID = strings.TrimSpace(strings.TrimPrefix(target, "book:"))
		default:
			return nil
		}
		if card.Kind == "" || (card.Kind == "target" && card.AccountID == "" && card.AddressBookID == "") {
			return nil
		}
		return db.upsertContactCard(ctx, card)
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT OR IGNORE INTO contact_save_targets (contact_id, user_id, target)
		VALUES (?, ?, ?)`, contactID, userID, target)
	return err
}

func normalizeContactSaveTargets(targets []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(targets)+1)
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	if len(out) == 0 {
		out = append(out, "local")
	}
	return out
}

func (db *DB) replaceContactSaveTargetsTx(ctx context.Context, tx *sql.Tx, userID, contactID string, targets []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_save_targets WHERE user_id = ? AND contact_id = ?`, userID, contactID); err != nil {
		return err
	}
	for _, target := range normalizeContactSaveTargets(targets) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_save_targets (contact_id, user_id, target)
			VALUES (?, ?, ?)`, contactID, userID, target); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) UpsertContactSource(ctx context.Context, source ContactSource) error {
	if strings.TrimSpace(source.UserID) == "" || strings.TrimSpace(source.ContactID) == "" || strings.TrimSpace(source.Provider) == "" || strings.TrimSpace(source.AccountID) == "" {
		return nil
	}
	if profile, err := db.GetContactProfile(ctx, source.UserID, source.ContactID); err != nil {
		return err
	} else if profile != nil {
		return db.upsertContactCard(ctx, models.ContactCard{
			UserID:        strings.TrimSpace(source.UserID),
			ProfileID:     strings.TrimSpace(source.ContactID),
			Kind:          "provider",
			Provider:      strings.TrimSpace(source.Provider),
			AccountID:     strings.TrimSpace(source.AccountID),
			AddressBookID: strings.TrimSpace(source.AddressBookID),
			RemoteID:      strings.TrimSpace(source.RemoteID),
			Etag:          strings.TrimSpace(source.Etag),
		})
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO contact_sources (id, user_id, contact_id, provider, account_id, address_book_id, remote_id, etag, sync_token)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, contact_id, provider, account_id, remote_id) DO UPDATE SET
			address_book_id = excluded.address_book_id,
			remote_id = excluded.remote_id,
			etag = excluded.etag,
			sync_token = excluded.sync_token,
			updated_at = CURRENT_TIMESTAMP`,
		uuid.NewString(), strings.TrimSpace(source.UserID), strings.TrimSpace(source.ContactID), strings.TrimSpace(source.Provider), strings.TrimSpace(source.AccountID), strings.TrimSpace(source.AddressBookID), strings.TrimSpace(source.RemoteID), strings.TrimSpace(source.Etag), strings.TrimSpace(source.SyncToken))
	return err
}

func (db *DB) upsertContactCard(ctx context.Context, card models.ContactCard) error {
	card.UserID = strings.TrimSpace(card.UserID)
	card.ProfileID = strings.TrimSpace(card.ProfileID)
	card.Kind = strings.TrimSpace(card.Kind)
	card.Provider = strings.TrimSpace(card.Provider)
	card.AccountID = strings.TrimSpace(card.AccountID)
	card.AddressBookID = strings.TrimSpace(card.AddressBookID)
	card.RemoteID = strings.TrimSpace(card.RemoteID)
	card.Etag = strings.TrimSpace(card.Etag)
	if card.UserID == "" || card.ProfileID == "" || card.Kind == "" {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card.ID = strings.TrimSpace(card.ID)
	if card.ID == "" {
		query := `
			SELECT id
			FROM contact_cards
			WHERE user_id = ? AND profile_id = ? AND kind = ?
			  AND provider = ? AND account_id = ? AND address_book_id = ? AND remote_id = ?
			ORDER BY updated_at DESC
			LIMIT 1`
		err := tx.QueryRowContext(ctx, query, card.UserID, card.ProfileID, card.Kind, card.Provider, card.AccountID, card.AddressBookID, card.RemoteID).Scan(&card.ID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if card.ID == "" && card.Kind == "provider" && card.RemoteID != "" {
			err = tx.QueryRowContext(ctx, `
				SELECT id
				FROM contact_cards
				WHERE user_id = ? AND kind = 'provider' AND provider = ? AND account_id = ? AND remote_id = ?
				ORDER BY updated_at DESC
				LIMIT 1`, card.UserID, card.Provider, card.AccountID, card.RemoteID).Scan(&card.ID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
		}
		if card.ID == "" {
			card.ID = uuid.NewString()
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id, etag, raw_payload, raw_payload_type, sync_status, last_error, is_deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET
			profile_id = excluded.profile_id,
			kind = excluded.kind,
			provider = excluded.provider,
			account_id = excluded.account_id,
			address_book_id = excluded.address_book_id,
			remote_id = excluded.remote_id,
			etag = excluded.etag,
			raw_payload = excluded.raw_payload,
			raw_payload_type = excluded.raw_payload_type,
			sync_status = excluded.sync_status,
			last_error = excluded.last_error,
			is_deleted = 0,
			updated_at = CURRENT_TIMESTAMP`,
		card.ID, card.UserID, card.ProfileID, card.Kind, card.Provider, card.AccountID, card.AddressBookID, card.RemoteID, card.Etag, card.RawPayload, card.RawPayloadType, card.SyncStatus, card.LastError); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) GetContactSource(ctx context.Context, userID, contactID, provider, accountID string) (*ContactSource, error) {
	var source ContactSource
	err := db.Read().QueryRowContext(ctx, `
		SELECT profile_id, user_id, provider, account_id, address_book_id, remote_id, etag, '' AS sync_token
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND provider = ? AND account_id = ? AND kind = 'provider' AND is_deleted = 0
		ORDER BY updated_at DESC
		LIMIT 1`, userID, contactID, provider, accountID).Scan(&source.ContactID, &source.UserID, &source.Provider, &source.AccountID, &source.AddressBookID, &source.RemoteID, &source.Etag, &source.SyncToken)
	if err == nil {
		return &source, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	err = db.Read().QueryRowContext(ctx, `
		SELECT contact_id, user_id, provider, account_id, address_book_id, remote_id, etag, sync_token
		FROM contact_sources
		WHERE user_id = ? AND contact_id = ? AND provider = ? AND account_id = ?
		ORDER BY updated_at DESC
		LIMIT 1`, userID, contactID, provider, accountID).Scan(&source.ContactID, &source.UserID, &source.Provider, &source.AccountID, &source.AddressBookID, &source.RemoteID, &source.Etag, &source.SyncToken)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &source, nil
}

func (db *DB) GetContactSources(ctx context.Context, userID, contactID, provider string) ([]ContactSource, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT profile_id, user_id, provider, account_id, address_book_id, remote_id, etag, '' AS sync_token
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND provider = ? AND kind = 'provider' AND is_deleted = 0
		ORDER BY account_id, address_book_id, remote_id`, userID, contactID, provider)
	if err != nil {
		return nil, err
	}
	sources, err := scanContactSourceRows(rows)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	rows, err = db.Read().QueryContext(ctx, `
		SELECT contact_id, user_id, provider, account_id, address_book_id, remote_id, etag, sync_token
		FROM contact_sources
		WHERE user_id = ? AND contact_id = ? AND provider = ?
		ORDER BY account_id`, userID, contactID, provider)
	if err != nil {
		return nil, err
	}
	return scanContactSourceRows(rows)
}

func (db *DB) GetContactSourceByRemoteID(ctx context.Context, userID, provider, accountID, remoteID string) (*ContactSource, error) {
	var source ContactSource
	err := db.Read().QueryRowContext(ctx, `
		SELECT profile_id, user_id, provider, account_id, address_book_id, remote_id, etag, '' AS sync_token
		FROM contact_cards
		WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ? AND kind = 'provider' AND is_deleted = 0`, userID, provider, accountID, remoteID).Scan(&source.ContactID, &source.UserID, &source.Provider, &source.AccountID, &source.AddressBookID, &source.RemoteID, &source.Etag, &source.SyncToken)
	if err == nil {
		return &source, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	err = db.Read().QueryRowContext(ctx, `
		SELECT contact_id, user_id, provider, account_id, address_book_id, remote_id, etag, sync_token
		FROM contact_sources
		WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ?`, userID, provider, accountID, remoteID).Scan(&source.ContactID, &source.UserID, &source.Provider, &source.AccountID, &source.AddressBookID, &source.RemoteID, &source.Etag, &source.SyncToken)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &source, nil
}

func (db *DB) ListContactSourcesForAccount(ctx context.Context, userID, provider, accountID string) ([]ContactSource, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT profile_id, user_id, provider, account_id, address_book_id, remote_id, etag, '' AS sync_token
		FROM contact_cards
		WHERE user_id = ? AND provider = ? AND account_id = ? AND kind = 'provider' AND is_deleted = 0
		ORDER BY remote_id`, userID, provider, accountID)
	if err != nil {
		return nil, err
	}
	sources, err := scanContactSourceRows(rows)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	rows, err = db.Read().QueryContext(ctx, `
		SELECT contact_id, user_id, provider, account_id, address_book_id, remote_id, etag, sync_token
		FROM contact_sources
		WHERE user_id = ? AND provider = ? AND account_id = ?
		ORDER BY remote_id`, userID, provider, accountID)
	if err != nil {
		return nil, err
	}
	return scanContactSourceRows(rows)
}

func (db *DB) ListContactSourcesForEmail(ctx context.Context, userID, provider, accountID, email string) ([]ContactSource, error) {
	normalized := normalizeContactEmail(email)
	if userID == "" || provider == "" || accountID == "" || normalized == "" {
		return nil, nil
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT cc.profile_id, cc.user_id, cc.provider, cc.account_id, cc.address_book_id, cc.remote_id, cc.etag, '' AS sync_token
		FROM contact_cards cc
		JOIN contact_fields cf ON cf.profile_id = cc.profile_id AND cf.user_id = cc.user_id
		JOIN contact_profiles cp ON cp.id = cc.profile_id AND cp.user_id = cc.user_id
		WHERE cc.user_id = ? AND cc.provider = ? AND cc.account_id = ? AND cf.kind = 'email' AND cf.normalized_value = ? AND cc.kind = 'provider' AND cc.is_deleted = 0 AND cp.is_deleted = 0
		ORDER BY cc.remote_id`, userID, provider, accountID, normalized)
	if err != nil {
		return nil, err
	}
	sources, err := scanContactSourceRows(rows)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	rows, err = db.Read().QueryContext(ctx, `
		SELECT cs.contact_id, cs.user_id, cs.provider, cs.account_id, cs.address_book_id, cs.remote_id, cs.etag, cs.sync_token
		FROM contact_sources cs
		JOIN contact_emails ce ON ce.contact_id = cs.contact_id AND ce.user_id = cs.user_id
		WHERE cs.user_id = ? AND cs.provider = ? AND cs.account_id = ? AND ce.normalized_email = ?
		ORDER BY cs.remote_id`, userID, provider, accountID, normalized)
	if err != nil {
		return nil, err
	}
	return scanContactSourceRows(rows)
}

func scanContactSourceRows(rows *sql.Rows) ([]ContactSource, error) {
	defer rows.Close()
	var sources []ContactSource
	for rows.Next() {
		var source ContactSource
		if err := rows.Scan(&source.ContactID, &source.UserID, &source.Provider, &source.AccountID, &source.AddressBookID, &source.RemoteID, &source.Etag, &source.SyncToken); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (db *DB) EnqueueContactSyncOperation(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) (string, error) {
	if userID == "" || strings.TrimSpace(contact.ID) == "" || normalizeContactEmail(contact.Email) == "" {
		return "", nil
	}
	payload, err := json.Marshal(ContactSyncOperationPayload{Contact: contact, Previous: previous})
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = db.Write().ExecContext(ctx, `
		INSERT INTO contact_sync_operations (id, user_id, contact_id, email, payload_json, status, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP)`, id, userID, contact.ID, strings.TrimSpace(contact.Email), string(payload))
	if err != nil {
		return "", err
	}
	return id, nil
}

func (db *DB) ClaimContactSyncOperations(ctx context.Context, limit int, lockTimeout time.Duration) ([]ContactSyncOperation, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	if lockTimeout <= 0 {
		lockTimeout = 5 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-lockTimeout).Format("2006-01-02 15:04:05")
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM contact_sync_operations
		WHERE (status = 'pending' OR (status = 'running' AND locked_at <= ?))
		  AND next_attempt_at <= CURRENT_TIMESTAMP
		ORDER BY created_at
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	ops := make([]ContactSyncOperation, 0, len(ids))
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `
			UPDATE contact_sync_operations
			SET status = 'running', locked_at = CURRENT_TIMESTAMP, attempt_count = attempt_count + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND (status = 'pending' OR (status = 'running' AND locked_at <= ?))`, id, cutoff)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		op, err := scanContactSyncOperationTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, tx.Commit()
}

func scanContactSyncOperationTx(ctx context.Context, tx *sql.Tx, id string) (ContactSyncOperation, error) {
	var op ContactSyncOperation
	var payloadRaw string
	var lockedAt, nextAttempt, createdAt, updatedAt sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, contact_id, email, payload_json, status, attempt_count, last_error,
		       locked_at, next_attempt_at, created_at, updated_at
		FROM contact_sync_operations
		WHERE id = ?`, id).Scan(&op.ID, &op.UserID, &op.ContactID, &op.Email, &payloadRaw, &op.Status, &op.AttemptCount, &op.LastError, &lockedAt, &nextAttempt, &createdAt, &updatedAt)
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal([]byte(payloadRaw), &op.Payload); err != nil {
		return op, err
	}
	op.LockedAt = nullableSQLiteTime(lockedAt)
	op.NextAttempt = nullableSQLiteTime(nextAttempt)
	op.CreatedAt = nullableSQLiteTime(createdAt)
	op.UpdatedAt = nullableSQLiteTime(updatedAt)
	return op, nil
}

func nullableSQLiteTime(raw sql.NullString) time.Time {
	if !raw.Valid {
		return time.Time{}
	}
	if t, ok := parseSQLiteDateTime(raw.String); ok {
		return t
	}
	return time.Time{}
}

func (db *DB) MarkContactSyncOperationSuccess(ctx context.Context, operationID string) error {
	if strings.TrimSpace(operationID) == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE contact_sync_operations
		SET status = 'done', locked_at = NULL, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, operationID)
	return err
}

func (db *DB) MarkContactSyncOperationError(ctx context.Context, operationID, message string, retry bool) error {
	if strings.TrimSpace(operationID) == "" {
		return nil
	}
	status := "error"
	nextAttemptExpr := "CURRENT_TIMESTAMP"
	if retry {
		status = "pending"
		nextAttemptExpr = "datetime('now', '+2 minutes')"
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE contact_sync_operations
		SET status = ?, locked_at = NULL, last_error = ?, next_attempt_at = `+nextAttemptExpr+`, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, strings.TrimSpace(message), operationID)
	return err
}

func (db *DB) LatestContactSyncOperationForContact(ctx context.Context, userID, contactID string) (*ContactSyncOperation, error) {
	var id string
	err := db.Read().QueryRowContext(ctx, `
		SELECT id
		FROM contact_sync_operations
		WHERE user_id = ? AND contact_id = ?
		ORDER BY created_at DESC, updated_at DESC
		LIMIT 1`, userID, contactID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	op, err := scanContactSyncOperationTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return &op, tx.Commit()
}

func (db *DB) DeleteContactSourceByRemoteID(ctx context.Context, userID, provider, accountID, remoteID string) error {
	source, err := db.GetContactSourceByRemoteID(ctx, userID, provider, accountID, remoteID)
	if err != nil || source == nil {
		return err
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_cards
		WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ? AND kind = 'provider'`, userID, provider, accountID, remoteID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_sources
		WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ?`, userID, provider, accountID, remoteID); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND provider = ? AND account_id = ? AND kind = 'provider' AND is_deleted = 0`, userID, source.ContactID, provider, accountID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM contact_sources
			WHERE user_id = ? AND contact_id = ? AND provider = ? AND account_id = ?`, userID, source.ContactID, provider, accountID).Scan(&remaining); err != nil {
			return err
		}
	}
	if remaining > 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND kind = 'target' AND account_id = ?`, userID, source.ContactID, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_save_targets
		WHERE user_id = ? AND contact_id = ? AND target = ?`, userID, source.ContactID, "account:"+accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DeleteContactSource(ctx context.Context, userID, contactID, provider, accountID string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND provider = ? AND account_id = ? AND kind = 'provider'`, userID, contactID, provider, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND kind = 'target' AND account_id = ?`, userID, contactID, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_sources
		WHERE user_id = ? AND contact_id = ? AND provider = ? AND account_id = ?`, userID, contactID, provider, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_save_targets
		WHERE user_id = ? AND contact_id = ? AND target = ?`, userID, contactID, "account:"+accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func scanContactRow(scanner interface{ Scan(dest ...any) error }, loc *time.Location) (models.Contact, error) {
	var c models.Contact
	var isManual, isDeleted int
	var lastSeen, createdAt, updatedAt sql.NullString
	if err := scanner.Scan(&c.ID, &c.Name, &c.Email, &c.Source, &isManual, &isDeleted, &c.MessageCount, &lastSeen, &createdAt, &updatedAt); err != nil {
		return c, err
	}
	c.IsManual = isManual == 1
	c.IsDeleted = isDeleted == 1
	c.Initials = initials(contactDisplayName(c.Name, c.Email))
	c.AvatarHash = avatarresolver.GravatarHash(c.Email)
	if lastSeen.Valid {
		c.LastSeenAt = formatContactTime(lastSeen.String, loc)
	}
	if createdAt.Valid {
		c.CreatedAt = formatContactTime(createdAt.String, loc)
	}
	if updatedAt.Valid {
		c.UpdatedAt = formatContactTime(updatedAt.String, loc)
	}
	return c, nil
}

func formatContactTime(raw string, loc *time.Location) string {
	if t, ok := parseSQLiteDateTime(raw); ok {
		if loc == nil {
			loc = time.Local
		}
		return t.In(loc).Format("Jan 2, 2006")
	}
	return raw
}

func (db *DB) SaveContact(ctx context.Context, userID string, contact models.Contact) (models.Contact, error) {
	email := strings.TrimSpace(contact.Email)
	normalized := normalizeContactEmail(email)
	if normalized == "" {
		return models.Contact{}, fmt.Errorf("email is required")
	}
	name := contactDisplayName(contact.Name, email)
	contactID := strings.TrimSpace(contact.ID)
	if contactID == "" {
		if existing, err := db.FindContactProfileByIdentity(ctx, userID, "email", normalized); err != nil {
			return models.Contact{}, err
		} else if existing != nil {
			contactID = existing.ID
		}
	}
	created := false
	var existingProfile *models.ContactProfile
	if contactID == "" {
		created = true
	} else {
		if existing, err := db.GetContactProfile(ctx, userID, contactID); err != nil {
			return models.Contact{}, err
		} else if existing == nil {
			created = true
		} else {
			existingProfile = existing
		}
	}

	profile := models.ContactProfile{
		ID:           contactID,
		UserID:       userID,
		DisplayName:  name,
		SortName:     name,
		PrimaryEmail: email,
		Notes:        strings.TrimSpace(contact.Notes),
		Cards:        contactCardsForSaveTargets(userID, contactID, contact.SaveTargets),
		Fields: []models.ContactField{{
			Kind:      "email",
			Label:     contactStoredFieldLabel(contact.EmailLabel, "primary"),
			Value:     email,
			IsPrimary: true,
			Source:    "manual",
		}},
	}
	if phone := strings.TrimSpace(contact.Phone); phone != "" {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "phone", Label: contactStoredFieldLabel(contact.PhoneLabel, "primary"), Value: phone, IsPrimary: true, Source: "manual"})
	}
	for i, email := range normalizedAdditionalContactValues(contact.AdditionalEmails, email) {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "email", Label: contactAdditionalFieldLabel(contact.AdditionalEmailLabels, i), Value: email, Source: "manual"})
	}
	for i, phone := range normalizedAdditionalContactValues(contact.AdditionalPhones, contact.Phone) {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "phone", Label: contactAdditionalFieldLabel(contact.AdditionalPhoneLabels, i), Value: phone, Source: "manual"})
	}
	if organization := strings.TrimSpace(contact.Organization); organization != "" {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "organization", Value: organization, IsPrimary: true, Source: "manual"})
	}
	if title := strings.TrimSpace(contact.Title); title != "" {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "title", Value: title, IsPrimary: true, Source: "manual"})
	}
	if notes := strings.TrimSpace(contact.Notes); notes != "" {
		profile.Fields = append(profile.Fields, models.ContactField{Kind: "notes", Value: notes, IsPrimary: true, Source: "manual"})
	}
	if existingProfile != nil {
		for _, field := range existingProfile.Fields {
			if field.Source == "manual" {
				continue
			}
			profile.Fields = append(profile.Fields, field)
		}
	}
	savedProfile, err := db.SaveContactProfile(ctx, userID, profile)
	if err != nil {
		return models.Contact{}, err
	}
	saved, err := db.GetContact(ctx, userID, savedProfile.ID)
	if err != nil || saved == nil {
		return models.Contact{}, err
	}
	if created {
		_ = db.LogContactActivity(ctx, userID, "manual_contact_added", email, "Manual contact added", 1)
	}
	return *saved, nil
}

func contactCardsForSaveTargets(userID, profileID string, targets []string) []models.ContactCard {
	targets = normalizeContactSaveTargets(targets)
	cards := make([]models.ContactCard, 0, len(targets))
	for _, target := range targets {
		switch {
		case target == "local":
			cards = append(cards, models.ContactCard{UserID: userID, ProfileID: profileID, Kind: "local"})
		case strings.HasPrefix(target, "account:"):
			accountID := strings.TrimSpace(strings.TrimPrefix(target, "account:"))
			if accountID != "" {
				cards = append(cards, models.ContactCard{UserID: userID, ProfileID: profileID, Kind: "target", AccountID: accountID})
			}
		case strings.HasPrefix(target, "book:"):
			bookID := strings.TrimSpace(strings.TrimPrefix(target, "book:"))
			if bookID != "" {
				cards = append(cards, models.ContactCard{UserID: userID, ProfileID: profileID, Kind: "target", AddressBookID: bookID})
			}
		}
	}
	if len(cards) == 0 {
		cards = append(cards, models.ContactCard{UserID: userID, ProfileID: profileID, Kind: "local"})
	}
	return cards
}

func (db *DB) UpsertSyncedContact(ctx context.Context, userID, accountID, name, email string) (string, bool, error) {
	return db.UpsertSyncedContactFromContact(ctx, userID, accountID, models.Contact{Name: name, Email: email})
}

func (db *DB) UpsertSyncedContactFromContact(ctx context.Context, userID, accountID string, contact models.Contact) (string, bool, error) {
	name := strings.TrimSpace(contact.Name)
	email := strings.TrimSpace(contact.Email)
	email = strings.TrimSpace(email)
	normalized := normalizeContactEmail(email)
	accountID = strings.TrimSpace(accountID)
	if userID == "" || accountID == "" || normalized == "" {
		return "", false, nil
	}
	display := contactDisplayName(name, email)
	source := "synced:" + accountID

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var contactID string
	var currentDisplay string
	manualCount := 0
	err = tx.QueryRowContext(ctx, `
		SELECT ci.profile_id, cp.display_name
		FROM contact_identities ci
		JOIN contact_profiles cp ON cp.id = ci.profile_id AND cp.user_id = ci.user_id
		WHERE ci.user_id = ? AND ci.kind = 'email' AND ci.normalized_value = ? AND cp.is_deleted = 0`, userID, normalized).Scan(&contactID, &currentDisplay)
	if err != nil && err != sql.ErrNoRows {
		return "", false, err
	}
	if contactID == "" {
		err = tx.QueryRowContext(ctx, `
			SELECT c.id, c.display_name
			FROM contact_emails ce
			JOIN contacts c ON ce.contact_id = c.id
			WHERE ce.user_id = ? AND ce.normalized_email = ?`, userID, normalized).Scan(&contactID, &currentDisplay)
		if err != nil && err != sql.ErrNoRows {
			return "", false, err
		}
	}

	created := false
	if contactID == "" {
		contactID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_profiles (id, user_id, display_name, sort_name, primary_email, is_deleted)
			VALUES (?, ?, ?, ?, ?, 0)`, contactID, userID, display, display, email); err != nil {
			return "", false, err
		}
		created = true
	} else {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id = ? AND profile_id = ? AND source = 'manual'`, userID, contactID).Scan(&manualCount); err != nil {
			return "", false, err
		}
		if manualCount > 0 || (strings.TrimSpace(currentDisplay) != "" && normalizeContactEmail(currentDisplay) != normalized) {
			display = currentDisplay
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_profiles (id, user_id, display_name, sort_name, primary_email, is_deleted)
			VALUES (?, ?, ?, ?, ?, 0)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				sort_name = excluded.sort_name,
				primary_email = CASE WHEN contact_profiles.primary_email = '' THEN excluded.primary_email ELSE contact_profiles.primary_email END,
				is_deleted = 0,
				updated_at = CURRENT_TIMESTAMP`, contactID, userID, display, display, email); err != nil {
			return "", false, err
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE contact_fields
		SET value = ?, normalized_value = ?, is_primary = ?, source = ?, confidence = 0.9, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND profile_id = ? AND kind = 'email' AND normalized_value = ? AND source = ?`,
		email, normalized, boolInt(manualCount == 0), source, userID, contactID, normalized, source)
	if err != nil {
		return "", false, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		var emailFieldCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id = ? AND profile_id = ? AND kind = 'email' AND normalized_value = ? AND source = ?`, userID, contactID, normalized, source).Scan(&emailFieldCount); err != nil {
			return "", false, err
		}
		if emailFieldCount == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO contact_fields (id, user_id, profile_id, kind, value, normalized_value, is_primary, ordinal, source, confidence)
				VALUES (?, ?, ?, 'email', ?, ?, ?, 1, ?, 0.9)`, uuid.NewString(), userID, contactID, email, normalized, boolInt(manualCount == 0), source); err != nil {
				return "", false, err
			}
		}
	}
	if err := replaceSyncedContactFieldsTx(ctx, tx, userID, contactID, source, contact); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_identities (user_id, profile_id, kind, normalized_value, confidence)
		VALUES (?, ?, 'email', ?, 0.9)
		ON CONFLICT(user_id, kind, normalized_value) DO UPDATE SET
			profile_id = excluded.profile_id,
			confidence = MAX(contact_identities.confidence, excluded.confidence),
			updated_at = CURRENT_TIMESTAMP`, userID, contactID, normalized); err != nil {
		return "", false, err
	}
	var targetCardID string
	err = tx.QueryRowContext(ctx, `
		SELECT id
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ? AND kind = 'target' AND account_id = ? AND address_book_id = '' AND remote_id = ''
		ORDER BY updated_at DESC
		LIMIT 1`, userID, contactID, accountID).Scan(&targetCardID)
	if err != nil && err != sql.ErrNoRows {
		return "", false, err
	}
	if targetCardID == "" {
		targetCardID = uuid.NewString()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id, is_deleted)
		VALUES (?, ?, ?, 'target', '', ?, '', '', 0)
		ON CONFLICT(id) DO UPDATE SET is_deleted = 0, updated_at = CURRENT_TIMESTAMP`,
		targetCardID, userID, contactID, accountID); err != nil {
		return "", false, err
	}

	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return contactID, created, nil
}

func replaceSyncedContactFieldsTx(ctx context.Context, tx *sql.Tx, userID, profileID, source string, contact models.Contact) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contact_fields
		WHERE user_id = ? AND profile_id = ? AND source = ? AND kind IN ('email', 'phone', 'organization', 'title', 'notes')`, userID, profileID, source); err != nil {
		return err
	}
	fields := []struct {
		kind  string
		label string
		value string
	}{
		{kind: "email", label: contactStoredFieldLabel(contact.EmailLabel, "primary"), value: contact.Email},
		{kind: "phone", label: contactStoredFieldLabel(contact.PhoneLabel, "primary"), value: contact.Phone},
		{kind: "organization", value: contact.Organization},
		{kind: "title", value: contact.Title},
		{kind: "notes", value: contact.Notes},
	}
	for i, email := range normalizedAdditionalContactValues(contact.AdditionalEmails, contact.Email) {
		fields = append(fields, struct {
			kind  string
			label string
			value string
		}{kind: "email", label: contactAdditionalFieldLabel(contact.AdditionalEmailLabels, i), value: email})
	}
	for i, phone := range normalizedAdditionalContactValues(contact.AdditionalPhones, contact.Phone) {
		fields = append(fields, struct {
			kind  string
			label string
			value string
		}{kind: "phone", label: contactAdditionalFieldLabel(contact.AdditionalPhoneLabels, i), value: phone})
	}
	for i, field := range fields {
		value := strings.TrimSpace(field.value)
		if value == "" {
			continue
		}
		normalized := normalizeContactFieldValue(field.kind, value)
		if normalized == "" {
			continue
		}
		var manualCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id = ? AND profile_id = ? AND kind = ? AND source = 'manual'`, userID, profileID, field.kind).Scan(&manualCount); err != nil {
			return err
		}
		isPrimary := manualCount == 0 && field.label == "primary"
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_fields (id, user_id, profile_id, kind, label, value, normalized_value, is_primary, ordinal, source, confidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0.9)`,
			uuid.NewString(), userID, profileID, field.kind, field.label, value, normalized, boolInt(isPrimary), i+2, source); err != nil {
			return err
		}
	}
	return nil
}

func normalizedAdditionalContactValues(values []string, primary string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	if primary = strings.TrimSpace(primary); primary != "" {
		seen[strings.ToLower(primary)] = true
	}
	for _, value := range values {
		out = appendContactValueWithSeen(out, value, seen)
	}
	return out
}

func appendContactValue(values []string, value string) []string {
	return appendContactValueWithSeen(values, value, contactValueSet(values))
}

func appendContactValueWithSeen(values []string, value string, seen map[string]bool) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	key := strings.ToLower(value)
	if seen[key] {
		return values
	}
	seen[key] = true
	return append(values, value)
}

func contactValueSet(values []string) map[string]bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[strings.ToLower(value)] = true
		}
	}
	return seen
}

func sameContactValue(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func bestContactProfileFieldValue(fields []models.ContactField, kind string) string {
	return strings.TrimSpace(bestContactProfileField(fields, kind).Value)
}

func bestContactProfileField(fields []models.ContactField, kind string) models.ContactField {
	var best models.ContactField
	bestScore := -1
	for _, field := range fields {
		if field.Kind != kind || strings.TrimSpace(field.Value) == "" {
			continue
		}
		score := 0
		if strings.HasPrefix(field.Source, "synced:") {
			score = 10
		}
		if field.IsPrimary {
			score += 20
		}
		if field.Source == "manual" {
			score += 40
		}
		if score > bestScore {
			best = field
			bestScore = score
		}
	}
	return best
}

func contactStoredFieldLabel(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = fallback
	}
	return value
}

func contactAdditionalFieldLabel(labels []string, index int) string {
	if index >= 0 && index < len(labels) {
		return contactStoredFieldLabel(labels[index], "alternate")
	}
	return "alternate"
}

func (db *DB) DeleteContact(ctx context.Context, userID, contactID string, preventRecreate bool) error {
	if contactID == "" {
		return nil
	}
	if profile, err := db.GetContactProfile(ctx, userID, contactID); err != nil {
		return err
	} else if profile != nil {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var manualCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id = ? AND profile_id = ? AND source = 'manual'`, userID, contactID).Scan(&manualCount); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles
			SET is_deleted = 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ?`, contactID, userID)
		if err != nil {
			return err
		}
		if manualCount == 0 && preventRecreate {
			if _, err := tx.ExecContext(ctx, `
				UPDATE contact_observations
				SET is_suppressed = 1, suppress_auto_create = 1, updated_at = CURRENT_TIMESTAMP
				WHERE user_id = ? AND profile_id = ?`, userID, contactID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err == nil {
			if affected, _ := res.RowsAffected(); affected > 0 {
				_ = db.LogContactActivity(ctx, userID, "contact_deleted", profile.PrimaryEmail, "Contact deleted", 1)
			}
		}
		return err
	}
	var email string
	_ = db.Read().QueryRowContext(ctx, `
		SELECT ce.email
		FROM contacts c
		LEFT JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE c.id = ? AND c.user_id = ?`, contactID, userID).Scan(&email)
	if preventRecreate {
		res, err := db.Write().ExecContext(ctx, `
			UPDATE contacts
			SET is_deleted = 1, suppress_auto_create = 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ?`, contactID, userID)
		if err == nil {
			if affected, _ := res.RowsAffected(); affected > 0 {
				_ = db.LogContactActivity(ctx, userID, "contact_deleted", email, "Contact deleted and suppressed", 1)
			}
		}
		return err
	}
	res, err := db.Write().ExecContext(ctx, `DELETE FROM contacts WHERE id = ? AND user_id = ?`, contactID, userID)
	if err == nil {
		if affected, _ := res.RowsAffected(); affected > 0 {
			_ = db.LogContactActivity(ctx, userID, "contact_deleted", email, "Contact deleted", 1)
		}
	}
	return err
}

func (db *DB) DeleteObservedContacts(ctx context.Context, userID string, preventRecreate bool) (int64, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var targetIDs []string
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT co.profile_id
		FROM contact_observations co
		WHERE co.user_id = ? AND co.is_suppressed = 0 AND co.profile_id != ''
		  AND NOT EXISTS (
			SELECT 1 FROM contact_fields cf
			WHERE cf.user_id = co.user_id AND cf.profile_id = co.profile_id AND cf.source = 'manual'
		  )`, userID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		targetIDs = append(targetIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, profileID := range targetIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET is_deleted = 1, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, userID, profileID); err != nil {
			return 0, err
		}
	}
	if preventRecreate {
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_observations
			SET is_suppressed = 1, suppress_auto_create = 1, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND profile_id IN (`+sqlPlaceholders(len(targetIDs))+`)`, append([]any{userID}, stringsToAny(targetIDs)...)...); err != nil && len(targetIDs) > 0 {
			return 0, err
		}
	} else if len(targetIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM contact_observations WHERE user_id = ? AND profile_id IN (`+sqlPlaceholders(len(targetIDs))+`)`, append([]any{userID}, stringsToAny(targetIDs)...)...); err != nil {
			return 0, err
		}
	}
	if len(targetIDs) > 0 {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		msg := "Discovered contacts deleted"
		eventType := "observed_contacts_deleted"
		if preventRecreate {
			msg = "Discovered contacts deleted and suppressed"
		}
		_ = db.LogContactActivity(ctx, userID, eventType, "", msg, len(targetIDs))
		return int64(len(targetIDs)), nil
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return 0, nil
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func stringsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func (db *DB) deleteObservedContactsLegacy(ctx context.Context, userID string, preventRecreate bool) (int64, error) {
	if preventRecreate {
		res, err := db.Write().ExecContext(ctx, `
			UPDATE contacts
			SET is_deleted = 1, suppress_auto_create = 1, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND is_manual = 0 AND is_deleted = 0`, userID)
		if err != nil {
			return 0, err
		}
		deleted, _ := res.RowsAffected()
		if deleted > 0 {
			_ = db.LogContactActivity(ctx, userID, "observed_contacts_deleted", "", "Discovered contacts deleted and suppressed", int(deleted))
		}
		return deleted, nil
	}
	res, err := db.Write().ExecContext(ctx, `DELETE FROM contacts WHERE user_id = ? AND is_manual = 0`, userID)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	if deleted > 0 {
		_ = db.LogContactActivity(ctx, userID, "observed_contacts_deleted", "", "Discovered contacts deleted", int(deleted))
	}
	return deleted, nil
}

func (db *DB) ListSuppressedContacts(ctx context.Context, userID string, limit int) ([]models.Contact, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT COALESCE(NULLIF(co.profile_id, ''), co.id), COALESCE(NULLIF(p.display_name, ''), NULLIF(co.observed_name, ''), co.email),
		       co.email, 'observed', 0, 1, co.message_count, co.last_seen_at, co.created_at, co.updated_at
		FROM contact_observations co
		LEFT JOIN contact_profiles p ON p.user_id = co.user_id AND p.id = co.profile_id
		WHERE co.user_id = ? AND co.is_suppressed = 1 AND co.suppress_auto_create = 1
		ORDER BY co.updated_at DESC, COALESCE(NULLIF(p.display_name, ''), co.observed_name, co.email) COLLATE NOCASE
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("query suppressed contacts: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	loc := timezoneLocationFromContext(ctx)
	for rows.Next() {
		c, err := scanContactRow(rows, loc)
		if err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (db *DB) CountSuppressedContacts(ctx context.Context, userID string) (int, error) {
	var count int
	err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM contact_observations
		WHERE user_id = ? AND is_suppressed = 1 AND suppress_auto_create = 1`, userID).Scan(&count)
	return count, err
}

func (db *DB) ClearSuppressedContacts(ctx context.Context, userID string) (int64, error) {
	res, err := db.Write().ExecContext(ctx, `
		DELETE FROM contact_observations
		WHERE user_id = ? AND is_suppressed = 1 AND suppress_auto_create = 1`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (db *DB) ClearSuppressedContact(ctx context.Context, userID, contactID string) error {
	if contactID == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `
		DELETE FROM contact_observations
		WHERE user_id = ? AND is_suppressed = 1 AND suppress_auto_create = 1 AND (profile_id = ? OR id = ?)`, userID, contactID, contactID)
	return err
}

func (db *DB) UpsertObservedContact(ctx context.Context, userID, name, email string, seenAt time.Time) error {
	settings := db.GetContactSettings(ctx, userID)
	return db.upsertObservedContact(ctx, userID, name, email, seenAt, 1, settings)
}

func (db *DB) upsertObservedContact(ctx context.Context, userID, name, email string, seenAt time.Time, count int, settings ContactSettings) error {
	email = strings.TrimSpace(email)
	normalized := normalizeContactEmail(email)
	if userID == "" || normalized == "" {
		return nil
	}
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	if count <= 0 {
		count = 1
	}
	display := contactDisplayName(name, email)

	var observationID, profileID string
	var isSuppressed, suppressAuto int
	err := db.Read().QueryRowContext(ctx, `
		SELECT id, profile_id, is_suppressed, suppress_auto_create
		FROM contact_observations
		WHERE user_id = ? AND normalized_email = ?`, userID, normalized).Scan(&observationID, &profileID, &isSuppressed, &suppressAuto)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	observationCreated := err == sql.ErrNoRows

	if isSuppressed == 1 {
		if settings.PreventRecreateDeleted && suppressAuto == 1 {
			return nil
		}
		if !settings.AutoCreateObserved {
			return nil
		}
	}

	if profileID == "" {
		profile, err := db.FindContactProfileByIdentity(ctx, userID, "email", normalized)
		if err != nil {
			return err
		}
		if profile != nil {
			profileID = profile.ID
		}
	}

	if profileID == "" {
		if !settings.AutoCreateObserved {
			return nil
		}
		profile, err := db.SaveContactProfile(ctx, userID, models.ContactProfile{
			DisplayName:  display,
			SortName:     display,
			PrimaryEmail: email,
			Cards:        []models.ContactCard{{Kind: "observed"}},
			Fields: []models.ContactField{{
				Kind:      "email",
				Label:     "observed",
				Value:     email,
				IsPrimary: true,
				Source:    "observed",
			}},
		})
		if err != nil {
			return err
		}
		profileID = profile.ID
	} else {
		if !settings.AutoCreateObserved && isSuppressed == 1 {
			return nil
		}
		if _, err := db.Write().ExecContext(ctx, `UPDATE contact_profiles SET is_deleted = 0, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, userID, profileID); err != nil {
			return err
		}
		var manualCount int
		var currentDisplay string
		_ = db.Read().QueryRowContext(ctx, `
			SELECT p.display_name, (SELECT COUNT(*) FROM contact_fields cf WHERE cf.user_id = p.user_id AND cf.profile_id = p.id AND cf.source = 'manual')
			FROM contact_profiles p
			WHERE p.user_id = ? AND p.id = ?`, userID, profileID).Scan(&currentDisplay, &manualCount)
		if manualCount == 0 && (strings.TrimSpace(currentDisplay) == "" || normalizeContactEmail(currentDisplay) == normalized) {
			if _, err := db.Write().ExecContext(ctx, `UPDATE contact_profiles SET display_name = ?, sort_name = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, display, display, userID, profileID); err != nil {
				return err
			}
		}
	}

	if observationID == "" {
		observationID = uuid.NewString()
	}
	_, err = db.Write().ExecContext(ctx, `
		INSERT INTO contact_observations (id, user_id, profile_id, email, normalized_email, observed_name, message_count, last_seen_at, is_suppressed, suppress_auto_create)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0)
		ON CONFLICT(user_id, normalized_email) DO UPDATE SET
			profile_id = excluded.profile_id,
			email = excluded.email,
			observed_name = excluded.observed_name,
			message_count = contact_observations.message_count + excluded.message_count,
			last_seen_at = MAX(COALESCE(contact_observations.last_seen_at, excluded.last_seen_at), excluded.last_seen_at),
			is_suppressed = 0,
			suppress_auto_create = 0,
			updated_at = CURRENT_TIMESTAMP`, observationID, userID, profileID, email, normalized, strings.TrimSpace(name), count, seenAt)
	if err != nil {
		return err
	}
	if observationCreated {
		_ = db.LogContactActivity(ctx, userID, "observed_contact_added", email, "Observed contact added", 1)
	}
	return nil
}

func (db *DB) BackfillObservedContacts(ctx context.Context, userID string) error {
	return db.BackfillObservedContactsWithProgress(ctx, userID, nil)
}

func (db *DB) BackfillObservedContactsWithProgress(ctx context.Context, userID string, progress func(processed int)) error {
	settings := db.GetContactSettings(ctx, userID)
	if !settings.AutoCreateObserved || (!settings.ObserveSenders && !settings.ObserveRecipients) {
		return nil
	}
	_ = db.LogContactActivity(ctx, userID, "backfill_started", "", "Observed contact backfill started", 0)
	parts := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if settings.ObserveSenders {
		parts = append(parts, `SELECT m.from_name AS name, m.from_email AS email, COALESCE(m.date_received, m.date_sent, m.created_at) AS seen_at
			FROM messages m
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND m.from_email != ''`)
		args = append(args, userID)
	}
	if settings.ObserveRecipients {
		parts = append(parts, `SELECT mr.name, mr.email, COALESCE(m.date_received, m.date_sent, m.created_at) AS seen_at
			FROM message_recipients mr
			JOIN messages m ON mr.message_id = m.id
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND mr.email != ''`)
		args = append(args, userID)
	}
	query := `
		WITH participants AS (` + strings.Join(parts, " UNION ALL ") + `), ranked AS (
			SELECT lower(trim(email)) AS normalized_email, email, name, seen_at,
			       ROW_NUMBER() OVER (PARTITION BY lower(trim(email)) ORDER BY seen_at DESC) AS rn,
			       COUNT(*) OVER (PARTITION BY lower(trim(email))) AS message_count,
			       MAX(seen_at) OVER (PARTITION BY lower(trim(email))) AS last_seen_at
			FROM participants
		)
		SELECT name, email, message_count, last_seen_at FROM ranked WHERE rn = 1`
	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	processed := 0
	for rows.Next() {
		var name, email string
		var count int
		var lastSeenRaw string
		if err := rows.Scan(&name, &email, &count, &lastSeenRaw); err != nil {
			return err
		}
		seenAt := time.Now().UTC()
		if t, ok := parseSQLiteDateTime(lastSeenRaw); ok {
			seenAt = t
		}
		if err := db.upsertObservedContact(ctx, userID, name, email, seenAt, count, settings); err != nil {
			return err
		}
		processed++
		if progress != nil {
			progress(processed)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = db.LogContactActivity(ctx, userID, "backfill_completed", "", "Observed contact backfill completed", processed)
	return nil
}

func (db *DB) CountObservedContactBackfillCandidates(ctx context.Context, userID string) (int, error) {
	settings := db.GetContactSettings(ctx, userID)
	if !settings.AutoCreateObserved || (!settings.ObserveSenders && !settings.ObserveRecipients) {
		return 0, nil
	}
	parts := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if settings.ObserveSenders {
		parts = append(parts, `SELECT lower(trim(m.from_email)) AS normalized_email
			FROM messages m
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND m.from_email != ''`)
		args = append(args, userID)
	}
	if settings.ObserveRecipients {
		parts = append(parts, `SELECT lower(trim(mr.email)) AS normalized_email
			FROM message_recipients mr
			JOIN messages m ON mr.message_id = m.id
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND mr.email != ''`)
		args = append(args, userID)
	}
	var total int
	err := db.Read().QueryRowContext(ctx, `SELECT COUNT(DISTINCT normalized_email) FROM (`+strings.Join(parts, " UNION ALL ")+`)`, args...).Scan(&total)
	return total, err
}

func (db *DB) UpsertObservedContactsForMessage(ctx context.Context, accountID, fromName, fromEmail string, to, cc, bcc []Recipient, seenAt time.Time) {
	userID, err := db.GetAccountUserID(ctx, accountID)
	if err != nil || userID == "" {
		return
	}
	settings := db.GetContactSettings(ctx, userID)
	if settings.ObserveSenders {
		_ = db.upsertObservedContact(ctx, userID, fromName, fromEmail, seenAt, 1, settings)
	}
	if settings.ObserveRecipients {
		for _, r := range to {
			_ = db.upsertObservedContact(ctx, userID, r.Name, r.Email, seenAt, 1, settings)
		}
		for _, r := range cc {
			_ = db.upsertObservedContact(ctx, userID, r.Name, r.Email, seenAt, 1, settings)
		}
		for _, r := range bcc {
			_ = db.upsertObservedContact(ctx, userID, r.Name, r.Email, seenAt, 1, settings)
		}
	}
}
