package storage

import (
	"context"
	"database/sql"
	"time"
)

// Trusted local housekeeping covers retained disabled owners too. It takes no
// provider capability, creates no unused/missing store and excludes deletion.
func (r *AccountRouting) PruneRetainedUserMailJobs(ctx context.Context, owner string, now time.Time, batch int) (result DurableJobPruneResult, err error) {
	if batch < 1 || batch > RetentionBatchSize {
		return result, ErrUserStoreIdentity
	}
	err = r.withStartupStore(ctx, owner, func(db *DB) error {
		guard := func(tx *sql.Tx) error {
			if err := r.validateStartupStoreOwner(ctx, owner); err != nil {
				return err
			}
			var actual string
			if err := tx.QueryRowContext(ctx, `SELECT user_id FROM gofer_user_store WHERE singleton=1 AND layout_version=1`).Scan(&actual); err != nil {
				return err
			}
			if actual != owner {
				return ErrUserStoreIdentity
			}
			return nil
		}
		var err error
		result, err = db.pruneDurableMailJobs(ctx, now, batch, guard)
		return err
	})
	return result, err
}
