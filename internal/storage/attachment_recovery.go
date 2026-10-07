package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
)

// AttachmentRecovery copies the identity needed across a file/provider wait.
// Parts retain MIME insertion order so identical filenames can be recovered
// without confusing sibling attachments. No borrowed DB handle escapes.
type AttachmentRecovery struct {
	Info                                                   AttachmentFetchInfo
	InternetMessageID, RawPath, MailboxSubject, AuthMethod string
	NetworkAllowed                                         bool
	PartIndex                                              int
	Parts                                                  []AttachmentRecoveryPart
}

type AttachmentRecoveryPart struct {
	Filename, ContentType, ContentID string
	SizeBytes                        int64
	Inline                           bool
}

type attachmentRecoveryQueries interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (db *DB) attachmentRecovery(ctx context.Context, q attachmentRecoveryQueries, owner string, id int64) (*AttachmentRecovery, error) {
	var s AttachmentRecovery
	visible := `EXISTS(SELECT 1 FROM message_folder_state ms JOIN folders f ON f.id=ms.folder_id WHERE ms.message_id=m.id AND ms.is_deleted=0 AND f.account_id=m.account_id)` + db.userProviderDraftProtectionSQL("m")
	err := q.QueryRowContext(ctx, `SELECT att.id,att.message_id,m.account_id,a.provider,COALESCE(m.remote_message_id,''),COALESCE(att.provider_remote_id,''),att.filename,att.content_type,COALESCE(att.content_id,''),att.size_bytes,att.storage_path,
	 COALESCE(m.internet_message_id,''),COALESCE(m.raw_path,''),COALESCE(a.provider_account_id,''),a.auth_method,(`+visible+`)
	 FROM attachments att JOIN messages m ON m.id=att.message_id JOIN accounts a ON a.id=m.account_id JOIN users u ON u.id=a.user_id
	 WHERE att.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active'`, id, owner).Scan(
		&s.Info.ID, &s.Info.MessageID, &s.Info.AccountID, &s.Info.AccountProvider, &s.Info.ProviderMessageID, &s.Info.ProviderAttachmentID, &s.Info.Filename, &s.Info.ContentType, &s.Info.ContentID, &s.Info.SizeBytes, &s.Info.StoragePath,
		&s.InternetMessageID, &s.RawPath, &s.MailboxSubject, &s.AuthMethod, &s.NetworkAllowed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id,filename,content_type,COALESCE(content_id,''),size_bytes,inline FROM attachments WHERE message_id=? ORDER BY id`, s.Info.MessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var part AttachmentRecoveryPart
		var partID int64
		if err := rows.Scan(&partID, &part.Filename, &part.ContentType, &part.ContentID, &part.SizeBytes, &part.Inline); err != nil {
			return nil, err
		}
		if partID == id {
			s.PartIndex = len(s.Parts)
		}
		s.Parts = append(s.Parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (db *DB) GetAttachmentRecoveryForUser(ctx context.Context, owner string, id int64) (*AttachmentRecovery, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, err := db.attachmentRecovery(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

// PublishRecoveredAttachment changes only the requested path. Provider lookup
// may bind a previously missing message ID in this same identity-checked commit.
// Local saved MIME is allowed for pending drafts; remote MIME is not.
func (db *DB) PublishRecoveredAttachment(ctx context.Context, owner string, expected AttachmentRecovery, providerID, path string, localMIME bool) error {
	if path == "" || expected.Info.ID <= 0 {
		return errors.New("attachment candidate is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := db.attachmentRecovery(ctx, tx, owner, expected.Info.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return ErrMessageMutationSuperseded
	}
	allowed := current.NetworkAllowed
	current.NetworkAllowed = expected.NetworkAllowed
	if !reflect.DeepEqual(*current, expected) {
		return ErrMessageMutationSuperseded
	}
	if localMIME {
		if expected.RawPath == "" {
			return errors.New("saved MIME identity is required")
		}
	} else {
		if !allowed || providerID == "" || (expected.Info.AccountProvider != "gmail" && expected.Info.AccountProvider != "outlook") || expected.AuthMethod != "oauth2" {
			return ErrMessageMutationSuperseded
		}
		if expected.Info.ProviderMessageID != "" && providerID != expected.Info.ProviderMessageID {
			return ErrMessageMutationSuperseded
		}
		if expected.Info.ProviderMessageID == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=?`, providerID, expected.Info.MessageID, expected.Info.AccountID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attachments SET storage_path=? WHERE id=? AND message_id=?`, path, expected.Info.ID, expected.Info.MessageID); err != nil {
		return err
	}
	return tx.Commit()
}
