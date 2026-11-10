package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// ChangeRecorder records, inside a change's write transaction, what the
// change did: the admin console records administrative changes in the
// audit log this way. When the record can't be written, the change doesn't
// happen either.
type ChangeRecorder func(ctx context.Context, tx *sql.Tx) error

type changeRecorderKey struct{}

// WithChangeRecorder returns a context under which storage's administrative
// writes call record inside their transaction, when they change something.
func WithChangeRecorder(ctx context.Context, record ChangeRecorder) context.Context {
	return context.WithValue(ctx, changeRecorderKey{}, record)
}

// inChangeTx runs change in one write transaction with the context's change
// recorder. change reports whether it changed anything; a write that
// changed nothing, such as adding something already there, isn't recorded.
func (db *DB) inChangeTx(ctx context.Context, change func(*sql.Tx) (bool, error)) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := change(tx)
	if err != nil {
		return err
	}
	if changed {
		if err := recordChangeTx(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recordChangeTx runs the context's change recorder inside tx, for writes
// that manage their own transaction.
func recordChangeTx(ctx context.Context, tx *sql.Tx) error {
	if record, ok := ctx.Value(changeRecorderKey{}).(ChangeRecorder); ok && record != nil {
		return record(ctx, tx)
	}
	return nil
}

// rowsChanged reports whether result changed any row.
func rowsChanged(result sql.Result) (bool, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count changed rows: %w", err)
	}
	return count > 0, nil
}
