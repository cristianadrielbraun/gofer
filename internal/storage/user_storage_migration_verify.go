package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
)

// Reopen closed stores read-only after all metadata/import work. This detects a
// late change to an earlier owner without running migrations or rebuilding a
// missing file. The coordinator keeps source/destination runtime locks throughout.
func migrationVerifyClosedUserStores(ctx context.Context, source *DB, directory string, report UserStorageMigrationPreflight) error {
	cursor := ""
	var checked int64
	for {
		owners, err := migrationOwnerPage(ctx, source, cursor)
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			break
		}
		for _, owner := range owners {
			hash := sha256.Sum256([]byte(owner))
			path := filepath.Join(directory, hex.EncodeToString(hash[:])+".db")
			if err := checkUserStoreIdentity(path, owner); err != nil {
				return err
			}
			if err := migrationVerifyClosedOwner(ctx, source.Path(), path, owner, report); err != nil {
				return err
			}
			checked++
		}
		cursor = owners[len(owners)-1]
	}
	if checked != report.Owners {
		return errors.New("final migration owner-store count does not match source")
	}
	return nil
}

func migrationVerifyClosedOwner(ctx context.Context, sourcePath, path, owner string, report UserStorageMigrationPreflight) (err error) {
	if err := requireExistingDatabase(path); err != nil {
		return err
	}
	local, err := OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, local.Close()) }()
	return withUserStorageMigrationSource(ctx, sourcePath, local, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, table := range report.Tables {
			if selection, selected := migrationRowSelection(table, owner); selected {
				if err := migrationCompareRows(ctx, tx, table, selection, owner); err != nil {
					return err
				}
			}
			if table.Name == "calendar_response_requests" && !migrationHasColumn(table, "claim_id") {
				var invalid bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.calendar_response_requests WHERE claim_id<>'')`).Scan(&invalid); err != nil {
					return err
				}
				if invalid {
					return errors.New("final migration adopted a claim absent from the source")
				}
			}
		}
		stub := UserStorageMigrationTable{Name: "users", Columns: []string{"id", "username", "username_normalized", "name", "avatar_url"}}
		if err := migrationCompareRows(ctx, tx, stub, `r.id=?`, owner); err != nil {
			return err
		}
		if err := migrationDestinationBoundary(ctx, tx, owner, true); err != nil {
			return err
		}
		if err := migrationSequences(ctx, tx, report, owner, false); err != nil {
			return err
		}
		// The copy transaction already ran the native FTS integrity command.
		// Here full SQLite/FK checks and exact FTS row/content parity are read-only.
		return migrationVerifyDatabaseChecks(ctx, tx, false)
	})
}
