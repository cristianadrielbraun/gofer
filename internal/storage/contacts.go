package storage

import (
	"context"
	"database/sql"
	"fmt"
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
	return status, rows.Err()
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
	where, args := contactFilterSQL(userID, filters)
	args = append(args, limit, offset)

	rows, err := db.Read().QueryContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE `+where+`
		ORDER BY COALESCE(ce.last_seen_at, c.updated_at) DESC, c.display_name COLLATE NOCASE
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	for rows.Next() {
		c, err := scanContactRow(rows)
		if err != nil {
			return nil, err
		}
		db.hydrateContactAvatar(ctx, &c)
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (db *DB) CountContacts(ctx context.Context, userID string, filters models.ContactFilters) (int, error) {
	where, args := contactFilterSQL(userID, filters)
	var count int
	err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT c.id)
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id
		WHERE `+where, args...).Scan(&count)
	return count, err
}

func (db *DB) ListContactsForExport(ctx context.Context, userID string) ([]models.Contact, error) {
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

	var contacts []models.Contact
	for rows.Next() {
		c, err := scanContactRow(rows)
		if err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func contactFilterSQL(userID string, filters models.ContactFilters) (string, []any) {
	query := strings.TrimSpace(filters.Query)
	where := `c.user_id = ? AND c.is_deleted = 0`
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

func (db *DB) SearchContacts(ctx context.Context, userID, query string, limit int) ([]models.Contact, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	like := "%" + query + "%"
	rows, err := db.Read().QueryContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id
		WHERE c.user_id = ? AND c.is_deleted = 0
		  AND (c.display_name LIKE ? OR ce.email LIKE ? OR ce.normalized_email LIKE ?)
		ORDER BY CASE WHEN ce.normalized_email = ? THEN 0 WHEN ce.normalized_email LIKE ? THEN 1 ELSE 2 END,
		         COALESCE(ce.last_seen_at, c.updated_at) DESC,
		         c.display_name COLLATE NOCASE
		LIMIT ?`, userID, like, like, strings.ToLower(like), normalizeContactEmail(query), strings.ToLower(query)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	for rows.Next() {
		c, err := scanContactRow(rows)
		if err != nil {
			return nil, err
		}
		db.hydrateContactAvatar(ctx, &c)
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (db *DB) GetContact(ctx context.Context, userID, contactID string) (*models.Contact, error) {
	if contactID == "" {
		return nil, nil
	}
	row := db.Read().QueryRowContext(ctx, `
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE c.user_id = ? AND c.id = ? AND c.is_deleted = 0`, userID, contactID)
	c, err := scanContactRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	db.hydrateContactAvatar(ctx, &c)
	c.SaveTargets, _ = db.GetContactSaveTargets(ctx, userID, contactID)
	return &c, nil
}

func (db *DB) GetContactSaveTargets(ctx context.Context, userID, contactID string) ([]string, error) {
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

func scanContactRow(scanner interface{ Scan(dest ...any) error }) (models.Contact, error) {
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
		c.LastSeenAt = formatContactTime(lastSeen.String)
	}
	if createdAt.Valid {
		c.CreatedAt = formatContactTime(createdAt.String)
	}
	if updatedAt.Valid {
		c.UpdatedAt = formatContactTime(updatedAt.String)
	}
	return c, nil
}

func formatContactTime(raw string) string {
	if t, ok := parseSQLiteDateTime(raw); ok {
		return t.Local().Format("Jan 2, 2006")
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

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.Contact{}, err
	}
	defer tx.Rollback()

	contactID := strings.TrimSpace(contact.ID)
	if contactID == "" {
		_ = tx.QueryRowContext(ctx, `SELECT contact_id FROM contact_emails WHERE user_id = ? AND normalized_email = ?`, userID, normalized).Scan(&contactID)
	}
	created := false
	if contactID == "" {
		contactID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contacts (id, user_id, display_name, source, is_manual, is_deleted, suppress_auto_create)
			VALUES (?, ?, ?, 'manual', 1, 0, 0)`, contactID, userID, name); err != nil {
			return models.Contact{}, err
		}
		created = true
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE contacts
			SET display_name = ?, source = 'manual', is_manual = 1, is_deleted = 0, suppress_auto_create = 0, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ?`, name, contactID, userID); err != nil {
			return models.Contact{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE contact_emails SET is_primary = 0 WHERE contact_id = ?`, contactID); err != nil {
		return models.Contact{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_emails (id, user_id, contact_id, email, normalized_email, is_primary, observed_name)
		VALUES (?, ?, ?, ?, ?, 1, '')
		ON CONFLICT(user_id, normalized_email) DO UPDATE SET
			contact_id = excluded.contact_id,
			email = excluded.email,
			is_primary = 1,
			updated_at = CURRENT_TIMESTAMP`, uuid.NewString(), userID, contactID, email, normalized); err != nil {
		return models.Contact{}, err
	}
	if err := db.replaceContactSaveTargetsTx(ctx, tx, userID, contactID, contact.SaveTargets); err != nil {
		return models.Contact{}, err
	}

	if err := tx.Commit(); err != nil {
		return models.Contact{}, err
	}
	saved, err := db.GetContact(ctx, userID, contactID)
	if err != nil || saved == nil {
		return models.Contact{}, err
	}
	if created {
		_ = db.LogContactActivity(ctx, userID, "manual_contact_added", email, "Manual contact added", 1)
	}
	return *saved, nil
}

func (db *DB) UpsertSyncedContact(ctx context.Context, userID, accountID, name, email string) (bool, error) {
	email = strings.TrimSpace(email)
	normalized := normalizeContactEmail(email)
	accountID = strings.TrimSpace(accountID)
	if userID == "" || accountID == "" || normalized == "" {
		return false, nil
	}
	display := contactDisplayName(name, email)
	source := "synced:" + accountID
	target := "account:" + accountID

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var contactID string
	var isManual int
	var currentDisplay, currentSource string
	err = tx.QueryRowContext(ctx, `
		SELECT c.id, c.is_manual, c.display_name, c.source
		FROM contact_emails ce
		JOIN contacts c ON ce.contact_id = c.id
		WHERE ce.user_id = ? AND ce.normalized_email = ?`, userID, normalized).Scan(&contactID, &isManual, &currentDisplay, &currentSource)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}

	created := false
	if contactID == "" {
		contactID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contacts (id, user_id, display_name, source, is_manual, is_deleted, suppress_auto_create)
			VALUES (?, ?, ?, ?, 0, 0, 0)`, contactID, userID, display, source); err != nil {
			return false, err
		}
		created = true
	} else {
		if isManual == 0 {
			if strings.TrimSpace(currentDisplay) != "" && normalizeContactEmail(currentDisplay) != normalized {
				display = currentDisplay
			}
			if currentSource != "" && currentSource != "observed" && currentSource != "provider:gmail" {
				source = currentSource
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE contacts
				SET display_name = ?, source = ?,
				    is_deleted = 0, suppress_auto_create = 0, updated_at = CURRENT_TIMESTAMP
				WHERE id = ? AND user_id = ?`, display, source, contactID, userID); err != nil {
				return false, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE contacts
				SET is_deleted = 0, suppress_auto_create = 0, updated_at = CURRENT_TIMESTAMP
				WHERE id = ? AND user_id = ?`, contactID, userID); err != nil {
				return false, err
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_emails (id, user_id, contact_id, email, normalized_email, is_primary, observed_name)
		VALUES (?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(user_id, normalized_email) DO UPDATE SET
			contact_id = excluded.contact_id,
			email = excluded.email,
			updated_at = CURRENT_TIMESTAMP`, uuid.NewString(), userID, contactID, email, normalized, strings.TrimSpace(name)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO contact_save_targets (contact_id, user_id, target)
		VALUES (?, ?, ?)`, contactID, userID, target); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

func (db *DB) DeleteContact(ctx context.Context, userID, contactID string, preventRecreate bool) error {
	if contactID == "" {
		return nil
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
		SELECT c.id, c.display_name, ce.email, c.source, c.is_manual, c.is_deleted,
		       ce.message_count, ce.last_seen_at, c.created_at, c.updated_at
		FROM contacts c
		JOIN contact_emails ce ON ce.contact_id = c.id AND ce.is_primary = 1
		WHERE c.user_id = ? AND c.is_deleted = 1 AND c.suppress_auto_create = 1
		ORDER BY c.updated_at DESC, c.display_name COLLATE NOCASE
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("query suppressed contacts: %w", err)
	}
	defer rows.Close()

	var contacts []models.Contact
	for rows.Next() {
		c, err := scanContactRow(rows)
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
		FROM contacts
		WHERE user_id = ? AND is_deleted = 1 AND suppress_auto_create = 1`, userID).Scan(&count)
	return count, err
}

func (db *DB) ClearSuppressedContacts(ctx context.Context, userID string) (int64, error) {
	res, err := db.Write().ExecContext(ctx, `
		DELETE FROM contacts
		WHERE user_id = ? AND is_deleted = 1 AND suppress_auto_create = 1`, userID)
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
		DELETE FROM contacts
		WHERE id = ? AND user_id = ? AND is_deleted = 1 AND suppress_auto_create = 1`, contactID, userID)
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

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var contactID string
	var isManual, isDeleted, suppressAuto int
	var currentDisplay string
	err = tx.QueryRowContext(ctx, `
		SELECT c.id, c.display_name, c.is_manual, c.is_deleted, c.suppress_auto_create
		FROM contact_emails ce
		JOIN contacts c ON ce.contact_id = c.id
		WHERE ce.user_id = ? AND ce.normalized_email = ?`, userID, normalized).Scan(&contactID, &currentDisplay, &isManual, &isDeleted, &suppressAuto)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if contactID != "" {
		if isDeleted == 1 {
			if settings.PreventRecreateDeleted && suppressAuto == 1 {
				return tx.Commit()
			}
			if !settings.AutoCreateObserved {
				return tx.Commit()
			}
			if _, err := tx.ExecContext(ctx, `UPDATE contacts SET is_deleted = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, contactID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_emails
			SET email = ?, observed_name = ?, message_count = message_count + ?, last_seen_at = MAX(COALESCE(last_seen_at, ?), ?), updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND normalized_email = ?`, email, strings.TrimSpace(name), count, seenAt, seenAt, userID, normalized); err != nil {
			return err
		}
		if isManual == 0 && (strings.TrimSpace(currentDisplay) == "" || normalizeContactEmail(currentDisplay) == normalized) {
			if _, err := tx.ExecContext(ctx, `UPDATE contacts SET display_name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, display, contactID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}

	if !settings.AutoCreateObserved {
		return tx.Commit()
	}
	contactID = uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contacts (id, user_id, display_name, source, is_manual)
		VALUES (?, ?, ?, 'observed', 0)`, contactID, userID, display); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_emails (id, user_id, contact_id, email, normalized_email, is_primary, observed_name, message_count, last_seen_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`, uuid.NewString(), userID, contactID, email, normalized, strings.TrimSpace(name), count, seenAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = db.LogContactActivity(ctx, userID, "observed_contact_added", email, "Observed contact added", 1)
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
