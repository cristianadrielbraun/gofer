package storage

import (
	"database/sql"
	"fmt"
)

// migrateV91ToV92 retires the original flat contact store. The canonical
// profile/card model has been authoritative since schema v40. Rebuilding the
// operation queue first removes the oldest schema's foreign key to contacts
// while preserving completed and pending operation history.
func migrateV91ToV92(tx *sql.Tx) error {
	usersExist, err := tableExistsTx(tx, "users")
	if err != nil {
		return fmt.Errorf("check users table: %w", err)
	}
	operationsExist, err := tableExistsTx(tx, "contact_sync_operations")
	if err != nil {
		return fmt.Errorf("check contact sync operations table: %w", err)
	}
	if usersExist {
		if err := prepareLegacyContactRetirementV92(tx); err != nil {
			return err
		}
		if operationsExist {
			if _, err := tx.Exec(createContactSyncOperationsTableSQL("contact_sync_operations_v92")); err != nil {
				return fmt.Errorf("create temporary contact sync operations table: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO contact_sync_operations_v92 (
				id, user_id, contact_id, email, payload_json, status, attempt_count,
				last_error, locked_at, next_attempt_at, created_at, updated_at
			)
			SELECT operation.id, operation.user_id,
			       COALESCE(legacy_map.profile_id, operation.contact_id),
			       operation.email, operation.payload_json, operation.status, operation.attempt_count,
			       operation.last_error, operation.locked_at, operation.next_attempt_at,
			       operation.created_at, operation.updated_at
			FROM contact_sync_operations operation
			LEFT JOIN contact_legacy_profile_map_v92 legacy_map
			  ON legacy_map.contact_id = operation.contact_id`); err != nil {
				return fmt.Errorf("copy contact sync operations: %w", err)
			}
			if _, err := tx.Exec(`DROP TABLE contact_sync_operations`); err != nil {
				return fmt.Errorf("drop old contact sync operations table: %w", err)
			}
			if _, err := tx.Exec(createContactSyncOperationsTableSQL("contact_sync_operations")); err != nil {
				return fmt.Errorf("create replacement contact sync operations table: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO contact_sync_operations (
				id, user_id, contact_id, email, payload_json, status, attempt_count,
				last_error, locked_at, next_attempt_at, created_at, updated_at
			)
			SELECT id, user_id, contact_id, email, payload_json, status, attempt_count,
			       last_error, locked_at, next_attempt_at, created_at, updated_at
			FROM contact_sync_operations_v92`); err != nil {
				return fmt.Errorf("restore contact sync operations: %w", err)
			}
			if _, err := tx.Exec(`DROP TABLE contact_sync_operations_v92`); err != nil {
				return fmt.Errorf("drop temporary contact sync operations table: %w", err)
			}
		} else if _, err := tx.Exec(createContactSyncOperationsTableSQL("contact_sync_operations")); err != nil {
			return fmt.Errorf("create contact sync operations table: %w", err)
		}
		for _, statement := range []string{
			`CREATE INDEX idx_contact_sync_operations_due
			 ON contact_sync_operations(status, next_attempt_at, locked_at)`,
			`CREATE INDEX idx_contact_sync_operations_contact
			 ON contact_sync_operations(user_id, contact_id, created_at DESC)`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create contact sync operation index: %w", err)
			}
		}
	}

	statements := []string{
		`DROP TABLE IF EXISTS contact_legacy_profile_map_v92`,
		`DROP TABLE IF EXISTS contact_sources`,
		`DROP TABLE IF EXISTS contact_save_targets`,
		`DROP TABLE IF EXISTS contact_emails`,
		`DROP TABLE IF EXISTS contacts`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (92)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("retire legacy contact schema: %w", err)
		}
	}
	return nil
}

func prepareLegacyContactRetirementV92(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS contact_legacy_profile_map_v92`); err != nil {
		return fmt.Errorf("clear legacy contact mapping table: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE contact_legacy_profile_map_v92 (
		contact_id TEXT PRIMARY KEY,
		profile_id TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create legacy contact mapping table: %w", err)
	}

	contactsExist, err := tableExistsTx(tx, "contacts")
	if err != nil {
		return fmt.Errorf("check legacy contacts table: %w", err)
	}
	if !contactsExist {
		return nil
	}
	profilesExist, err := tableExistsTx(tx, "contact_profiles")
	if err != nil {
		return fmt.Errorf("check contact profiles table: %w", err)
	}
	if profilesExist {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO contact_legacy_profile_map_v92 (contact_id, profile_id)
			SELECT legacy.id, profile.id
			FROM contacts legacy
			JOIN contact_profiles profile
			  ON profile.id = legacy.id AND profile.user_id = legacy.user_id`); err != nil {
			return fmt.Errorf("map legacy contacts by identifier: %w", err)
		}
	}

	emailsExist, err := tableExistsTx(tx, "contact_emails")
	if err != nil {
		return fmt.Errorf("check legacy contact emails table: %w", err)
	}
	identitiesExist, err := tableExistsTx(tx, "contact_identities")
	if err != nil {
		return fmt.Errorf("check contact identities table: %w", err)
	}
	if profilesExist && emailsExist && identitiesExist {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO contact_legacy_profile_map_v92 (contact_id, profile_id)
			SELECT legacy.id, identity.profile_id
			FROM contacts legacy
			JOIN contact_emails legacy_email
			  ON legacy_email.contact_id = legacy.id AND legacy_email.user_id = legacy.user_id
			JOIN contact_identities identity
			  ON identity.user_id = legacy.user_id
			 AND identity.kind = 'email'
			 AND identity.normalized_value = legacy_email.normalized_email
			JOIN contact_profiles profile
			  ON profile.id = identity.profile_id AND profile.user_id = identity.user_id
			ORDER BY legacy_email.is_primary DESC`); err != nil {
			return fmt.Errorf("map legacy contacts by email identity: %w", err)
		}
	}

	var unmappedContacts int
	if err := tx.QueryRow(`
		SELECT COUNT(*)
		FROM contacts legacy
		LEFT JOIN contact_legacy_profile_map_v92 legacy_map ON legacy_map.contact_id = legacy.id
		WHERE legacy_map.contact_id IS NULL`).Scan(&unmappedContacts); err != nil {
		return fmt.Errorf("count unmapped legacy contacts: %w", err)
	}
	if unmappedContacts != 0 {
		return fmt.Errorf("refuse to retire legacy contact schema: %d contacts are not represented in contact_profiles", unmappedContacts)
	}

	if err := verifyLegacyContactSourcesMigratedV92(tx); err != nil {
		return err
	}
	if err := verifyLegacyContactSaveTargetsMigratedV92(tx); err != nil {
		return err
	}
	return nil
}

func verifyLegacyContactSourcesMigratedV92(tx *sql.Tx) error {
	sourcesExist, err := tableExistsTx(tx, "contact_sources")
	if err != nil {
		return fmt.Errorf("check legacy contact sources table: %w", err)
	}
	if !sourcesExist {
		return nil
	}
	var unmappedSources int
	if err := tx.QueryRow(`
		SELECT COUNT(*)
		FROM contact_sources source
		JOIN contact_legacy_profile_map_v92 legacy_map ON legacy_map.contact_id = source.contact_id
		WHERE NOT EXISTS (
			SELECT 1
			FROM contact_cards card
			WHERE card.user_id = source.user_id
			  AND card.profile_id = legacy_map.profile_id
			  AND card.kind = 'provider'
			  AND card.provider = source.provider
			  AND card.account_id = source.account_id
			  AND card.remote_id = source.remote_id
		)`).Scan(&unmappedSources); err != nil {
		return fmt.Errorf("count unmapped legacy contact sources: %w", err)
	}
	if unmappedSources != 0 {
		return fmt.Errorf("refuse to retire legacy contact schema: %d provider sources are not represented in contact_cards", unmappedSources)
	}
	return nil
}

func verifyLegacyContactSaveTargetsMigratedV92(tx *sql.Tx) error {
	targetsExist, err := tableExistsTx(tx, "contact_save_targets")
	if err != nil {
		return fmt.Errorf("check legacy contact save targets table: %w", err)
	}
	if !targetsExist {
		return nil
	}
	var unmappedTargets int
	if err := tx.QueryRow(`
		SELECT COUNT(*)
		FROM contact_save_targets target
		JOIN contact_legacy_profile_map_v92 legacy_map ON legacy_map.contact_id = target.contact_id
		WHERE NOT (
			(target.target = 'local' AND EXISTS (
				SELECT 1 FROM contact_cards card
				WHERE card.user_id = target.user_id
				  AND card.profile_id = legacy_map.profile_id
				  AND card.kind = 'local'
			)) OR
			(target.target LIKE 'account:%' AND EXISTS (
				SELECT 1 FROM contact_sync_memberships membership
				WHERE membership.user_id = target.user_id
				  AND membership.profile_id = legacy_map.profile_id
				  AND membership.account_id = substr(target.target, 9)
			))
		)`).Scan(&unmappedTargets); err != nil {
		return fmt.Errorf("count unmapped legacy contact save targets: %w", err)
	}
	if unmappedTargets != 0 {
		return fmt.Errorf("refuse to retire legacy contact schema: %d save targets are not represented in contact_sync_memberships or contact_cards", unmappedTargets)
	}
	return nil
}

func createContactSyncOperationsTableSQL(tableName string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		contact_id TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		attempt_count INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		locked_at DATETIME,
		next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`, tableName)
}
