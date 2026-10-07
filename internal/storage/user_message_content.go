package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var ErrMessageContentChanged = errors.New("message content changed during external work")

// MessageContentSnapshot binds copied content to its owner, mailbox principal
// and immutable body paths. Remote work must compare this before publication.
type MessageContentSnapshot struct {
	Owner                                                               string
	ID                                                                  int64
	AccountID, Provider, AuthMethod, ProviderAccountID, RemoteMessageID string
	BodyHTMLPath, BodyTextPath, OriginalHTMLPath, RawPath               string
	SenderEmail, Subject, Preview                                       string
}

type messageContentRowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func messageContentSnapshot(ctx context.Context, query messageContentRowReader, owner string, id int64) (*MessageContentSnapshot, error) {
	if id <= 0 || strings.TrimSpace(owner) == "" {
		return nil, sql.ErrNoRows
	}
	s := &MessageContentSnapshot{Owner: owner, ID: id}
	err := query.QueryRowContext(ctx, `SELECT m.account_id,COALESCE(a.provider,''),COALESCE(a.auth_method,''),COALESCE(a.provider_account_id,''),COALESCE(m.remote_message_id,''),COALESCE(m.body_html_path,''),COALESCE(m.body_text_path,''),COALESCE(m.body_html_original_path,''),COALESCE(m.raw_path,''),LOWER(COALESCE(m.from_email,'')),COALESCE(m.subject,''),COALESCE(m.preview_text,'')
	FROM messages m JOIN accounts a ON a.id=m.account_id
	WHERE m.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
	AND EXISTS(SELECT 1 FROM message_folder_state ms JOIN folders f ON f.id=ms.folder_id WHERE ms.message_id=m.id AND ms.is_deleted=0 AND f.account_id=m.account_id)`, id, owner).
		Scan(&s.AccountID, &s.Provider, &s.AuthMethod, &s.ProviderAccountID, &s.RemoteMessageID, &s.BodyHTMLPath, &s.BodyTextPath, &s.OriginalHTMLPath, &s.RawPath, &s.SenderEmail, &s.Subject, &s.Preview)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (db *DB) GetMessageContentSnapshotForUser(ctx context.Context, owner string, id int64) (*MessageContentSnapshot, error) {
	return messageContentSnapshot(ctx, db.Read(), owner, id)
}

// PublishUserRemoteContent changes the cached path and consent atomically.
// Guard checks central lifecycle after waiting for a writer and before commit.
func (db *DB) PublishUserRemoteContent(ctx context.Context, snapshot *MessageContentSnapshot, htmlPath, mode string, guard func(*sql.Tx) error) error {
	if snapshot == nil || htmlPath == "" || (mode != "email" && mode != "sender") || guard == nil {
		return errors.New("remote content publication needs a snapshot, path, consent and guard")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := guard(tx); err != nil {
		return err
	}
	current, err := messageContentSnapshot(ctx, tx, snapshot.Owner, snapshot.ID)
	if err != nil {
		return err
	}
	if *current != *snapshot {
		return ErrMessageContentChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_html_path=? WHERE id=? AND account_id=?`, htmlPath, snapshot.ID, snapshot.AccountID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO remote_content_messages(message_id) VALUES(?)`, snapshot.ID)
	if err == nil && mode == "sender" && snapshot.SenderEmail != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO app_settings(user_id,key,value,updated_at) VALUES(?,?,'1',CURRENT_TIMESTAMP) ON CONFLICT(user_id,key) DO UPDATE SET value='1',updated_at=CURRENT_TIMESTAMP`, snapshot.Owner, remoteContentSenderSettingKey(snapshot.SenderEmail))
	}
	if err != nil {
		return err
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{snapshot.ID}); err != nil {
		return err
	}
	current, err = messageContentSnapshot(ctx, tx, snapshot.Owner, snapshot.ID)
	if err != nil {
		return err
	}
	expected := *snapshot
	expected.BodyHTMLPath = htmlPath
	if *current != expected {
		return ErrMessageContentChanged
	}
	if err := guard(tx); err != nil {
		return err
	}
	return tx.Commit()
}
