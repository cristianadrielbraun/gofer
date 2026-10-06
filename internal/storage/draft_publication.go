package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DraftPublication contains immutable candidate files and their delivery jobs.
// Stage runs after allocating the local ID, inside the write transaction. It
// must only perform local file work and must clean its candidate on any failure.
type DraftPublication struct {
	TextPath, HTMLPath, RawPath string
	Attachments                 []AttachmentRow
	Sync                        QueueIMAPDraftUpsertInput
	Pending                     *QueueOutgoingSendInput
}

func (db *DB) PublishDraft(ctx context.Context, draft DraftMessageInput, stage func(int64) (DraftPublication, error)) (int64, error) {
	if draft.Date.IsZero() {
		draft.Date = time.Now().UTC()
	}
	if stage == nil {
		return 0, errors.New("draft staging is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var collision bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m WHERE account_id=? AND internet_message_id=? AND NOT EXISTS(SELECT 1 FROM message_folder_state s WHERE s.message_id=m.id AND s.is_draft=1))`, draft.AccountID, draft.InternetMessageID).Scan(&collision); err != nil {
		return 0, err
	}
	if collision {
		return 0, errors.New("message already exists outside Drafts")
	}
	var folder bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folders WHERE id=? AND account_id=? AND role='drafts')`, draft.FolderID, draft.AccountID).Scan(&folder); err != nil {
		return 0, err
	}
	if !folder {
		return 0, errors.New("draft folder does not belong to account")
	}
	id, err := db.saveDraftMessageTx(ctx, tx, draft)
	if err != nil {
		return 0, err
	}
	c, err := stage(id)
	if err != nil {
		return 0, err
	}
	if c.Sync.State.AccountID != draft.AccountID || c.Sync.State.DraftKey != draft.InternetMessageID || c.Sync.State.FolderID != draft.FolderID {
		return 0, errors.New("draft sync identity mismatch")
	}
	c.Sync.State.LocalMessageID = id
	// Seed remote identity when editing a draft originally received by IMAP.
	// Existing tracked state wins, including a revision completed just before
	// this transaction; never overwrite it with a stale request snapshot.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(s.remote_uid,0),COALESCE(f.uid_validity,0) FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE s.message_id=? AND s.folder_id=?`, id, draft.FolderID).Scan(&c.Sync.State.RemoteUID, &c.Sync.State.UIDValidity); err != nil {
		return 0, err
	}
	var trackedUID, trackedValidity uint32
	trackedErr := tx.QueryRowContext(ctx, `SELECT remote_uid,uid_validity FROM imap_draft_states WHERE account_id=? AND draft_key=?`, draft.AccountID, draft.InternetMessageID).Scan(&trackedUID, &trackedValidity)
	if trackedErr == nil {
		c.Sync.State.RemoteUID = trackedUID
		c.Sync.State.UIDValidity = trackedValidity
	} else if trackedErr != sql.ErrNoRows {
		return 0, trackedErr
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_text_path=?,body_html_path=?,body_html_original_path='',raw_path=?,has_attachments=? WHERE id=?`, c.TextPath, c.HTMLPath, c.RawPath, len(c.Attachments) > 0, id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id=?`, id); err != nil {
		return 0, err
	}
	for _, a := range c.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(message_id,filename,content_type,size_bytes,content_id,inline,storage_path) VALUES(?,?,?,?,?,?,?)`, id, a.Filename, a.ContentType, a.SizeBytes, a.ContentID, a.Inline, a.StoragePath); err != nil {
			return 0, err
		}
	}
	if _, err := queueIMAPDraftUpsertTx(ctx, tx, c.Sync); err != nil {
		return 0, err
	}
	if p := c.Pending; p != nil {
		if p.AccountID != draft.AccountID || p.ID == "" || p.DraftID != draft.InternetMessageID || len(p.MIMEData) == 0 || len(p.MessageJSON) == 0 || len(p.EnvelopeRecipients) == 0 {
			return 0, errors.New("pending send payload mismatch")
		}
		recipients, err := json.Marshal(p.EnvelopeRecipients)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET envelope_from=?,envelope_recipients=?,mime_data=?,message_json=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=? AND message_id=? AND status='pending'`, p.EnvelopeFrom, string(recipients), p.MIMEData, string(p.MessageJSON), p.ID, draft.AccountID, id); err != nil {
			return 0, err
		}
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{id}); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	db.UpsertObservedContactsForMessage(ctx, draft.AccountID, draft.FromName, draft.FromEmail, draft.ToRecipients, draft.CCRecipients, draft.BCCRecipients, draft.Date)
	db.RefreshFolderUnreadCount(ctx, draft.FolderID)
	return id, nil
}

// DiscardIMAPDraft cancels waiting delivery and retains remote deletion even
// after the local row is gone. An in-flight send keeps its immutable snapshot.
func (db *DB) DiscardIMAPDraft(ctx context.Context, accountID, draftKey string) (string, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id int64
	var folder, remote string
	var uid, validity uint32
	err = tx.QueryRowContext(ctx, `SELECT m.id,f.id,f.remote_id,COALESCE(s.remote_uid,0),COALESCE(f.uid_validity,0) FROM messages m JOIN message_folder_state s ON s.message_id=m.id JOIN folders f ON f.id=s.folder_id WHERE m.account_id=? AND m.internet_message_id=? AND s.is_draft=1 LIMIT 1`, accountID, draftKey).Scan(&id, &folder, &remote, &uid, &validity)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var trackedUID, trackedValidity uint32
	trackedErr := tx.QueryRowContext(ctx, `SELECT remote_uid,uid_validity FROM imap_draft_states WHERE account_id=? AND draft_key=?`, accountID, draftKey).Scan(&trackedUID, &trackedValidity)
	if trackedErr == nil {
		uid, validity = trackedUID, trackedValidity
	} else if trackedErr != sql.ErrNoRows {
		return "", trackedErr
	}
	if _, err := queueIMAPDraftDeleteTx(ctx, tx, IMAPDraftState{AccountID: accountID, DraftKey: draftKey, LocalMessageID: id, FolderID: folder, FolderRemoteName: remote, RemoteUID: uid, UIDValidity: validity}); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status='canceled',locked_at=NULL,envelope_recipients='[]',mime_data=NULL,message_json='',updated_at=CURRENT_TIMESTAMP WHERE message_id=? AND status IN ('pending','failed','ambiguous')`, id); err != nil {
		return "", err
	}
	for _, q := range []string{`DELETE FROM message_folder_state WHERE message_id=?`, `DELETE FROM message_recipients WHERE message_id=?`, `DELETE FROM message_references WHERE message_id=?`, `DELETE FROM unresolved_references WHERE child_message_id=?`, `DELETE FROM messages WHERE id=?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return "", fmt.Errorf("discard draft: %w", err)
		}
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{id}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	db.RefreshFolderUnreadCount(ctx, folder)
	return folder, nil
}
