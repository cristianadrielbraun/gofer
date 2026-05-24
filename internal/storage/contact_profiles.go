package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

func normalizeContactFieldValue(kind, value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "email":
		return normalizeContactEmail(value)
	default:
		return strings.ToLower(value)
	}
}

func (db *DB) SaveContactProfile(ctx context.Context, userID string, profile models.ContactProfile) (models.ContactProfile, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return models.ContactProfile{}, fmt.Errorf("user is required")
	}
	profile.ID = strings.TrimSpace(profile.ID)
	if profile.ID == "" {
		profile.ID = uuid.NewString()
	}
	profile.UserID = userID
	profile.DisplayName = contactDisplayName(profile.DisplayName, profile.PrimaryEmail)
	profile.SortName = strings.TrimSpace(profile.SortName)
	if profile.SortName == "" {
		profile.SortName = profile.DisplayName
	}
	profile.PrimaryEmail = strings.TrimSpace(profile.PrimaryEmail)

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.ContactProfile{}, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO contact_profiles (id, user_id, display_name, sort_name, primary_email, avatar_url, notes, is_deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			sort_name = excluded.sort_name,
			primary_email = excluded.primary_email,
			avatar_url = excluded.avatar_url,
			notes = excluded.notes,
			is_deleted = excluded.is_deleted,
			updated_at = CURRENT_TIMESTAMP`,
		profile.ID, profile.UserID, profile.DisplayName, profile.SortName, profile.PrimaryEmail, strings.TrimSpace(profile.AvatarURL), strings.TrimSpace(profile.Notes), boolInt(profile.IsDeleted))
	if err != nil {
		return models.ContactProfile{}, err
	}

	if err := upsertContactCardsTx(ctx, tx, profile); err != nil {
		return models.ContactProfile{}, err
	}
	if err := replaceContactFieldsTx(ctx, tx, profile); err != nil {
		return models.ContactProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.ContactProfile{}, err
	}
	saved, err := db.GetContactProfile(ctx, userID, profile.ID)
	if err != nil || saved == nil {
		return models.ContactProfile{}, err
	}
	return *saved, nil
}

func upsertContactCardsTx(ctx context.Context, tx *sql.Tx, profile models.ContactProfile) error {
	for _, card := range profile.Cards {
		card.ID = strings.TrimSpace(card.ID)
		if card.ID == "" {
			card.ID = uuid.NewString()
		}
		kind := strings.TrimSpace(card.Kind)
		if kind == "" {
			kind = "local"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id, etag, raw_payload, raw_payload_type, sync_status, last_error, is_deleted)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
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
				is_deleted = excluded.is_deleted,
				updated_at = CURRENT_TIMESTAMP`,
			card.ID, profile.UserID, profile.ID, kind, strings.TrimSpace(card.Provider), strings.TrimSpace(card.AccountID), strings.TrimSpace(card.AddressBookID), strings.TrimSpace(card.RemoteID), strings.TrimSpace(card.Etag), card.RawPayload, strings.TrimSpace(card.RawPayloadType), strings.TrimSpace(card.SyncStatus), strings.TrimSpace(card.LastError), boolInt(card.IsDeleted)); err != nil {
			return err
		}
	}
	return nil
}

func replaceContactFieldsTx(ctx context.Context, tx *sql.Tx, profile models.ContactProfile) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_fields WHERE user_id = ? AND profile_id = ?`, profile.UserID, profile.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_identities WHERE user_id = ? AND profile_id = ?`, profile.UserID, profile.ID); err != nil {
		return err
	}
	for i, field := range profile.Fields {
		field.ID = strings.TrimSpace(field.ID)
		if field.ID == "" {
			field.ID = uuid.NewString()
		}
		field.Kind = strings.ToLower(strings.TrimSpace(field.Kind))
		field.Value = strings.TrimSpace(field.Value)
		if field.Kind == "" || field.Value == "" {
			continue
		}
		field.NormalizedValue = normalizeContactFieldValue(field.Kind, field.Value)
		if field.NormalizedValue == "" {
			continue
		}
		if field.Confidence <= 0 {
			field.Confidence = 1
		}
		ordinal := field.Ordinal
		if ordinal <= 0 {
			ordinal = i + 1
		}
		var cardID sql.NullString
		if trimmed := strings.TrimSpace(field.CardID); trimmed != "" {
			cardID = sql.NullString{String: trimmed, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_fields (id, user_id, profile_id, card_id, kind, label, value, normalized_value, is_primary, ordinal, source, confidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			field.ID, profile.UserID, profile.ID, cardID, field.Kind, strings.TrimSpace(field.Label), field.Value, field.NormalizedValue, boolInt(field.IsPrimary), ordinal, strings.TrimSpace(field.Source), field.Confidence); err != nil {
			return err
		}
		if field.Kind == "email" || field.Kind == "phone" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO contact_identities (user_id, profile_id, kind, normalized_value, confidence)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(user_id, kind, normalized_value) DO UPDATE SET
					profile_id = excluded.profile_id,
					confidence = excluded.confidence,
					updated_at = CURRENT_TIMESTAMP`,
				profile.UserID, profile.ID, field.Kind, field.NormalizedValue, field.Confidence); err != nil {
				return err
			}
		}
	}
	return nil
}

func (db *DB) GetContactProfile(ctx context.Context, userID, profileID string) (*models.ContactProfile, error) {
	var profile models.ContactProfile
	var isDeleted int
	err := db.Read().QueryRowContext(ctx, `
		SELECT id, user_id, display_name, sort_name, primary_email, avatar_url, notes, is_deleted
		FROM contact_profiles
		WHERE user_id = ? AND id = ?`, userID, profileID).Scan(&profile.ID, &profile.UserID, &profile.DisplayName, &profile.SortName, &profile.PrimaryEmail, &profile.AvatarURL, &profile.Notes, &isDeleted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	profile.IsDeleted = isDeleted == 1
	cards, err := db.ListContactCards(ctx, userID, profileID)
	if err != nil {
		return nil, err
	}
	fields, err := db.ListContactFields(ctx, userID, profileID)
	if err != nil {
		return nil, err
	}
	profile.Cards = cards
	profile.Fields = fields
	profile.Insights = ContactProfileInsights(profile)
	return &profile, nil
}

func ContactProfileInsights(profile models.ContactProfile) []models.ContactInsight {
	var insights []models.ContactInsight
	activeCards := make([]models.ContactCard, 0, len(profile.Cards))
	providerCards := make([]models.ContactCard, 0, len(profile.Cards))
	providerKeys := make(map[string]bool)
	for _, card := range profile.Cards {
		if card.IsDeleted {
			continue
		}
		activeCards = append(activeCards, card)
		if card.Kind == "provider" {
			providerCards = append(providerCards, card)
			key := strings.TrimSpace(card.Provider) + ":" + strings.TrimSpace(card.AccountID) + ":" + strings.TrimSpace(card.AddressBookID)
			providerKeys[key] = true
		}
	}
	if len(providerCards) > 1 {
		insights = append(insights, models.ContactInsight{
			Kind:     "multi_source",
			Severity: "info",
			Title:    "Exists in multiple address books",
			Message:  fmt.Sprintf("This profile has %d provider cards across %d account/address-book locations.", len(providerCards), len(providerKeys)),
			Count:    len(providerCards),
		})
	}
	if len(activeCards) == 1 && activeCards[0].Kind == "observed" {
		insights = append(insights, models.ContactInsight{
			Kind:     "observed_only",
			Severity: "info",
			Title:    "Only observed from mail",
			Message:  "This contact was inferred from email activity and is not saved to an address book yet.",
			Count:    1,
		})
	}

	fieldsByKind := make(map[string][]models.ContactField)
	for _, field := range profile.Fields {
		kind := strings.ToLower(strings.TrimSpace(field.Kind))
		if kind == "" || strings.TrimSpace(field.Value) == "" {
			continue
		}
		fieldsByKind[kind] = append(fieldsByKind[kind], field)
	}
	for kind, fields := range fieldsByKind {
		uniqueValues := make(map[string]models.ContactField)
		manualValues := make(map[string]models.ContactField)
		providerValues := make(map[string]models.ContactField)
		for _, field := range fields {
			normalized := strings.TrimSpace(field.NormalizedValue)
			if normalized == "" {
				normalized = normalizeContactFieldValue(kind, field.Value)
			}
			if normalized == "" {
				continue
			}
			uniqueValues[normalized] = field
			if field.Source == "manual" {
				manualValues[normalized] = field
			} else if strings.HasPrefix(field.Source, "synced:") || field.Source == "observed" {
				providerValues[normalized] = field
			}
		}
		if len(uniqueValues) > 1 {
			severity := "notice"
			if kind == "email" || kind == "phone" {
				severity = "warning"
			}
			insights = append(insights, models.ContactInsight{
				Kind:     "field_conflict",
				Severity: severity,
				Title:    fmt.Sprintf("%s has multiple values", contactInsightFieldLabel(kind)),
				Message:  fmt.Sprintf("This profile has %d different %s values from its cards and fields.", len(uniqueValues), contactInsightFieldLabel(kind)),
				Field:    kind,
				Count:    len(uniqueValues),
			})
		}
		if len(providerValues) > 0 && len(manualValues) == 0 {
			insights = append(insights, models.ContactInsight{
				Kind:     "provider_only_field",
				Severity: "info",
				Title:    fmt.Sprintf("%s only exists in synced data", contactInsightFieldLabel(kind)),
				Message:  fmt.Sprintf("No manual %s is set; Gofer is currently using provider or observed data.", contactInsightFieldLabel(kind)),
				Field:    kind,
				Count:    len(providerValues),
			})
		}
		if len(manualValues) > 0 && len(providerValues) > 0 {
			for manualValue := range manualValues {
				for providerValue, providerField := range providerValues {
					if manualValue == providerValue {
						continue
					}
					insights = append(insights, models.ContactInsight{
						Kind:     "manual_override",
						Severity: "notice",
						Title:    fmt.Sprintf("Manual %s differs from synced data", contactInsightFieldLabel(kind)),
						Message:  fmt.Sprintf("Gofer keeps the manual %s while preserving the provider value for review.", contactInsightFieldLabel(kind)),
						Field:    kind,
						Source:   providerField.Source,
						Count:    len(manualValues) + len(providerValues),
					})
					break
				}
				break
			}
		}
	}
	return insights
}

func contactInsightFieldLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "email":
		return "email"
	case "phone":
		return "phone"
	case "organization":
		return "organization"
	default:
		if strings.TrimSpace(kind) == "" {
			return "field"
		}
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

func (db *DB) ListContactCards(ctx context.Context, userID, profileID string) ([]models.ContactCard, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id, etag, raw_payload, raw_payload_type, sync_status, last_error, is_deleted
		FROM contact_cards
		WHERE user_id = ? AND profile_id = ?
		ORDER BY CASE kind WHEN 'local' THEN 0 ELSE 1 END, provider, account_id, address_book_id`, userID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cards []models.ContactCard
	for rows.Next() {
		var card models.ContactCard
		var isDeleted int
		if err := rows.Scan(&card.ID, &card.UserID, &card.ProfileID, &card.Kind, &card.Provider, &card.AccountID, &card.AddressBookID, &card.RemoteID, &card.Etag, &card.RawPayload, &card.RawPayloadType, &card.SyncStatus, &card.LastError, &isDeleted); err != nil {
			return nil, err
		}
		card.IsDeleted = isDeleted == 1
		cards = append(cards, card)
	}
	return cards, rows.Err()
}

func (db *DB) ListContactFields(ctx context.Context, userID, profileID string) ([]models.ContactField, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, profile_id, COALESCE(card_id, ''), kind, label, value, normalized_value, is_primary, ordinal, source, confidence
		FROM contact_fields
		WHERE user_id = ? AND profile_id = ?
		ORDER BY kind, ordinal, value COLLATE NOCASE`, userID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fields []models.ContactField
	for rows.Next() {
		var field models.ContactField
		var isPrimary int
		if err := rows.Scan(&field.ID, &field.UserID, &field.ProfileID, &field.CardID, &field.Kind, &field.Label, &field.Value, &field.NormalizedValue, &isPrimary, &field.Ordinal, &field.Source, &field.Confidence); err != nil {
			return nil, err
		}
		field.IsPrimary = isPrimary == 1
		fields = append(fields, field)
	}
	return fields, rows.Err()
}

func (db *DB) PreferContactField(ctx context.Context, userID, profileID, fieldID string) (*models.ContactProfile, error) {
	userID = strings.TrimSpace(userID)
	profileID = strings.TrimSpace(profileID)
	fieldID = strings.TrimSpace(fieldID)
	if userID == "" || profileID == "" || fieldID == "" {
		return nil, fmt.Errorf("contact field is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var field models.ContactField
	var isPrimary int
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, profile_id, COALESCE(card_id, ''), kind, label, value, normalized_value, is_primary, ordinal, source, confidence
		FROM contact_fields
		WHERE user_id = ? AND profile_id = ? AND id = ?`, userID, profileID, fieldID).Scan(&field.ID, &field.UserID, &field.ProfileID, &field.CardID, &field.Kind, &field.Label, &field.Value, &field.NormalizedValue, &isPrimary, &field.Ordinal, &field.Source, &field.Confidence)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	field.IsPrimary = isPrimary == 1
	field.Kind = strings.ToLower(strings.TrimSpace(field.Kind))
	if field.Kind == "" || strings.TrimSpace(field.Value) == "" {
		return nil, fmt.Errorf("contact field cannot be preferred")
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE contact_fields
		SET is_primary = 0, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND profile_id = ? AND kind = ?`, userID, profileID, field.Kind); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE contact_fields
		SET is_primary = 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND profile_id = ? AND id = ?`, userID, profileID, fieldID); err != nil {
		return nil, err
	}
	switch field.Kind {
	case "email":
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles
			SET primary_email = ?, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND id = ?`, strings.TrimSpace(field.Value), userID, profileID); err != nil {
			return nil, err
		}
	case "name", "full_name", "display_name":
		display := strings.TrimSpace(field.Value)
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles
			SET display_name = ?, sort_name = ?, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND id = ?`, display, display, userID, profileID); err != nil {
			return nil, err
		}
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles
			SET updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND id = ?`, userID, profileID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetContactProfile(ctx, userID, profileID)
}

func (db *DB) FindContactProfileByIdentity(ctx context.Context, userID, kind, value string) (*models.ContactProfile, error) {
	normalized := normalizeContactFieldValue(kind, value)
	if userID == "" || strings.TrimSpace(kind) == "" || normalized == "" {
		return nil, nil
	}
	var profileID string
	err := db.Read().QueryRowContext(ctx, `
		SELECT contact_identities.profile_id
		FROM contact_identities
		JOIN contact_profiles cp ON cp.id = contact_identities.profile_id AND cp.user_id = contact_identities.user_id
		WHERE contact_identities.user_id = ? AND contact_identities.kind = ? AND contact_identities.normalized_value = ? AND cp.is_deleted = 0`, userID, strings.ToLower(strings.TrimSpace(kind)), normalized).Scan(&profileID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.GetContactProfile(ctx, userID, profileID)
}
