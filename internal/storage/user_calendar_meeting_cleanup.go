package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Cleanup is a separate capability. It cannot be passed to event/create/draft
// APIs, whose selected-source checks remain unchanged.
type UserCalendarMeetingCleanupSnapshot struct {
	source      CalendarSource
	sourceState [32]byte
	id, request string
	reservation userCalendarCreateRecord
	record      calendarMeetingDraftRecord
	finalID     string
	final       userCalendarCreateRecord
	prune       bool
}

func (c *UserCalendarMeetingCleanupSnapshot) Source() CalendarSource    { return c.source }
func (c *UserCalendarMeetingCleanupSnapshot) Google() CalendarMeetDraft { return c.record.Google }
func (c *UserCalendarMeetingCleanupSnapshot) Teams() CalendarTeamsDraft { return c.record.Teams }
func (c *UserCalendarMeetingCleanupSnapshot) FinalResult() CalendarCreateRequest {
	return c.final.Result
}
func (c *UserCalendarMeetingCleanupSnapshot) Prune() bool { return c.prune }

// Persist one account's scheduling cursor before attempting native work. It is
// only a hint: neither it nor the returned candidate authorizes a mutation.
// Advancing one item at a time lets a blocked call, malformed legacy claim or
// uncertain saved meeting survive without starving all later drafts. The
// existing owner-scoped settings store keeps this bounded across restarts.
func (db *DB) NextAccountCalendarMeetingCleanup(ctx context.Context, owner, account string, guard func(*sql.Tx, string) error) (*CalendarMeetingCleanupCandidate, error) {
	if guard == nil {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	key := "calendar_meeting_cleanup_cursor:" + account
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id=? AND key=?`, owner, key).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var cursor CalendarMeetingCleanupCandidate
	// Corrupt scheduling hints can safely restart a pass. They never supply
	// resource identities to native cleanup or change any draft's age/state.
	if len(raw) <= 4096 {
		_ = json.Unmarshal([]byte(raw), &cursor)
	}
	rows, err := calendarMeetingCleanupCandidatesTx(ctx, tx, owner, account, 1, &cursor)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	wire, err := json.Marshal(rows[0])
	if err != nil {
		return nil, err
	}
	if err := teamsDraftChanged(tx.ExecContext(ctx, `INSERT INTO app_settings(user_id,key,value) VALUES(?,?,?) ON CONFLICT(user_id,key) DO UPDATE SET value=excluded.value,updated_at=CURRENT_TIMESTAMP`, owner, key, string(wire))); err != nil {
		return nil, err
	}
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id=? AND key=?`, owner, key).Scan(&raw); err != nil {
		return nil, err
	}
	if raw != string(wire) {
		return nil, ErrCalendarCreateConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &rows[0], nil
}

func calendarMeetingCleanupSourceTx(ctx context.Context, tx *sql.Tx, owner, account, id string, guard func(*sql.Tx, string) error) (CalendarSource, [32]byte, error) {
	catalog, err := calendarDiscoverySnapshotTx(ctx, tx, owner, account, guard)
	if err != nil {
		return CalendarSource{}, [32]byte{}, err
	}
	for _, source := range catalog.catalog {
		if source.ID == id && !source.IsDeleted && (source.Provider == "gmail" || source.Provider == "outlook") {
			wire, err := json.Marshal(source)
			return source, sha256.Sum256(wire), err
		}
	}
	return CalendarSource{}, [32]byte{}, ErrCalendarSourceChanged
}

func calendarMeetingCleanupProof(row userCalendarCreateRecord, source CalendarSource, binding [32]byte) (userCalendarCreateAuthority, error) {
	var proof userCalendarCreateAuthority
	err := json.Unmarshal([]byte(row.Hash), &proof)
	nonce, parseErr := uuid.Parse(proof.Nonce)
	if err != nil || parseErr != nil || nonce == uuid.Nil || nonce.String() != proof.Nonce || proof.Format != 1 || proof.Binding != binding || binding == ([32]byte{}) || proof.Draft == "" || row.Source != source.ID {
		return proof, ErrCalendarCreateConflict
	}
	return proof, nil
}

func calendarMeetingCleanupDueTx(ctx context.Context, tx *sql.Tx, c *UserCalendarMeetingCleanupSnapshot) error {
	var due bool
	if c.source.Provider == "gmail" {
		query := `SELECT cleanup_pending=1 AND (conference_json<>'' OR created_at<datetime('now','-1 hour')) FROM calendar_meet_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
		if c.prune {
			query = `SELECT cleanup_pending=0 AND created_at<datetime('now','-7 days') FROM calendar_meet_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
		}
		if err := tx.QueryRowContext(ctx, query, c.source.UserID, c.source.ID, c.id).Scan(&due); err != nil {
			return err
		}
	} else {
		query := `SELECT state='abandoned' OR (state IN ('active','saving') AND updated_at<datetime('now','-1 hour')) FROM calendar_teams_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
		if c.prune {
			query = `SELECT state IN ('saved','cleaned') AND updated_at<datetime('now','-7 days') FROM calendar_teams_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
		}
		if err := tx.QueryRowContext(ctx, query, c.source.UserID, c.source.ID, c.id).Scan(&due); err != nil {
			return err
		}
	}
	if !due {
		return ErrCalendarCreateConflict
	}
	return nil
}

// bind copies the local account service and computes the original selected=true
// principal inside this same read transaction. No token/provider work is allowed.
func (db *DB) SnapshotUserCalendarMeetingCleanup(ctx context.Context, owner, account, source, id string, prune bool, guard func(*sql.Tx, string) error, bind func(*sql.Tx, CalendarSource) ([32]byte, error)) (*UserCalendarMeetingCleanupSnapshot, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id || guard == nil || bind == nil {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c := &UserCalendarMeetingCleanupSnapshot{id: id, prune: prune}
	c.source, c.sourceState, err = calendarMeetingCleanupSourceTx(ctx, tx, owner, account, source, guard)
	if err != nil {
		return nil, err
	}
	binding, err := bind(tx, c.source)
	if err != nil {
		return nil, err
	}
	c.request = calendarMeetingDraftRequest(source, c.source.Provider, id)
	c.reservation, err = calendarCreateRecordTx(ctx, tx, owner, c.request)
	if err != nil {
		return nil, err
	}
	proof, err := calendarMeetingCleanupProof(c.reservation, c.source, binding)
	if err != nil || proof.Draft != "meeting:"+c.source.Provider+":"+id || c.reservation.Result != (CalendarCreateRequest{}) {
		return nil, ErrCalendarCreateConflict
	}
	c.record, err = calendarMeetingDraftRecordTx(ctx, tx, c.source, id)
	if err != nil {
		return nil, err
	}
	if c.source.Provider == "gmail" && c.record.Google.RemoteID != CalendarMeetDraftRemoteID(owner, source, id) {
		return nil, ErrCalendarCreateConflict
	}
	if c.source.Provider == "outlook" && c.record.Teams.UsedBy != "" {
		if !strings.HasPrefix(c.record.Teams.UsedBy, "create:") {
			return nil, ErrCalendarCreateConflict
		}
		c.finalID = strings.TrimPrefix(c.record.Teams.UsedBy, "create:")
		if c.finalID == "" || strings.HasPrefix(c.finalID, "meeting:") {
			return nil, ErrCalendarCreateConflict
		}
		c.final, err = calendarCreateRecordTx(ctx, tx, owner, c.finalID)
		if err != nil {
			return nil, err
		}
		if _, err := calendarMeetingCleanupProof(c.final, c.source, binding); err != nil {
			return nil, err
		}
		if c.final.Result != (CalendarCreateRequest{}) && (c.final.Result.RemoteID != c.record.Teams.RemoteID || c.final.Result.EventID == "") {
			return nil, ErrCalendarCreateConflict
		}
	}
	if c.source.Provider == "outlook" && ((c.record.Teams.State == "active" && c.finalID != "") || (c.record.Teams.State == "saving" && (c.finalID == "" || c.record.Teams.RemoteID == "")) || (c.record.Teams.State == "saved" && (c.record.Teams.RemoteID == "" || c.final.Result == (CalendarCreateRequest{})))) {
		return nil, ErrCalendarCreateConflict
	}
	if err := calendarMeetingCleanupDueTx(ctx, tx, c); err != nil {
		return nil, err
	}
	if err := validateCalendarMeetingCleanupTx(ctx, tx, c, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

func validateCalendarMeetingCleanupTargetsTx(ctx context.Context, tx *sql.Tx, c *UserCalendarMeetingCleanupSnapshot, guard func(*sql.Tx, string) error) error {
	if c == nil || guard == nil || c.id == "" || c.request == "" {
		return ErrCalendarCreateConflict
	}
	_, state, err := calendarMeetingCleanupSourceTx(ctx, tx, c.source.UserID, c.source.AccountID, c.source.ID, guard)
	if err != nil {
		return err
	}
	if state != c.sourceState {
		return ErrCalendarSourceChanged
	}
	if c.finalID != "" {
		row, err := calendarCreateRecordTx(ctx, tx, c.source.UserID, c.finalID)
		if err != nil {
			return err
		}
		if row != c.final {
			return ErrCalendarCreateConflict
		}
	}
	return nil
}

func validateCalendarMeetingCleanupTx(ctx context.Context, tx *sql.Tx, c *UserCalendarMeetingCleanupSnapshot, guard func(*sql.Tx, string) error) error {
	if err := validateCalendarMeetingCleanupTargetsTx(ctx, tx, c, guard); err != nil {
		return err
	}
	row, err := calendarCreateRecordTx(ctx, tx, c.source.UserID, c.request)
	if err != nil {
		return err
	}
	if row != c.reservation {
		return ErrCalendarCreateConflict
	}
	record, err := calendarMeetingDraftRecordTx(ctx, tx, c.source, c.id)
	if err != nil {
		return err
	}
	if record != c.record {
		return ErrCalendarCreateConflict
	}
	return nil
}

func (db *DB) ValidateUserCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarMeetingCleanupTx(ctx, tx, c, guard); err != nil {
		return err
	}
	return tx.Commit()
}

// Expire a saving reservation only after native inspection under the account
// then source gates proved it is still the original private, guest-free draft.
// Saved requires the exact durable completed create, not a native marker prefix.
func (db *DB) PublishUserCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, transition CalendarMeetingDraftTransition, guard func(*sql.Tx, string) error) (*UserCalendarMeetingCleanupSnapshot, error) {
	if c == nil || c.prune {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateCalendarMeetingCleanupTx(ctx, tx, c, guard); err != nil {
		return nil, err
	}
	if err := calendarMeetingCleanupDueTx(ctx, tx, c); err != nil {
		return nil, err
	}
	next := *c
	var result sql.Result
	if c.source.Provider == "gmail" {
		if transition != CalendarMeetingDraftCleaned || !c.record.Cleanup {
			return nil, ErrCalendarCreateConflict
		}
		next.record.Cleanup = false
		result, err = tx.ExecContext(ctx, `UPDATE calendar_meet_drafts SET cleanup_pending=0 WHERE user_id=? AND source_id=? AND draft_id=?`, c.source.UserID, c.source.ID, c.id)
	} else {
		state := c.record.Teams.State
		switch transition {
		case CalendarMeetingDraftAbandon:
			if state != "active" || c.finalID != "" {
				return nil, ErrCalendarCreateConflict
			}
			next.record.Teams.State = "abandoned"
		case CalendarMeetingDraftExpire:
			if state != "saving" || c.finalID == "" || c.final.Result != (CalendarCreateRequest{}) {
				return nil, ErrCalendarCreateConflict
			}
			next.record.Teams.State = "abandoned"
		case CalendarMeetingDraftSaved:
			if state != "saving" || c.final.Result.RemoteID == "" || c.final.Result.EventID == "" || c.final.Result.RemoteID != c.record.Teams.RemoteID {
				return nil, ErrCalendarCreateConflict
			}
			next.record.Teams.State = "saved"
		case CalendarMeetingDraftCleaned:
			if state != "abandoned" || c.final.Result != (CalendarCreateRequest{}) {
				return nil, ErrCalendarCreateConflict
			}
			next.record.Teams.State = "cleaned"
		default:
			return nil, ErrCalendarCreateConflict
		}
		// Expiration retains the original age; terminal states start retention.
		if next.record.Teams.State == "saved" || next.record.Teams.State == "cleaned" {
			if err := tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&next.record.Timestamp); err != nil {
				return nil, err
			}
		}
		result, err = tx.ExecContext(ctx, `UPDATE calendar_teams_drafts SET state=?,updated_at=? WHERE user_id=? AND source_id=? AND draft_id=?`, next.record.Teams.State, next.record.Timestamp, c.source.UserID, c.source.ID, c.id)
	}
	if err := teamsDraftChanged(result, err); err != nil {
		return nil, err
	}
	if err := validateCalendarMeetingCleanupTx(ctx, tx, &next, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

// Delete only this terminal draft and its own exact meeting reservation. Keep
// any event-create reservation, including a pending original finalized save.
func (db *DB) PruneUserCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, guard func(*sql.Tx, string) error) error {
	if c == nil || !c.prune {
		return ErrCalendarCreateConflict
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarMeetingCleanupTx(ctx, tx, c, guard); err != nil {
		return err
	}
	if err := calendarMeetingCleanupDueTx(ctx, tx, c); err != nil {
		return err
	}
	query := `DELETE FROM calendar_meet_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
	if c.source.Provider == "outlook" {
		query = `DELETE FROM calendar_teams_drafts WHERE user_id=? AND source_id=? AND draft_id=?`
	}
	if err := teamsDraftChanged(tx.ExecContext(ctx, query, c.source.UserID, c.source.ID, c.id)); err != nil {
		return err
	}
	if err := teamsDraftChanged(tx.ExecContext(ctx, `DELETE FROM calendar_create_requests WHERE user_id=? AND request_id=? AND source_id=? AND request_hash=?`, c.source.UserID, c.request, c.source.ID, c.reservation.Hash)); err != nil {
		return err
	}
	if err := validateCalendarMeetingCleanupTargetsTx(ctx, tx, c, guard); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_create_requests WHERE user_id=? AND request_id=?`, c.source.UserID, c.request).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return ErrCalendarCreateConflict
	}
	_, err = calendarMeetingDraftRecordTx(ctx, tx, c.source, c.id)
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return err
		}
		return ErrCalendarCreateConflict
	}
	return tx.Commit()
}
