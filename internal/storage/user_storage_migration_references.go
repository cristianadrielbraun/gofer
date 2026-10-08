package storage

import (
	"context"
	"database/sql"
	"encoding/json"
)

// These references deliberately have no FK: historical snapshots may outlive a
// deleted target. Preserve missing/empty targets, but reject an existing target
// owned by someone else. Provider resource IDs and Internet Message-IDs are not
// local keys and must never be inferred as local references.
var userStorageMigrationOptionalLinks = []migrationLink{
	{"contact_cards", "accounts", []string{"account_id"}, []string{"id"}},
	{"contact_cards", "account_contact_address_books", []string{"address_book_id"}, []string{"id"}},
	{"contact_conflicts", "accounts", []string{"account_id"}, []string{"id"}},
	{"contact_sync_memberships", "accounts", []string{"account_id"}, []string{"id"}},
	{"contact_sync_memberships", "account_contact_address_books", []string{"address_book_id"}, []string{"id"}},
	{"contact_observations", "contact_profiles", []string{"profile_id"}, []string{"id"}},
	{"contact_sync_operations", "contact_profiles", []string{"contact_id"}, []string{"id"}},
	{"calendar_create_requests", "calendar_events", []string{"event_id"}, []string{"id"}},
	{"calendar_incoming_messages", "calendar_events", []string{"event_id"}, []string{"id"}},
	{"message_mutations", "folders", []string{"folder_id"}, []string{"id"}},
	{"message_mutations", "folders", []string{"destination_folder_id"}, []string{"id"}},
	{"label_mutation_queue", "folders", []string{"folder_id"}, []string{"id"}},
	{"imap_draft_states", "folders", []string{"folder_id"}, []string{"id"}},
	{"gofer_provider_draft_states", "folders", []string{"folder_id"}, []string{"id"}},
}

// Streaming inspection does not canonicalize, repair or adopt queued payloads.
// Malformed/unbound legacy work stays byte-for-byte unchanged and cannot acquire
// a new authority through migration. Valid embedded local references still must
// agree with the row's original owner.
func migrationSourcePayloads(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,payload_json FROM contact_sync_operations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var owner, raw string
		if err := rows.Scan(&owner, &raw); err != nil {
			rows.Close()
			return err
		}
		var payload ContactSyncOperationPayload
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		// Contact.ID selected the original profile in the shared worker. Target
		// lists, Previous and the account exclusion are historical values, not
		// grants: both shared and owned workers re-read owned current targets.
		// Preserve those ignored snapshots even if an old target now belongs
		// elsewhere; never replay or rewrite them during migration.
		if err := migrationEmbeddedReference(ctx, tx, owner, "contact_profiles", payload.Contact.ID); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT user_id,payload FROM calendar_reply_jobs`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var owner, raw string
		if err := rows.Scan(&owner, &raw); err != nil {
			rows.Close()
			return err
		}
		var payload struct {
			Event         CalendarEvent
			UserAuthority *migrationCalendarAuthority
		}
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		if payload.Event.UserID != "" && payload.Event.UserID != owner {
			rows.Close()
			return migrationSourceError("calendar_reply_jobs", "payload crosses storage owners")
		}
		if err := migrationEmbeddedReference(ctx, tx, owner, "calendar_events", payload.Event.ID); err != nil {
			rows.Close()
			return err
		}
		if err := migrationEmbeddedReference(ctx, tx, owner, "calendar_sources", payload.Event.SourceID); err != nil {
			rows.Close()
			return err
		}
		if err := migrationCalendarPayloadAuthority(ctx, tx, owner, payload.UserAuthority); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT a.user_id,s.message_json FROM outgoing_sends s JOIN accounts a ON a.id=s.account_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, raw string
		if err := rows.Scan(&owner, &raw); err != nil {
			return err
		}
		var payload struct {
			Notification *struct {
				UserID, SourceID string
				UserAuthority    *migrationCalendarAuthority
			} `json:"calendar_notification"`
		}
		if json.Unmarshal([]byte(raw), &payload) != nil || payload.Notification == nil {
			continue
		}
		note := payload.Notification
		if note.UserID != "" && note.UserID != owner {
			return migrationSourceError("outgoing_sends", "payload crosses storage owners")
		}
		if err := migrationEmbeddedReference(ctx, tx, owner, "calendar_sources", note.SourceID); err != nil {
			return err
		}
		if err := migrationCalendarPayloadAuthority(ctx, tx, owner, note.UserAuthority); err != nil {
			return err
		}
	}
	return rows.Err()
}

type migrationCalendarAuthority struct{ Owner, Account, Source, Event string }

func migrationCalendarPayloadAuthority(ctx context.Context, tx *sql.Tx, owner string, authority *migrationCalendarAuthority) error {
	if authority == nil {
		return nil
	}
	if authority.Owner != "" && authority.Owner != owner {
		return migrationSourceError("calendar", "saved authority crosses storage owners")
	}
	for _, ref := range []struct{ table, id string }{{"accounts", authority.Account}, {"calendar_sources", authority.Source}, {"calendar_events", authority.Event}} {
		if err := migrationEmbeddedReference(ctx, tx, owner, ref.table, ref.id); err != nil {
			return err
		}
	}
	return nil
}

func migrationEmbeddedReference(ctx context.Context, tx *sql.Tx, owner, table, id string) error {
	if id == "" {
		return nil
	}
	var foreign bool
	rule := userStorageMigrationTables[table]
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+quoteStoreIdentifier(table)+` r WHERE r.id=? AND (`+rule.ownerSQL("", "r")+`) IS NOT ?)`, id, owner).Scan(&foreign); err != nil {
		return err
	}
	if foreign {
		return migrationSourceError(table, "queued local reference crosses storage owners")
	}
	return nil
}
