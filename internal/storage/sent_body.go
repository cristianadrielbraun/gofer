package storage

import (
	"context"
	"errors"
	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

// PublishSentBody makes a local Sent cache retryable independently of SMTP
// acceptance. Candidate files must be immutable until publication succeeds.
func (db *DB) PublishSentBody(ctx context.Context, id int64, accountID, messageID string, p *mailmessage.ParsedMessage, textPath, htmlPath string) error {
	if p == nil {
		return errors.New("Sent body is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages SET body_text_path=?,body_html_path=?,raw_path=?,has_attachments=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=? AND internet_message_id=? AND EXISTS(SELECT 1 FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE s.message_id=messages.id AND f.role='sent')`, textPath, htmlPath, p.RawPath, len(p.Attachments) > 0, id, accountID, messageID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("Sent message identity changed")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id=?`, id); err != nil {
		return err
	}
	for _, a := range p.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(message_id,filename,content_type,size_bytes,content_id,inline,storage_path) VALUES(?,?,?,?,?,?,?)`, id, a.Filename, a.ContentType, a.Size, a.ContentID, a.Inline, a.BlobPath); err != nil {
			return err
		}
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{id}); err != nil {
		return err
	}
	return tx.Commit()
}
