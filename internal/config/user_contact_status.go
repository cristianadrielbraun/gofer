package config

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// StartContactPull normalizes a built-in config and returns the snapshot from
// that same transaction. A worker must not recapture a possibly replaced
// identity after marking started, or reuse the old absent-config fingerprint.
func (s *UserAccountStore) StartContactPull(ctx context.Context, snapshot *AccountServiceSnapshot) (next *AccountServiceSnapshot, err error) {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return nil, err
	}
	cfg := snapshot.ContactConfig()
	if !cfg.Enabled || (cfg.Provider != "gmail" && cfg.Provider != "outlook" && cfg.Provider != "carddav") {
		return nil, storage.ErrContactPublication
	}
	err = s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.serviceGuard(ctx, snapshot, true)(tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled,last_started_at)
 VALUES(?,?,?,1,CURRENT_TIMESTAMP) ON CONFLICT(account_id) DO UPDATE SET
 provider=excluded.provider,last_started_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
 WHERE account_contact_sync_configs.user_id=excluded.user_id AND account_contact_sync_configs.enabled=1`, snapshot.id, snapshot.owner, cfg.Provider)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return storage.ErrContactPublication
		}
		next, err = s.serviceSnapshot(ctx, tx, snapshot.owner, snapshot.id)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

// FinishContactPull guards status publication as well as data. Completion does
// not reset DAV cursors checkpointed by individual book transactions. Cancellation
// uses the operation context; there is no detached late status write.
func (s *UserAccountStore) FinishContactPull(ctx context.Context, snapshot *AccountServiceSnapshot, imported int, pullErr error) error {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return err
	}
	if imported < 0 {
		imported = 0
	}
	return s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.serviceGuard(ctx, snapshot, true)(tx); err != nil {
			return err
		}
		var result sql.Result
		if pullErr == nil {
			result, err = tx.ExecContext(ctx, `UPDATE account_contact_sync_configs SET last_success_at=CURRENT_TIMESTAMP,last_import_count=?,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND account_id=? AND enabled=1`, imported, snapshot.owner, snapshot.id)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE account_contact_sync_configs SET last_error=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND account_id=? AND enabled=1`, strings.TrimSpace(pullErr.Error()), snapshot.owner, snapshot.id)
		}
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return storage.ErrContactPublication
		}
		return tx.Commit()
	})
}
