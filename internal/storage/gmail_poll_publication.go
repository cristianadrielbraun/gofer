package storage

import (
	"context"
	"database/sql"
	"errors"
)

// MarkOwnedGmailPollCheck seals the mailbox identity and enabled receiving in
// the same writer transaction as status publication. Account edits cannot fit
// between an earlier routed read and an unguarded INSERT/UPDATE.
func (db *DB) MarkOwnedGmailPollCheck(ctx context.Context, owner, subject string, state GmailPollState, changed bool, pollErr error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM accounts a JOIN users u ON u.id=a.user_id
 WHERE a.id=? AND a.user_id=? AND a.provider='gmail' AND a.provider_account_id=? AND a.auth_method='oauth2'
 AND COALESCE(a.email_sync_enabled,1)=1 AND COALESCE(a.is_deleting,0)=0
 AND u.status='active' AND u.deletion_pending=0`, state.AccountID, owner, subject).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountRoute
	}
	if err != nil {
		return err
	}
	if err := markGmailPollCheck(ctx, tx, state, changed, pollErr); err != nil {
		return err
	}
	return tx.Commit()
}
