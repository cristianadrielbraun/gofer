package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const calendarReplySchema = `CREATE TABLE IF NOT EXISTS calendar_reply_jobs (
	id TEXT PRIMARY KEY REFERENCES outgoing_sends(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
	resource_id TEXT NOT NULL, remote_id TEXT NOT NULL, version TEXT NOT NULL,
	response TEXT NOT NULL CHECK(response IN ('accepted','tentative','declined')),
	payload TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','complete','conflict','canceled','dismissed')),
	attempted_at DATETIME,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_reply_pending ON calendar_reply_jobs(user_id,source_id,resource_id) WHERE state = 'pending';`

func migrateV99ToV100(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(calendarReplySchema); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 100); err != nil {
		return err
	}
	return tx.Commit()
}

// Keep this upgrade separate: an active development instance may already have
// created the initial reply queue before recovery controls were added.
func migrateV100ToV101(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 101 {
		return nil
	}
	var definition string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='calendar_reply_jobs'`).Scan(&definition); err != nil {
		return err
	}
	// Upgrades from older schemas create the current table in v100 already.
	// Only an earlier v100 table needs rebuilding; avoid needless table swaps.
	if strings.Contains(definition, "'dismissed'") && strings.Contains(definition, "attempted_at") {
		if err := markSchemaVersion(tx, 101); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(`CREATE TABLE calendar_reply_jobs_next (
		id TEXT PRIMARY KEY REFERENCES outgoing_sends(id) ON DELETE CASCADE,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
		resource_id TEXT NOT NULL,remote_id TEXT NOT NULL,version TEXT NOT NULL,
		response TEXT NOT NULL CHECK(response IN ('accepted','tentative','declined')),
		payload TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','complete','conflict','canceled','dismissed')),
		attempted_at DATETIME,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO calendar_reply_jobs_next(id,user_id,source_id,resource_id,remote_id,version,response,payload,state,created_at)
		SELECT id,user_id,source_id,resource_id,remote_id,version,response,payload,state,created_at FROM calendar_reply_jobs;
		DROP TABLE calendar_reply_jobs;
		ALTER TABLE calendar_reply_jobs_next RENAME TO calendar_reply_jobs;
		CREATE UNIQUE INDEX idx_calendar_reply_pending ON calendar_reply_jobs(user_id,source_id,resource_id) WHERE state='pending';`); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 101); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarReplyJob struct {
	ID, UserID, SourceID, ResourceID, RemoteID, Version, Response, Payload, State string
	SendStatus                                                                    string
}

// Commit the notification and its calendar follow-up together. A crash cannot
// leave a deliverable email without the durable calendar work it represents.
func (db *DB) QueueCalendarReply(ctx context.Context, job CalendarReplyJob, input QueueOutgoingSendInput) (string, error) {
	if job.UserID == "" || job.SourceID == "" || job.ResourceID == "" || job.RemoteID == "" || job.Version == "" || !json.Valid([]byte(job.Payload)) || input.EnvelopeFrom == "" || len(input.EnvelopeRecipients) != 1 || len(input.MIMEData) == 0 || !json.Valid(input.MessageJSON) {
		return "", fmt.Errorf("invalid calendar reply")
	}
	job.ID = uuid.NewString()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var accountID string
	if err := tx.QueryRowContext(ctx, `SELECT source.account_id FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		WHERE source.id = ? AND source.user_id = ? AND source.is_selected = 1 AND source.is_deleted = 0
		AND source.provider = 'caldav' AND COALESCE(account.is_deleting,0) = 0`, job.SourceID, job.UserID).Scan(&accountID); err != nil {
		return "", err
	}
	if accountID != input.AccountID {
		return "", ErrCalendarUpdateConflict
	}
	recipients, _ := json.Marshal(input.EnvelopeRecipients)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,envelope_recipients,mime_data,message_json,send_after,next_attempt_at,status)
		VALUES(?,?,?,?,?,?,?,?,?,'pending')`, job.ID, input.AccountID, input.Transport, input.EnvelopeFrom, string(recipients), input.MIMEData, string(input.MessageJSON), now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_reply_jobs(id,user_id,source_id,resource_id,remote_id,version,response,payload) VALUES(?,?,?,?,?,?,?,?)`, job.ID, job.UserID, job.SourceID, job.ResourceID, job.RemoteID, job.Version, job.Response, job.Payload); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return job.ID, nil
}

const calendarReplySelect = `SELECT j.id,j.user_id,j.source_id,j.resource_id,j.remote_id,j.version,j.response,j.payload,j.state,s.status FROM calendar_reply_jobs j JOIN outgoing_sends s ON s.id=j.id `

func scanCalendarReply(row interface{ Scan(...any) error }) (job CalendarReplyJob, err error) {
	err = row.Scan(&job.ID, &job.UserID, &job.SourceID, &job.ResourceID, &job.RemoteID, &job.Version, &job.Response, &job.Payload, &job.State, &job.SendStatus)
	return
}

func (db *DB) GetCalendarReply(ctx context.Context, userID, id string) (CalendarReplyJob, error) {
	return scanCalendarReply(db.Read().QueryRowContext(ctx, calendarReplySelect+`WHERE j.user_id=? AND j.id=?`, userID, id))
}

func (db *DB) PendingCalendarReply(ctx context.Context, userID, sourceID, resourceID string) (CalendarReplyJob, error) {
	return scanCalendarReply(db.Read().QueryRowContext(ctx, calendarReplySelect+`WHERE j.user_id=? AND j.source_id=? AND j.resource_id=? AND j.state='pending'`, userID, sourceID, resourceID))
}

func (db *DB) UnresolvedCalendarReply(ctx context.Context, userID, sourceID, resourceID string) (CalendarReplyJob, error) {
	return scanCalendarReply(db.Read().QueryRowContext(ctx, calendarReplySelect+`WHERE j.user_id=? AND j.source_id=? AND j.resource_id=? AND j.state IN ('pending','conflict') ORDER BY j.rowid DESC LIMIT 1`, userID, sourceID, resourceID))
}

func (db *DB) CancelCalendarReply(ctx context.Context, userID, id string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status='canceled',last_error='Canceled by the user',locked_at=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('pending','failed') AND EXISTS(SELECT 1 FROM calendar_reply_jobs j WHERE j.id=outgoing_sends.id AND j.user_id=? AND j.state='pending')`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrOutgoingSendNotCancelable
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_response_requests WHERE user_id=? AND EXISTS(SELECT 1 FROM calendar_reply_jobs j WHERE j.id=? AND j.user_id=calendar_response_requests.user_id AND j.source_id=calendar_response_requests.source_id AND j.remote_id=calendar_response_requests.remote_id AND j.version=calendar_response_requests.version)`, userID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET state='canceled' WHERE id=? AND user_id=?`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// Only an explicit user confirmation can resolve ambiguous SMTP delivery.
// Do not append another Sent copy: the user has confirmed the existing send.
func (db *DB) ConfirmCalendarReplySent(ctx context.Context, userID, id string) error {
	result, err := db.Write().ExecContext(ctx, `UPDATE outgoing_sends SET status='sent',last_error='',locked_at=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status='ambiguous' AND EXISTS(SELECT 1 FROM calendar_reply_jobs j WHERE j.id=outgoing_sends.id AND j.user_id=? AND j.state='pending')`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrOutgoingSendNotRetryable
	}
	return nil
}

func (db *DB) DismissCalendarReplyConflict(ctx context.Context, userID, id string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_reply_jobs SET state='dismissed' WHERE id=? AND user_id=? AND state='conflict'`, id, userID)
	return err
}

func (db *DB) CalendarReplyForSend(ctx context.Context, id string) (CalendarReplyJob, error) {
	return scanCalendarReply(db.Read().QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=?`, id))
}

func (db *DB) ListCalendarReplyFollowups(ctx context.Context) ([]CalendarReplyJob, error) {
	rows, err := db.Read().QueryContext(ctx, calendarReplySelect+`WHERE j.state='pending' AND s.status='sent' ORDER BY j.attempted_at,j.created_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CalendarReplyJob
	for rows.Next() {
		job, err := scanCalendarReply(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (db *DB) AttemptCalendarReply(ctx context.Context, userID, id string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_reply_jobs SET attempted_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND state='pending'`, id, userID)
	return err
}

func (db *DB) FinishCalendarReply(ctx context.Context, userID, id, state string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_reply_jobs SET state=? WHERE id=? AND user_id=? AND state='pending'`, state, id, userID)
	return err
}
