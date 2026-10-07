package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type UserProviderDraftState struct {
	AccountID, DraftKey, FolderID, Provider, MailboxSubject  string
	LocalMessageID                                           int64
	LocalRevision, RemoteID, RemoteMessageID, RemoteRevision string
}

type UserProviderDraftOperation struct {
	ID                              int64
	State                           UserProviderDraftState
	Kind, RevisionToken, Status     string
	MIMEData                        []byte
	MessageDate                     time.Time
	AttemptCount                    int
	CreateAttempted                 bool
	CandidateID, CandidateMessageID string
}

type QueueUserProviderDraftInput struct {
	State         UserProviderDraftState
	RevisionToken string
	MIMEData      []byte
	MessageDate   time.Time
}

func ensureUserProviderDraftSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS gofer_provider_draft_states (
		 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		 draft_key TEXT NOT NULL, provider TEXT NOT NULL CHECK(provider IN ('gmail','outlook')),
		 mailbox_subject TEXT NOT NULL, local_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
		 folder_id TEXT NOT NULL, local_revision TEXT NOT NULL DEFAULT '',
		 remote_id TEXT NOT NULL DEFAULT '', remote_message_id TEXT NOT NULL DEFAULT '',remote_revision TEXT NOT NULL DEFAULT '',
		 PRIMARY KEY(account_id,draft_key))`,
		`CREATE TABLE IF NOT EXISTS gofer_provider_draft_operations (
		 id INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, draft_key TEXT NOT NULL,
		 kind TEXT NOT NULL CHECK(kind IN ('upsert','delete')), revision_token TEXT NOT NULL DEFAULT '',
		 mime_data BLOB, message_date DATETIME, status TEXT NOT NULL DEFAULT 'pending',
		 attempt_count INTEGER NOT NULL DEFAULT 0,create_attempted INTEGER NOT NULL DEFAULT 0,
		 candidate_id TEXT NOT NULL DEFAULT '',candidate_message_id TEXT NOT NULL DEFAULT '',
		 last_error TEXT NOT NULL DEFAULT '',next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		 FOREIGN KEY(account_id,draft_key) REFERENCES gofer_provider_draft_states(account_id,draft_key) ON DELETE CASCADE)`,
		`CREATE INDEX IF NOT EXISTS idx_gofer_provider_draft_due ON gofer_provider_draft_operations(account_id,next_attempt_at,id)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func queueUserProviderDraftTx(ctx context.Context, tx *sql.Tx, input QueueUserProviderDraftInput, kind string) error {
	s := input.State
	if s.AccountID == "" || s.DraftKey == "" || s.FolderID == "" || (s.Provider != "gmail" && s.Provider != "outlook") || (kind == "upsert" && (input.RevisionToken == "" || len(input.MIMEData) == 0)) {
		return errors.New("provider draft payload is incomplete")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts a JOIN users u ON u.id=a.user_id WHERE a.id=? AND a.provider=? AND a.auth_method='oauth2' AND COALESCE(a.provider_account_id,'')=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active')`, s.AccountID, s.Provider, s.MailboxSubject).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrMessageMutationSuperseded
	}
	var localID any
	if s.LocalMessageID > 0 {
		localID = s.LocalMessageID
	}
	localRevision := input.RevisionToken
	if kind == "delete" {
		localRevision = ""
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO gofer_provider_draft_states(account_id,draft_key,provider,mailbox_subject,local_message_id,folder_id,local_revision,remote_id,remote_message_id,remote_revision)
	 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id,draft_key) DO UPDATE SET local_message_id=excluded.local_message_id,folder_id=excluded.folder_id,local_revision=excluded.local_revision,
	 remote_id=CASE WHEN gofer_provider_draft_states.remote_id='' THEN excluded.remote_id ELSE gofer_provider_draft_states.remote_id END,
	 remote_message_id=CASE WHEN gofer_provider_draft_states.remote_id='' AND gofer_provider_draft_states.remote_message_id='' THEN excluded.remote_message_id ELSE gofer_provider_draft_states.remote_message_id END
	 WHERE gofer_provider_draft_states.provider=excluded.provider AND gofer_provider_draft_states.mailbox_subject=excluded.mailbox_subject`, s.AccountID, s.DraftKey, s.Provider, s.MailboxSubject, localID, s.FolderID, localRevision, s.RemoteID, s.RemoteMessageID, s.RemoteRevision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrMessageMutationSuperseded
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM gofer_provider_draft_operations WHERE account_id=? AND draft_key=? AND status IN ('pending','failed','blocked') AND create_attempted=0 AND candidate_id='' ORDER BY id LIMIT 1`, s.AccountID, s.DraftKey).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var date any
	if kind == "upsert" {
		if input.MessageDate.IsZero() {
			input.MessageDate = time.Now().UTC()
		}
		date = input.MessageDate.UTC()
	}
	if id == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO gofer_provider_draft_operations(account_id,draft_key,kind,revision_token,mime_data,message_date) VALUES(?,?,?,?,?,?)`, s.AccountID, s.DraftKey, kind, input.RevisionToken, input.MIMEData, date)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE gofer_provider_draft_operations SET kind=?,revision_token=?,mime_data=?,message_date=?,status='pending',attempt_count=0,last_error='',next_attempt_at=CURRENT_TIMESTAMP WHERE id=?`, kind, input.RevisionToken, input.MIMEData, date, id)
	}
	return err
}

const userProviderDraftSelect = `SELECT o.id,s.account_id,s.draft_key,s.folder_id,s.provider,s.mailbox_subject,COALESCE(s.local_message_id,0),s.local_revision,s.remote_id,s.remote_message_id,s.remote_revision,
 o.kind,o.revision_token,o.mime_data,o.message_date,o.status,o.attempt_count,o.create_attempted,o.candidate_id,o.candidate_message_id
 FROM gofer_provider_draft_operations o JOIN gofer_provider_draft_states s ON s.account_id=o.account_id AND s.draft_key=o.draft_key`

func scanUserProviderDraft(row rowScanner) (UserProviderDraftOperation, error) {
	var op UserProviderDraftOperation
	var date sqliteNullTime
	err := row.Scan(&op.ID, &op.State.AccountID, &op.State.DraftKey, &op.State.FolderID, &op.State.Provider, &op.State.MailboxSubject, &op.State.LocalMessageID, &op.State.LocalRevision, &op.State.RemoteID, &op.State.RemoteMessageID, &op.State.RemoteRevision,
		&op.Kind, &op.RevisionToken, &op.MIMEData, &date, &op.Status, &op.AttemptCount, &op.CreateAttempted, &op.CandidateID, &op.CandidateMessageID)
	op.MessageDate = date.Time
	return op, err
}

func (db *DB) ClaimUserProviderDraft(ctx context.Context, accountID string, now time.Time) (*UserProviderDraftOperation, error) {
	if !db.userMailDelivery {
		return nil, errors.New("provider drafts require a marked user store")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	op, err := scanUserProviderDraft(tx.QueryRowContext(ctx, userProviderDraftSelect+` WHERE o.account_id=? AND o.status IN ('pending','failed','ambiguous') AND julianday(replace(o.next_attempt_at,' +0000 UTC',''))<=julianday(?)
	 AND NOT EXISTS(SELECT 1 FROM gofer_provider_draft_operations older WHERE older.account_id=o.account_id AND older.draft_key=o.draft_key AND older.id<o.id)
	 AND EXISTS(SELECT 1 FROM accounts a JOIN users u ON u.id=a.user_id WHERE a.id=s.account_id AND a.provider=s.provider AND a.auth_method='oauth2' AND COALESCE(a.provider_account_id,'')=s.mailbox_subject AND COALESCE(a.is_deleting,0)=0 AND u.status='active') ORDER BY o.id LIMIT 1`, accountID, formatDBTime(now)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_operations SET status='syncing',attempt_count=attempt_count+1 WHERE id=?`, op.ID); err != nil {
		return nil, err
	}
	op.Status = "syncing"
	op.AttemptCount++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &op, nil
}

func checkUserProviderDraftAttempt(ctx context.Context, tx *sql.Tx, op UserProviderDraftOperation) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_provider_draft_operations o JOIN gofer_provider_draft_states s ON s.account_id=o.account_id AND s.draft_key=o.draft_key JOIN accounts a ON a.id=s.account_id JOIN users u ON u.id=a.user_id
	 WHERE o.id=? AND o.account_id=? AND o.draft_key=? AND o.kind=? AND o.revision_token=? AND o.status='syncing' AND o.attempt_count=? AND s.provider=? AND s.mailbox_subject=? AND a.provider=s.provider AND a.auth_method='oauth2' AND COALESCE(a.provider_account_id,'')=s.mailbox_subject AND COALESCE(a.is_deleting,0)=0 AND u.status='active')`, op.ID, op.State.AccountID, op.State.DraftKey, op.Kind, op.RevisionToken, op.AttemptCount, op.State.Provider, op.State.MailboxSubject).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrMessageMutationSuperseded
	}
	return nil
}

func (db *DB) mutateUserProviderDraft(ctx context.Context, op UserProviderDraftOperation, fn func(*sql.Tx) error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkUserProviderDraftAttempt(ctx, tx, op); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) BeginUserProviderDraftCreate(ctx context.Context, op UserProviderDraftOperation) error {
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_operations SET create_attempted=1 WHERE id=? AND create_attempted=0 AND candidate_id='' AND EXISTS(SELECT 1 FROM gofer_provider_draft_states s WHERE s.account_id=gofer_provider_draft_operations.account_id AND s.draft_key=gofer_provider_draft_operations.draft_key AND s.local_revision!='')`, op.ID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return ErrMessageMutationSuperseded
		}
		return nil
	})
}

// Resolve a received Gmail message ID to its stable draft-container ID before
// PUT, retaining that identity even if replacement acknowledgment is lost.
func (db *DB) AdoptUserProviderDraftRemote(ctx context.Context, op UserProviderDraftOperation, id, messageID, revision string) error {
	if id == "" || messageID == "" {
		return errors.New("provider draft remote identity is required")
	}
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_states SET remote_id=?,remote_message_id=?,remote_revision=? WHERE account_id=? AND draft_key=? AND (remote_id='' OR remote_id=?)`, id, messageID, revision, op.State.AccountID, op.State.DraftKey, id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return ErrMessageMutationSuperseded
		}
		return nil
	})
}

func (db *DB) RecordUserProviderDraftCandidate(ctx context.Context, op UserProviderDraftOperation, id, messageID string) error {
	if id == "" || messageID == "" {
		return errors.New("provider draft candidate identity is required")
	}
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_operations SET candidate_id=?,candidate_message_id=? WHERE id=? AND (candidate_id='' OR (candidate_id=? AND candidate_message_id=?))`, id, messageID, op.ID, id, messageID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return ErrMessageMutationSuperseded
		}
		return nil
	})
}

func (db *DB) CompleteUserProviderDraftUpsert(ctx context.Context, op UserProviderDraftOperation) error {
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		var id, messageID string
		if err := tx.QueryRowContext(ctx, `SELECT candidate_id,candidate_message_id FROM gofer_provider_draft_operations WHERE id=? AND kind='upsert'`, op.ID).Scan(&id, &messageID); err != nil {
			return err
		}
		if id == "" || messageID == "" {
			return errors.New("provider draft candidate is not confirmed")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_states SET remote_id=?,remote_message_id=?,remote_revision=? WHERE account_id=? AND draft_key=?`, id, messageID, op.RevisionToken, op.State.AccountID, op.State.DraftKey); err != nil {
			return err
		}
		// Publishing an older remote revision must not bind its ID to newer local
		// content. The next immutable operation sees the actual remote state.
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND internet_message_id=? AND id IN(SELECT local_message_id FROM gofer_provider_draft_states WHERE account_id=? AND draft_key=? AND local_revision=?)`, messageID, op.State.AccountID, op.State.DraftKey, op.State.AccountID, op.State.DraftKey, op.RevisionToken); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM gofer_provider_draft_operations WHERE id=?`, op.ID)
		return err
	})
}

func (db *DB) CompleteUserProviderDraftDelete(ctx context.Context, op UserProviderDraftOperation) error {
	if op.Kind != "delete" {
		return errors.New("provider draft delete operation is required")
	}
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM gofer_provider_draft_operations WHERE id=?`, op.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_states SET remote_id='',remote_message_id='',remote_revision='' WHERE account_id=? AND draft_key=?`, op.State.AccountID, op.State.DraftKey); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM gofer_provider_draft_states WHERE account_id=? AND draft_key=? AND local_revision='' AND NOT EXISTS(SELECT 1 FROM gofer_provider_draft_operations o WHERE o.account_id=gofer_provider_draft_states.account_id AND o.draft_key=gofer_provider_draft_states.draft_key)`, op.State.AccountID, op.State.DraftKey)
		return err
	})
}

func (db *DB) AbandonDiscardedUserProviderDraft(ctx context.Context, op UserProviderDraftOperation) error {
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM gofer_provider_draft_operations WHERE id=? AND kind='upsert' AND candidate_id='' AND EXISTS(SELECT 1 FROM gofer_provider_draft_states s WHERE s.account_id=gofer_provider_draft_operations.account_id AND s.draft_key=gofer_provider_draft_operations.draft_key AND s.local_revision='')`, op.ID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return ErrMessageMutationSuperseded
		}
		return nil
	})
}

func (db *DB) FailUserProviderDraft(ctx context.Context, op UserProviderDraftOperation, status, text string, next time.Time, creationRejected bool) error {
	if status != "failed" && status != "ambiguous" && status != "blocked" {
		return errors.New("invalid provider draft failure status")
	}
	return db.mutateUserProviderDraft(ctx, op, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE gofer_provider_draft_operations SET status=?,last_error=?,next_attempt_at=?,create_attempted=CASE WHEN ? AND candidate_id='' THEN 0 ELSE create_attempted END WHERE id=?`, status, text, next.UTC(), creationRejected, op.ID)
		return err
	})
}

func (db *DB) NextUserProviderDraftAttempt(ctx context.Context, accountID string) (time.Time, error) {
	if !db.userMailDelivery {
		return time.Time{}, nil
	}
	var next sqliteNullTime
	err := db.Read().QueryRowContext(ctx, `SELECT o.next_attempt_at FROM gofer_provider_draft_operations o JOIN gofer_provider_draft_states s ON s.account_id=o.account_id AND s.draft_key=o.draft_key JOIN accounts a ON a.id=s.account_id
	 WHERE o.account_id=? AND o.status IN ('pending','syncing','failed','ambiguous') AND a.provider=s.provider AND a.auth_method='oauth2' AND COALESCE(a.provider_account_id,'')=s.mailbox_subject AND COALESCE(a.is_deleting,0)=0
	 AND NOT EXISTS(SELECT 1 FROM gofer_provider_draft_operations earlier WHERE earlier.account_id=o.account_id AND earlier.draft_key=o.draft_key AND earlier.id<o.id)
	 ORDER BY julianday(replace(o.next_attempt_at,' +0000 UTC','')) LIMIT 1`, accountID).Scan(&next)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return next.Time, err
}

func (db *DB) protectUserProviderDraftTx(ctx context.Context, tx *sql.Tx, accountID, key string) (bool, int64, error) {
	if !db.userMailDelivery {
		return false, 0, nil
	}
	var id sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT s.local_message_id FROM gofer_provider_draft_states s WHERE s.account_id=? AND s.draft_key=? AND EXISTS(SELECT 1 FROM gofer_provider_draft_operations o WHERE o.account_id=s.account_id AND o.draft_key=s.draft_key)`, accountID, key).Scan(&id)
	if err == sql.ErrNoRows {
		return false, 0, nil
	}
	return err == nil, id.Int64, err
}

// Alias is a fixed local SQL alias, never request input.
func (db *DB) userProviderDraftProtectionSQL(alias string) string {
	if !db.userMailDelivery {
		return ""
	}
	return ` AND NOT EXISTS(SELECT 1 FROM gofer_provider_draft_states ds JOIN gofer_provider_draft_operations dop ON dop.account_id=ds.account_id AND dop.draft_key=ds.draft_key WHERE ds.local_message_id=` + alias + `.id) `
}
