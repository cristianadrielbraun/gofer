package storage

import (
	"context"
	"fmt"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

// MessageBodyCache references already-written, immutable candidate blob files.
// Database publication is one transaction; a failed transaction is retryable.
type MessageBodyCache struct {
	Parsed                               *mailmessage.ParsedMessage
	FetchInfo                            *MessageFetchInfo
	UIDValidity                          uint32
	TextPath, HTMLPath, OriginalHTMLPath string
}

func (db *DB) SaveMessageBodyCache(ctx context.Context, id int64, accountID string, c MessageBodyCache) error {
	if c.Parsed == nil {
		return fmt.Errorf("parsed body required")
	}
	if c.FetchInfo == nil || c.FetchInfo.AccountID != accountID {
		return fmt.Errorf("message fetch identity required")
	}
	p := c.Parsed
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var matches int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN message_folder_state ms ON ms.message_id=m.id JOIN folders f ON f.id=ms.folder_id WHERE m.id=? AND m.account_id=? AND f.account_id=? AND f.remote_id=? AND ms.remote_uid=? AND f.uid_validity=?`, id, accountID, accountID, c.FetchInfo.FolderRemoteID, c.FetchInfo.RemoteUID, c.UIDValidity).Scan(&matches); err != nil {
		return err
	}
	if matches == 0 {
		return fmt.Errorf("message identity changed before body publication")
	}
	var rawID string
	var received sqliteNullTime
	if err := tx.QueryRowContext(ctx, `SELECT internet_message_id,date_received FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&rawID, &received); err != nil {
		return err
	}
	snippet := p.Snippet
	if snippet == "" {
		snippet = p.Subject
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET body_text_path=?,body_html_path=?,body_html_original_path=?,raw_path=?,subject=?,from_name=?,from_email=?,snippet=?,preview_text=?,has_attachments=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=?`, c.TextPath, c.HTMLPath, c.OriginalHTMLPath, p.RawPath, p.Subject, p.FromName, p.FromEmail, snippet, snippet, len(p.Attachments) > 0, id, accountID)
	if err != nil {
		return err
	}
	for _, table := range []string{"attachments", "message_recipients"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE message_id=?`, id); err != nil {
			return err
		}
	}
	for _, a := range p.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(message_id,filename,content_type,size_bytes,content_id,inline,storage_path) VALUES(?,?,?,?,?,?,?)`, id, a.Filename, a.ContentType, a.Size, a.ContentID, a.Inline, a.BlobPath); err != nil {
			return err
		}
	}
	for _, list := range []struct {
		kind       string
		recipients []mailmessage.Recipient
	}{{"to", p.To}, {"cc", p.CC}} {
		for _, r := range list.recipients {
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(message_id,kind,name,email) VALUES(?,?,?,?)`, id, list.kind, r.Name, r.Email); err != nil {
				return err
			}
		}
	}
	normalized := mailmessage.NormalizeMessageID(rawID)
	if normalized == "" {
		normalized = fmt.Sprintf("local-%d@gofer.local", id)
	}
	date := time.Now().UTC()
	if received.Valid {
		date = received.Time
	}
	reply := ""
	if ids := mailmessage.ParseMessageIDs(p.InReplyTo); len(ids) > 0 {
		reply = ids[0]
	}
	if err := db.reconcileMessageThreadTx(ctx, tx, id, accountID, normalized, reply, p.References, p.Subject, date); err != nil {
		return err
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{id}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
