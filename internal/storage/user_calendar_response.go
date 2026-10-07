package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// An empty claim_id preserves historical reservations without adopting them.
// Their original uncertainty remains protected by the unchanged composite key.
func migrateV106ToV107(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 107 {
		return nil
	}
	present, err := columnExistsTx(tx, "calendar_response_requests", "claim_id")
	if err != nil {
		return err
	}
	if !present {
		if _, err := tx.Exec(`ALTER TABLE calendar_response_requests ADD COLUMN claim_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	} else {
		// Historical partial upgrades may already contain the new column. Keep
		// existing claims, but reject an incompatible identity representation.
		var kind, defaultValue string
		var required int
		if err := tx.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info('calendar_response_requests') WHERE name='claim_id'`).Scan(&kind, &required, &defaultValue); err != nil {
			return err
		}
		if !strings.EqualFold(kind, "TEXT") || required != 1 || defaultValue != "''" {
			return fmt.Errorf("calendar response claim column has an incompatible definition")
		}
	}
	if err := markSchemaVersion(tx, 107); err != nil {
		return err
	}
	return tx.Commit()
}

type UserCalendarResponseClaim struct {
	snapshot                             *UserCalendarEventSnapshot
	remote, version, response, id, scope string
}

type CalendarResponseClaimIdentity struct {
	ID, RemoteID, Version, Response, Scope string
}

func (s *UserCalendarResponseClaim) Identity() CalendarResponseClaimIdentity {
	return CalendarResponseClaimIdentity{s.id, s.remote, s.version, s.response, s.scope}
}

func calendarResponseClaimTarget(snapshot *UserCalendarEventSnapshot, scope, version, response string) (string, error) {
	if snapshot == nil || snapshot.event.ID == "" || strings.TrimSpace(version) == "" || len(version) > 2048 || (response != "accepted" && response != "tentative" && response != "declined") {
		return "", ErrCalendarEventChanged
	}
	role := strings.ToLower(strings.TrimSpace(snapshot.source.AccessRole))
	provider := snapshot.source.Provider
	if (provider != "gmail" && provider != "outlook" && provider != CalendarSourceProviderCalDAV) || (role != "owner" && role != "writer" && !(provider == CalendarSourceProviderCalDAV && (role == "" || role == "unknown"))) || snapshot.event.IsDeleted || snapshot.event.Status == "cancelled" {
		return "", ErrCalendarEventChanged
	}
	event := snapshot.event
	series := event.SeriesRemoteID != "" || calendarPublicationHasRecurrence(event.RecurrenceJSON)
	switch scope {
	case "event":
		if series || version != event.ETag {
			return "", ErrCalendarEventChanged
		}
	case "occurrence":
		if event.SeriesRemoteID == "" || event.SeriesRemoteID == event.RemoteID || version != event.ETag {
			return "", ErrCalendarEventChanged
		}
	case "series":
		if !series {
			return "", ErrCalendarEventChanged
		}
		if event.SeriesRemoteID != "" {
			return event.SeriesRemoteID, nil
		}
	default:
		return "", ErrCalendarEventChanged
	}
	if event.RemoteID == "" {
		return "", ErrCalendarEventChanged
	}
	return event.RemoteID, nil
}

func calendarResponseClaimMatchesTx(ctx context.Context, tx *sql.Tx, claim *UserCalendarResponseClaim) error {
	var id, response string
	err := tx.QueryRowContext(ctx, `SELECT claim_id,response FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=?`, claim.snapshot.event.UserID, claim.snapshot.source.ID, claim.remote, claim.version).Scan(&id, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarEventChanged
	}
	if err != nil {
		return err
	}
	if id != claim.id || response != claim.response {
		return ErrCalendarEventChanged
	}
	return nil
}

// Reserve before a provider write. The scope selects a private captured remote
// identity; only the series master's freshly verified version comes from the
// provider preflight. No browser owner/source/remote ID can retarget this claim.
func (db *DB) ReserveUserCalendarResponse(ctx context.Context, snapshot *UserCalendarEventSnapshot, scope, version, response string, guard func(*sql.Tx, string) error) (*UserCalendarResponseClaim, error) {
	remote, err := calendarResponseClaimTarget(snapshot, scope, version, response)
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, ErrCalendarEventChanged
	}
	claim := &UserCalendarResponseClaim{snapshot: snapshot, remote: remote, version: version, response: response, id: uuid.NewString(), scope: scope}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateUserCalendarEventTx(ctx, tx, snapshot, guard); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,claim_id) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, snapshot.event.UserID, snapshot.source.ID, remote, version, response, claim.id)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrCalendarResponsePending
	}
	if err := calendarResponseClaimMatchesTx(ctx, tx, claim); err != nil {
		return nil, err
	}
	if err := validateUserCalendarEventTx(ctx, tx, snapshot, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

// Call only after definitive rejection or before any provider write. An
// ambiguous response keeps its durable reservation across eviction/restart.
func (db *DB) ReleaseUserCalendarResponse(ctx context.Context, claim *UserCalendarResponseClaim, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.snapshot == nil || claim.id == "" || guard == nil {
		return ErrCalendarEventChanged
	}
	snapshot := claim.snapshot
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarEventTx(ctx, tx, snapshot, guard); err != nil {
		return err
	}
	if err := calendarResponseClaimMatchesTx(ctx, tx, claim); err != nil {
		return err
	}
	var queued bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_reply_jobs WHERE user_id=? AND source_id=? AND remote_id=? AND version=? AND state='pending')`, snapshot.event.UserID, snapshot.source.ID, claim.remote, claim.version).Scan(&queued); err != nil {
		return err
	}
	if queued {
		return ErrCalendarResponsePending
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=? AND response=? AND claim_id=?`, snapshot.event.UserID, snapshot.source.ID, claim.remote, claim.version, claim.response, claim.id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCalendarEventChanged
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=?)`, snapshot.event.UserID, snapshot.source.ID, claim.remote, claim.version).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrCalendarEventChanged
	}
	if err := validateUserCalendarEventTx(ctx, tx, snapshot, guard); err != nil {
		return err
	}
	return tx.Commit()
}
