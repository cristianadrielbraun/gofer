package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Meeting previews create provider resources too. Their namespace uses the same
// durable principal/nonce authority as event creates, independent of final IDs.
func calendarMeetingDraftRequest(source, provider, id string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + source + "\x00" + id))
	return "meeting:" + hex.EncodeToString(sum[:])
}

func CalendarMeetDraftRemoteID(owner, source, id string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + source + "\x00" + id))
	return "gofermeetdraft" + hex.EncodeToString(sum[:])
}

type calendarMeetingDraftRecord struct {
	Google    CalendarMeetDraft
	Teams     CalendarTeamsDraft
	Timestamp string
	Cleanup   bool
}

type UserCalendarMeetingDraftSnapshot struct {
	claim  *UserCalendarCreateClaim
	record calendarMeetingDraftRecord
}

// Candidates are scheduling data, not authority to delete a native resource or
// update a draft. The cleanup consumer must reacquire an exact private claim.
type CalendarMeetingCleanupCandidate struct {
	SourceID, DraftID, Provider string
	Prune                       bool
}

// Read one already-discovered account. Unlike legacy cleanup discovery, this
// does not expire active drafts as a side effect of listing work. A source may
// be deselected after preparing a conference; it still needs temporary cleanup.
func (db *DB) ListAccountCalendarMeetingCleanup(ctx context.Context, owner, account string, limit int, guard func(*sql.Tx, string) error) ([]CalendarMeetingCleanupCandidate, error) {
	if limit < 1 || limit > 16 || guard == nil {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	result, err := calendarMeetingCleanupCandidatesTx(ctx, tx, owner, account, limit, nil)
	if err != nil {
		return nil, err
	}
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func calendarMeetingCleanupCandidatesTx(ctx context.Context, tx *sql.Tx, owner, account string, limit int, after *CalendarMeetingCleanupCandidate) ([]CalendarMeetingCleanupCandidate, error) {
	query := `WITH owned_sources AS (
 SELECT id,user_id,provider FROM calendar_sources
 WHERE user_id=? AND account_id=? AND is_deleted=0 AND provider IN ('gmail','outlook')
)
SELECT source_id,draft_id,provider,prune FROM (
 SELECT d.source_id,d.draft_id,s.provider,d.cleanup_pending=0 AS prune,d.created_at AS due_at
 FROM calendar_meet_drafts d JOIN owned_sources s ON s.id=d.source_id AND s.user_id=d.user_id AND s.provider='gmail'
 WHERE (d.cleanup_pending=1 AND (d.conference_json<>'' OR d.created_at<datetime('now','-1 hour')))
    OR (d.cleanup_pending=0 AND d.created_at<datetime('now','-7 days'))
 UNION ALL
 SELECT d.source_id,d.draft_id,s.provider,d.state IN ('saved','cleaned') AS prune,d.updated_at AS due_at
 FROM calendar_teams_drafts d JOIN owned_sources s ON s.id=d.source_id AND s.user_id=d.user_id AND s.provider='outlook'
 WHERE d.state='abandoned'
    OR (d.state IN ('active','saving') AND d.updated_at<datetime('now','-1 hour'))
    OR (d.state IN ('saved','cleaned') AND d.updated_at<datetime('now','-7 days'))
)`
	args := []any{owner, account}
	if after == nil {
		query += ` ORDER BY due_at,provider,source_id,draft_id LIMIT ?`
	} else {
		// Wrap once after the previous candidate. Stable identity ordering
		// survives state/age changes without permanently hiding failed work.
		query += ` ORDER BY (provider,source_id,draft_id)<=(?,?,?),provider,source_id,draft_id LIMIT ?`
		args = append(args, after.Provider, after.SourceID, after.DraftID)
	}
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var result []CalendarMeetingCleanupCandidate
	for rows.Next() {
		var candidate CalendarMeetingCleanupCandidate
		if err := rows.Scan(&candidate.SourceID, &candidate.DraftID, &candidate.Provider, &candidate.Prune); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, candidate)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *UserCalendarMeetingDraftSnapshot) Google() CalendarMeetDraft { return s.record.Google }
func (s *UserCalendarMeetingDraftSnapshot) Teams() CalendarTeamsDraft { return s.record.Teams }

func calendarMeetingDraftRecordTx(ctx context.Context, tx *sql.Tx, source CalendarSource, id string) (calendarMeetingDraftRecord, error) {
	var r calendarMeetingDraftRecord
	if source.Provider == "gmail" {
		err := tx.QueryRowContext(ctx, `SELECT user_id,source_id,draft_id,remote_id,conference_json,used_by,CAST(created_at AS TEXT),cleanup_pending FROM calendar_meet_drafts WHERE user_id=? AND source_id=? AND draft_id=?`, source.UserID, source.ID, id).Scan(&r.Google.UserID, &r.Google.SourceID, &r.Google.DraftID, &r.Google.RemoteID, &r.Google.ConferenceJSON, &r.Google.UsedBy, &r.Timestamp, &r.Cleanup)
		return r, err
	}
	if source.Provider == "outlook" {
		err := tx.QueryRowContext(ctx, `SELECT user_id,source_id,draft_id,remote_id,meeting_json,used_by,state,CAST(updated_at AS TEXT) FROM calendar_teams_drafts WHERE user_id=? AND source_id=? AND draft_id=?`, source.UserID, source.ID, id).Scan(&r.Teams.UserID, &r.Teams.SourceID, &r.Teams.DraftID, &r.Teams.RemoteID, &r.Teams.MeetingJSON, &r.Teams.UsedBy, &r.Teams.State, &r.Timestamp)
		return r, err
	}
	return r, ErrCalendarCreateConflict
}

func (s *UserCalendarMeetingDraftSnapshot) id() string {
	if s.claim.source.source.Provider == "gmail" {
		return s.record.Google.DraftID
	}
	return s.record.Teams.DraftID
}

func validateCalendarMeetingDraftTx(ctx context.Context, tx *sql.Tx, s *UserCalendarMeetingDraftSnapshot, guard func(*sql.Tx, string) error) error {
	if s == nil || s.claim == nil || s.claim.source == nil || guard == nil {
		return ErrCalendarCreateConflict
	}
	if err := validateUserCalendarSourceTx(ctx, tx, s.claim.source, guard); err != nil {
		return err
	}
	if err := calendarCreateClaimMatchesTx(ctx, tx, s.claim); err != nil {
		return err
	}
	row, err := calendarMeetingDraftRecordTx(ctx, tx, s.claim.source.source, s.id())
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarCreateConflict
	}
	if err != nil {
		return err
	}
	if row != s.record {
		return ErrCalendarCreateConflict
	}
	return nil
}

// The principal reservation and temporary draft publish in one transaction.
// Existing unbound legacy drafts must be bound by offline migration, not adopted
// by whichever account happens to occupy their source at the time of a retry.
func (db *DB) BeginUserCalendarMeetingDraft(ctx context.Context, source *UserCalendarSourceSnapshot, id string, binding [32]byte, guard func(*sql.Tx, string) error) (*UserCalendarMeetingDraftSnapshot, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id || source == nil || (source.source.Provider != "gmail" && source.source.Provider != "outlook") || guard == nil {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	request := calendarMeetingDraftRequest(source.source.ID, source.source.Provider, id)
	claim, inserted, err := beginUserCalendarCreateTx(ctx, tx, source, request, "meeting:"+source.source.Provider+":"+id, binding, guard)
	if err != nil {
		return nil, err
	}
	if claim.record.Result != (CalendarCreateRequest{}) {
		return nil, ErrCalendarCreateConflict
	}
	var created string
	if inserted {
		if err := tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&created); err != nil {
			return nil, err
		}
		var result sql.Result
		if source.source.Provider == "gmail" {
			result, err = tx.ExecContext(ctx, `INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,created_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, source.source.UserID, source.source.ID, id, CalendarMeetDraftRemoteID(source.source.UserID, source.source.ID, id), created)
		} else {
			result, err = tx.ExecContext(ctx, `INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,updated_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, source.source.UserID, source.source.ID, id, created)
		}
		if err := teamsDraftChanged(result, err); err != nil {
			return nil, err
		}
	}
	record, err := calendarMeetingDraftRecordTx(ctx, tx, source.source, id)
	if err != nil {
		return nil, err
	}
	if inserted && record.Timestamp != created {
		return nil, ErrCalendarCreateConflict
	}
	if inserted && ((source.source.Provider == "gmail" && (record.Google != (CalendarMeetDraft{UserID: source.source.UserID, SourceID: source.source.ID, DraftID: id, RemoteID: CalendarMeetDraftRemoteID(source.source.UserID, source.source.ID, id)}) || !record.Cleanup)) || (source.source.Provider == "outlook" && record.Teams != (CalendarTeamsDraft{UserID: source.source.UserID, SourceID: source.source.ID, DraftID: id, State: "active"}))) {
		return nil, ErrCalendarCreateConflict
	}
	snapshot := &UserCalendarMeetingDraftSnapshot{claim: claim, record: record}
	if err := validateCalendarMeetingDraftTx(ctx, tx, snapshot, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (db *DB) SnapshotUserCalendarMeetingDraft(ctx context.Context, source *UserCalendarSourceSnapshot, id string, binding [32]byte, guard func(*sql.Tx, string) error) (*UserCalendarMeetingDraftSnapshot, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id || source == nil || guard == nil || binding == ([32]byte{}) || (source.source.Provider != "gmail" && source.source.Provider != "outlook") {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return nil, err
	}
	request := calendarMeetingDraftRequest(source.source.ID, source.source.Provider, id)
	row, err := calendarCreateRecordTx(ctx, tx, source.source.UserID, request)
	if err != nil {
		return nil, err
	}
	var proof userCalendarCreateAuthority
	err = json.Unmarshal([]byte(row.Hash), &proof)
	nonce, nonceErr := uuid.Parse(proof.Nonce)
	if err != nil || nonceErr != nil || nonce == uuid.Nil || nonce.String() != proof.Nonce || proof.Format != 1 || proof.Binding != binding || proof.Draft != "meeting:"+source.source.Provider+":"+id || row.Source != source.source.ID || row.Result != (CalendarCreateRequest{}) {
		return nil, ErrCalendarCreateConflict
	}
	record, err := calendarMeetingDraftRecordTx(ctx, tx, source.source, id)
	if err != nil {
		return nil, err
	}
	snapshot := &UserCalendarMeetingDraftSnapshot{claim: &UserCalendarCreateClaim{source: source, request: request, record: row}, record: record}
	if err := validateCalendarMeetingDraftTx(ctx, tx, snapshot, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (db *DB) ValidateUserCalendarMeetingDraft(ctx context.Context, s *UserCalendarMeetingDraftSnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarMeetingDraftTx(ctx, tx, s, guard); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarMeetingDraftTransition string

const (
	CalendarMeetingDraftRemote     CalendarMeetingDraftTransition = "remote"
	CalendarMeetingDraftConference CalendarMeetingDraftTransition = "conference"
	CalendarMeetingDraftBind       CalendarMeetingDraftTransition = "bind"
	CalendarMeetingDraftAbandon    CalendarMeetingDraftTransition = "abandon"
	CalendarMeetingDraftExpire     CalendarMeetingDraftTransition = "expire"
	CalendarMeetingDraftSaved      CalendarMeetingDraftTransition = "saved"
	CalendarMeetingDraftCleaned    CalendarMeetingDraftTransition = "cleaned"
)

// All transitions verify the exact private row and original principal after the
// writer wait, then verify the committed result. No native I/O holds this TX.
func (db *DB) PublishUserCalendarMeetingDraft(ctx context.Context, s *UserCalendarMeetingDraftSnapshot, transition CalendarMeetingDraftTransition, value string, guard func(*sql.Tx, string) error) (*UserCalendarMeetingDraftSnapshot, error) {
	if transition == CalendarMeetingDraftBind {
		return nil, ErrCalendarCreateConflict
	}
	return db.publishUserCalendarMeetingDraft(ctx, s, transition, value, guard, nil)
}

func (db *DB) BindUserCalendarMeetingDraft(ctx context.Context, s *UserCalendarMeetingDraftSnapshot, create *UserCalendarCreateClaim, event *UserCalendarEventSnapshot, guard func(*sql.Tx, string) error) (*UserCalendarMeetingDraftSnapshot, error) {
	if s == nil || s.claim == nil || s.claim.source == nil || (create == nil) == (event == nil) {
		return nil, ErrCalendarCreateConflict
	}
	var target string
	targetGuard := func(tx *sql.Tx) error {
		if create != nil {
			if create.source == nil || create.source.source.ID != s.claim.source.source.ID || create.source.source.UserID != s.claim.source.source.UserID || create.source.state != s.claim.source.state || create.record.Result != (CalendarCreateRequest{}) {
				return ErrCalendarCreateConflict
			}
			if err := validateUserCalendarSourceTx(ctx, tx, create.source, guard); err != nil {
				return err
			}
			return calendarCreateClaimMatchesTx(ctx, tx, create)
		}
		if event.source.ID != s.claim.source.source.ID || event.source.UserID != s.claim.source.source.UserID || event.sourceState != s.claim.source.state {
			return ErrCalendarEventChanged
		}
		return validateUserCalendarEventTx(ctx, tx, event, guard)
	}
	if create != nil {
		target = "create:" + create.request
	} else {
		target = "event:" + event.event.ID
	}
	return db.publishUserCalendarMeetingDraft(ctx, s, CalendarMeetingDraftBind, target, guard, targetGuard)
}

// Finish only a prepared meeting bound to this exact completed create. A
// copied result or remote ID cannot prove which request accepted the meeting.
func (db *DB) FinishUserCalendarTeamsCreate(ctx context.Context, s *UserCalendarMeetingDraftSnapshot, create *UserCalendarCreateClaim, guard func(*sql.Tx, string) error) (*UserCalendarMeetingDraftSnapshot, error) {
	if s == nil || s.claim == nil || s.claim.source == nil || create == nil || create.source == nil || guard == nil {
		return nil, ErrCalendarCreateConflict
	}
	source := s.claim.source
	if source.source.Provider != "outlook" || create.source.source.ID != source.source.ID || create.source.source.UserID != source.source.UserID || create.source.state != source.state || create.record.Result.RemoteID == "" || create.record.Result.EventID == "" || s.record.Teams.RemoteID != create.record.Result.RemoteID || s.record.Teams.UsedBy != "create:"+create.request || (s.record.Teams.State != "saving" && s.record.Teams.State != "saved") {
		return nil, ErrCalendarCreateConflict
	}
	targetGuard := func(tx *sql.Tx) error {
		if err := validateUserCalendarSourceTx(ctx, tx, create.source, guard); err != nil {
			return err
		}
		return calendarCreateClaimMatchesTx(ctx, tx, create)
	}
	return db.publishUserCalendarMeetingDraft(ctx, s, CalendarMeetingDraftSaved, "", guard, targetGuard)
}

func (db *DB) publishUserCalendarMeetingDraft(ctx context.Context, s *UserCalendarMeetingDraftSnapshot, transition CalendarMeetingDraftTransition, value string, guard func(*sql.Tx, string) error, targetGuard func(*sql.Tx) error) (*UserCalendarMeetingDraftSnapshot, error) {
	if s == nil || s.claim == nil || s.claim.source == nil || guard == nil || len(value) > 256<<10 || strings.ContainsRune(value, 0) {
		return nil, ErrCalendarCreateConflict
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateCalendarMeetingDraftTx(ctx, tx, s, guard); err != nil {
		return nil, err
	}
	if targetGuard != nil {
		if err := targetGuard(tx); err != nil {
			return nil, err
		}
	}
	next := *s
	source := s.claim.source.source
	var result sql.Result
	if source.Provider == "gmail" {
		d := &next.record.Google
		switch transition {
		case CalendarMeetingDraftConference:
			if value == "" || d.UsedBy != "" || (d.ConferenceJSON != "" && d.ConferenceJSON != value) {
				return nil, ErrCalendarCreateConflict
			}
			d.ConferenceJSON = value
		case CalendarMeetingDraftBind:
			if value == "" || len(value) > 4096 || d.ConferenceJSON == "" || (d.UsedBy != "" && d.UsedBy != value) {
				return nil, ErrCalendarCreateConflict
			}
			d.UsedBy = value
		case CalendarMeetingDraftCleaned:
			if value != "" {
				return nil, ErrCalendarCreateConflict
			}
			next.record.Cleanup = false
		default:
			return nil, ErrCalendarCreateConflict
		}
		result, err = tx.ExecContext(ctx, `UPDATE calendar_meet_drafts SET conference_json=?,used_by=?,cleanup_pending=? WHERE user_id=? AND source_id=? AND draft_id=?`, d.ConferenceJSON, d.UsedBy, next.record.Cleanup, source.UserID, source.ID, d.DraftID)
	} else if source.Provider == "outlook" {
		d := &next.record.Teams
		condition := ""
		switch transition {
		case CalendarMeetingDraftRemote:
			if value == "" || len(value) > 4096 || d.State != "active" || d.UsedBy != "" || (d.RemoteID != "" && d.RemoteID != value) {
				return nil, ErrCalendarCreateConflict
			}
			d.RemoteID = value
		case CalendarMeetingDraftConference:
			if value == "" || d.RemoteID == "" || d.State != "active" || d.UsedBy != "" || (d.MeetingJSON != "" && d.MeetingJSON != value) {
				return nil, ErrCalendarCreateConflict
			}
			d.MeetingJSON = value
		case CalendarMeetingDraftBind:
			if value == "" || len(value) > 4096 || d.RemoteID == "" || d.MeetingJSON == "" {
				return nil, ErrCalendarCreateConflict
			}
			if d.State == "active" && d.UsedBy == "" {
				condition = " AND updated_at>=datetime('now','-1 hour')"
			} else if (d.State != "saving" && d.State != "saved") || d.UsedBy != value {
				return nil, ErrCalendarCreateConflict
			}
			d.UsedBy = value
			d.State = "saving"
		case CalendarMeetingDraftAbandon:
			if value != "" {
				return nil, ErrCalendarCreateConflict
			}
			if d.State == "active" && d.UsedBy == "" {
				d.State = "abandoned"
			} else {
				// A late dialog close must not renew the cleanup deadline of a saving,
				// saved or already abandoned reservation.
				if err := validateCalendarMeetingDraftTx(ctx, tx, &next, guard); err != nil {
					return nil, err
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return &next, nil
			}
		case CalendarMeetingDraftExpire:
			if value != "" || d.State != "saving" {
				return nil, ErrCalendarCreateConflict
			}
			condition = " AND updated_at<datetime('now','-1 hour')"
			d.State = "abandoned"
		case CalendarMeetingDraftSaved:
			if value != "" || (d.State != "saving" && d.State != "saved") {
				return nil, ErrCalendarCreateConflict
			}
			if d.State == "saved" {
				// Idempotent recovery retains the original cleanup timestamp and
				// checks both private claims under this writer transaction.
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return &next, nil
			}
			d.State = "saved"
		case CalendarMeetingDraftCleaned:
			if value != "" || d.State != "abandoned" {
				return nil, ErrCalendarCreateConflict
			}
			d.State = "cleaned"
		default:
			return nil, ErrCalendarCreateConflict
		}
		if err := tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&next.record.Timestamp); err != nil {
			return nil, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE calendar_teams_drafts SET remote_id=?,meeting_json=?,used_by=?,state=?,updated_at=? WHERE user_id=? AND source_id=? AND draft_id=?`+condition, d.RemoteID, d.MeetingJSON, d.UsedBy, d.State, next.record.Timestamp, source.UserID, source.ID, d.DraftID)
	} else {
		return nil, ErrCalendarCreateConflict
	}
	if err := teamsDraftChanged(result, err); err != nil {
		return nil, err
	}
	if err := validateCalendarMeetingDraftTx(ctx, tx, &next, guard); err != nil {
		return nil, err
	}
	if targetGuard != nil {
		if err := targetGuard(tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}
