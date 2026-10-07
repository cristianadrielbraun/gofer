package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
)

// RawMessageSnapshot copies one visible message's retrieval identity. It holds
// no connection; callers must validate again after file, protocol or writer waits.
type RawMessageSnapshot struct {
	owner      string
	id         int64
	info       RawMessageInfo
	connection []any
}

type RawMessageInfo struct {
	AccountID, Provider, Subject, AuthMethod, EmailAddress, RawPath, RemoteID, InternetMessageID string
	FolderID, FolderRemoteID                                                                     string
	RemoteUID, UIDValidity                                                                       uint32
}

func (s *RawMessageSnapshot) Info() RawMessageInfo { return s.info }

// Only an already-published raw path may differ between queued readers.
// A replacement message, mailbox or IMAP UID is never adopted after waiting.
func (s *RawMessageSnapshot) SameIdentity(other *RawMessageSnapshot) bool {
	if s == nil || other == nil {
		return false
	}
	a, b := *s, *other
	a.info.RawPath = ""
	b.info.RawPath = ""
	return reflect.DeepEqual(a, b)
}

func (db *DB) rawMessageSnapshotTx(ctx context.Context, tx *sql.Tx, owner string, id int64) (*RawMessageSnapshot, error) {
	if owner == "" || id <= 0 {
		return nil, sql.ErrNoRows
	}
	s := &RawMessageSnapshot{owner: owner, id: id}
	var host, username, tlsMode string
	var port int
	var password []byte
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.provider,a.provider_account_id,a.auth_method,a.email_address,
 COALESCE(m.raw_path,''),COALESCE(m.remote_message_id,''),COALESCE(m.internet_message_id,''),
 f.id,COALESCE(f.remote_id,''),COALESCE(ms.remote_uid,0),COALESCE(f.uid_validity,0),a.imap_host,a.imap_port,a.imap_tls_mode,a.username,a.encrypted_password
 FROM messages m JOIN accounts a ON a.id=m.account_id JOIN users u ON u.id=a.user_id
 JOIN message_folder_state ms ON ms.message_id=m.id JOIN folders f ON f.id=ms.folder_id AND f.account_id=a.id
 WHERE m.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND u.status='active' AND u.deletion_pending=0 AND ms.is_deleted=0`+db.userProviderDraftProtectionSQL("m")+`
 ORDER BY f.id LIMIT 1`, id, owner).Scan(&s.info.AccountID, &s.info.Provider, &s.info.Subject, &s.info.AuthMethod, &s.info.EmailAddress,
		&s.info.RawPath, &s.info.RemoteID, &s.info.InternetMessageID, &s.info.FolderID, &s.info.FolderRemoteID, &s.info.RemoteUID, &s.info.UIDValidity,
		&host, &port, &tlsMode, &username, &password)
	if err != nil {
		return nil, err
	}
	s.connection = []any{host, port, tlsMode, username, password}
	return s, nil
}

func (db *DB) SnapshotRawMessage(ctx context.Context, owner string, id int64) (*RawMessageSnapshot, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, err := db.rawMessageSnapshotTx(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

func (db *DB) validateRawMessageTx(ctx context.Context, tx *sql.Tx, s *RawMessageSnapshot, guard func() error) error {
	if s == nil || s.owner == "" || s.id <= 0 || guard == nil {
		return ErrMessageMutationSuperseded
	}
	if err := guard(); err != nil {
		return err
	}
	current, err := db.rawMessageSnapshotTx(ctx, tx, s.owner, s.id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMessageMutationSuperseded
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, s) {
		return ErrMessageMutationSuperseded
	}
	return nil
}

func (db *DB) ValidateRawMessage(ctx context.Context, s *RawMessageSnapshot, guard func() error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.validateRawMessageTx(ctx, tx, s, guard); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishRawMessage restores only raw_path. Body caches, attachment IDs, FTS and
// thread membership remain intact. Both identity and lifecycle are rechecked
// after the writer wait and after the update so trigger faults roll back.
func (db *DB) PublishRawMessage(ctx context.Context, s *RawMessageSnapshot, path string, guard func() error) (*RawMessageSnapshot, error) {
	if path == "" {
		return nil, errors.New("raw message candidate required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := db.validateRawMessageTx(ctx, tx, s, guard); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE messages SET raw_path=? WHERE id=? AND account_id=?`, path, s.id, s.info.AccountID)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrMessageMutationSuperseded
	}
	expected := *s
	expected.info.RawPath = path
	if err := db.validateRawMessageTx(ctx, tx, &expected, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &expected, nil
}
