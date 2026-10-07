package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func userLabelProvider(provider string) (string, error) {
	switch provider {
	case "imap":
		return LabelProviderIMAPKeyword, nil
	case "gmail":
		return LabelProviderGmail, nil
	case "outlook":
		return LabelProviderOutlook, nil
	}
	return "", fmt.Errorf("unsupported label provider %q", provider)
}

// A new request replaces the old row with a fresh autoincrement ID. Opposite
// operations coalesce, and an in-flight reply cannot consume newer intent.
func queueUserLabelTx(ctx context.Context, tx *sql.Tx, account string, message int64, folder, provider, operation, name string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM label_mutation_queue WHERE account_id=? AND message_id=? AND provider_type=? AND label_name=? COLLATE NOCASE`, account, message, provider, name); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO label_mutation_queue(account_id,message_id,folder_id,provider_type,operation,label_name) VALUES(?,?,?,?,?,?)`, account, message, folder, provider, operation, name)
	return err
}

func ownedLabelTargetTx(ctx context.Context, tx *sql.Tx, owner string, info ThreadMessageMutationInfo) (string, string, error) {
	var provider, folder string
	err := tx.QueryRowContext(ctx, `SELECT a.provider,COALESCE((SELECT q.folder_id FROM message_mutations q WHERE q.account_id=a.id AND q.message_id=m.id AND q.kind='move' AND q.status!='applied' ORDER BY q.created_at LIMIT 1),mfs.folder_id)
 FROM messages m JOIN accounts a ON a.id=m.account_id JOIN users u ON u.id=a.user_id
 JOIN message_folder_state mfs ON mfs.message_id=m.id JOIN folders f ON f.id=mfs.folder_id AND f.account_id=a.id
 WHERE m.id=? AND a.id=? AND a.user_id=? AND mfs.folder_id=? AND mfs.is_deleted=0
 AND COALESCE(a.is_deleting,0)=0 AND u.status='active'`, info.MessageID, info.AccountID, owner, info.FolderID).Scan(&provider, &folder)
	if err != nil {
		return "", "", err
	}
	if provider != info.AccountProvider {
		return "", "", errors.New("label target provider changed")
	}
	labelProvider, err := userLabelProvider(provider)
	return labelProvider, folder, err
}

// QueueUserLabels saves optimistic labels and remote intent in one transaction.
// Inputs are resolved by authenticated handlers; ownership is repeated here.
func (db *DB) QueueUserLabels(ctx context.Context, owner string, infos []ThreadMessageMutationInfo, name, operation string) error {
	name = strings.TrimSpace(name)
	if owner == "" || name == "" || len(infos) == 0 || len(name) > 512 {
		return errors.New("invalid label action")
	}
	if operation != LabelMutationAdd && operation != LabelMutationRemove {
		return errors.New("invalid label operation")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, info := range infos {
		provider, folder, err := ownedLabelTargetTx(ctx, tx, owner, info)
		if err != nil {
			return err
		}
		if err := queueUserLabelTx(ctx, tx, info.AccountID, info.MessageID, folder, provider, operation, name); err != nil {
			return err
		}
		if operation == LabelMutationAdd {
			if err := db.addMessageLabelsTx(ctx, tx, info.MessageID, info.AccountID, []LabelInput{{AccountID: info.AccountID, ProviderType: LabelProviderLocal, Name: name}}); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=? AND label_id IN (SELECT id FROM labels WHERE account_id=? AND lower(name)=lower(?) AND provider_type IN (?,?))`, info.MessageID, info.AccountID, name, provider, LabelProviderLocal); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// IMAP spam actions persist both training keywords and the folder move, even
// when the message is already in the requested destination.
func (db *DB) QueueUserIMAPSpam(ctx context.Context, owner string, infos []ThreadMessageMutationInfo, destination string, spam bool) error {
	if owner == "" || destination == "" || len(infos) == 0 {
		return errors.New("invalid spam action")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	bySource := map[string][]int64{}
	for _, info := range infos {
		provider, source, err := ownedLabelTargetTx(ctx, tx, owner, info)
		if err != nil {
			return err
		}
		if provider != LabelProviderIMAPKeyword {
			return errors.New("IMAP spam action requires IMAP")
		}
		for _, flag := range []struct {
			name string
			add  bool
		}{{"$Junk", spam}, {"$NotJunk", !spam}} {
			operation := LabelMutationRemove
			if flag.add {
				operation = LabelMutationAdd
			}
			if err := queueUserLabelTx(ctx, tx, info.AccountID, info.MessageID, source, provider, operation, flag.name); err != nil {
				return err
			}
		}
		if info.FolderID != destination {
			bySource[info.FolderID] = append(bySource[info.FolderID], info.MessageID)
		}
	}
	refresh := map[string]struct{}{}
	for source, ids := range bySource {
		folders, err := moveMessagesAndQueueTx(ctx, tx, ids, source, destination, owner)
		if err != nil {
			return err
		}
		for folder := range folders {
			refresh[folder] = struct{}{}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for folder := range refresh {
		_, _ = db.RefreshFolderUnreadCount(ctx, folder)
		_ = db.RefreshFolderThreadState(ctx, folder)
	}
	return nil
}

func (db *DB) NextUserLabelMutationAttempt(ctx context.Context, account string) (time.Time, error) {
	var next sqliteNullTime
	err := db.Read().QueryRowContext(ctx, `SELECT next_attempt_at FROM label_mutation_queue WHERE account_id=? ORDER BY julianday(replace(next_attempt_at,' +0000 UTC','')),id LIMIT 1`, account).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return next.Time, err
}

const userLabelAttemptPredicate = `id=? AND account_id=? AND message_id=? AND folder_id=? AND provider_type=? AND operation=? AND label_name=? AND attempts=?`

func userLabelAttemptArgs(e LabelMutationQueueEntry) []any {
	return []any{e.ID, e.AccountID, e.MessageID, e.FolderID, e.ProviderType, e.Operation, e.LabelName, e.Attempts}
}

func (db *DB) FailUserLabelMutation(ctx context.Context, e LabelMutationQueueEntry, failure error, retryAt time.Time) error {
	next := time.Now().Add(labelMutationRetryDelay(e.Attempts + 1))
	if retryAt.After(next) {
		next = retryAt
	}
	args := append([]any{e.Attempts + 1, formatDBTime(next), failure.Error()}, userLabelAttemptArgs(e)...)
	_, err := db.Write().ExecContext(ctx, `UPDATE label_mutation_queue SET attempts=?,next_attempt_at=?,last_error=?,updated_at=CURRENT_TIMESTAMP WHERE `+userLabelAttemptPredicate, args...)
	return err
}

func (db *DB) DiscardUserLabelMutation(ctx context.Context, e LabelMutationQueueEntry) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM label_mutation_queue WHERE `+userLabelAttemptPredicate, userLabelAttemptArgs(e)...)
	return err
}

// Publication validates the queue snapshot and the message/account identity in
// the same transaction as label association and queue deletion. IMAP also checks
// its folder/UID epoch; a pending move may hide this still-valid source locally.
func (db *DB) CompleteUserLabelMutation(ctx context.Context, e LabelMutationQueueEntry, info MessageMutationInfo, subject, resolvedID string, validity uint32, label LabelInput) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM label_mutation_queue WHERE `+userLabelAttemptPredicate, userLabelAttemptArgs(e)...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrMessageMutationSuperseded
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN accounts a ON a.id=m.account_id WHERE m.id=? AND m.account_id=? AND a.provider=? AND COALESCE(a.provider_account_id,'')=? AND COALESCE(a.is_deleting,0)=0 AND COALESCE(m.remote_message_id,'')=? AND COALESCE(m.internet_message_id,'')=?`, e.MessageID, e.AccountID, info.AccountProvider, subject, info.RemoteMessageID, info.InternetMessageID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("label message or mailbox identity changed")
	}
	auth := "oauth2"
	if info.AccountProvider == "imap" {
		auth = "plain"
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id=? AND auth_method=?`, e.AccountID, auth).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("label mailbox authentication changed")
	}
	if e.ProviderType == LabelProviderIMAPKeyword {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_folder_state mfs JOIN folders f ON f.id=mfs.folder_id WHERE mfs.message_id=? AND mfs.folder_id=? AND f.account_id=? AND f.remote_id=? AND COALESCE(mfs.remote_uid,0)=? AND COALESCE(f.uid_validity,0)=?
  AND (mfs.is_deleted=0 OR EXISTS(SELECT 1 FROM message_mutations q WHERE q.account_id=f.account_id AND q.message_id=mfs.message_id AND q.folder_id=f.id AND q.kind='move' AND q.status!='applied'))`, e.MessageID, info.FolderID, e.AccountID, info.FolderRemoteID, info.RemoteUID, validity).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("label IMAP folder or UID identity changed")
		}
	} else if info.RemoteMessageID == "" && resolvedID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, resolvedID, e.MessageID); err != nil {
			return err
		}
	}
	if e.ProviderType == LabelProviderIMAPKeyword && (e.LabelName == "$Junk" || e.LabelName == "$NotJunk") {
		// Training keywords are mailbox status, not labels shown in the UI.
		if _, err := tx.ExecContext(ctx, `DELETE FROM label_mutation_queue WHERE `+userLabelAttemptPredicate, userLabelAttemptArgs(e)...); err != nil {
			return err
		}
		return tx.Commit()
	}
	if e.Operation == LabelMutationAdd {
		label.AccountID = e.AccountID
		if err := db.addMessageLabelsTx(ctx, tx, e.MessageID, e.AccountID, []LabelInput{label}); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=? AND label_id IN (SELECT id FROM labels WHERE account_id=? AND provider_type=? AND (lower(name)=lower(?) OR (?!='' AND provider_id=?)))`, e.MessageID, e.AccountID, e.ProviderType, e.LabelName, label.ProviderID, label.ProviderID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=? AND label_id IN (SELECT id FROM labels WHERE account_id=? AND provider_type='local' AND lower(name)=lower(?))`, e.MessageID, e.AccountID, e.LabelName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM label_mutation_queue WHERE `+userLabelAttemptPredicate, userLabelAttemptArgs(e)...); err != nil {
		return err
	}
	return tx.Commit()
}
