package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This is the row-copy stage, not layout publication. The offline coordinator
// must hold the source/runtime locks, inspect the source, create private fresh
// destinations and retain those locks through final verification/publication.
// Credentials and external blobs require separate authenticated verification.
func copyUserStorageMigrationRows(ctx context.Context, sourcePath string, destination *DB, report UserStorageMigrationPreflight, owner string) (err error) {
	return withUserStorageMigrationSource(ctx, sourcePath, destination, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
			return err
		}
		if err := migrationFreshDestination(ctx, tx, owner); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM migration_source.schema_version`).Scan(&version); err != nil {
			return err
		}
		if version != report.SchemaVersion || !migrationSharedVersionSupported(version) || report.TargetSchemaVersion != CurrentSchemaVersion {
			return migrationSourceError("schema_version", "source inspection does not match the copy")
		}
		if owner != "" {
			var valid bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_source.users WHERE id=? AND user_type='webmail' AND is_admin=0)`, owner).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return ErrUserStoreOwner
			}
			// Keep only the non-authenticating profile stub. Disabled/deletion
			// state and administrative actor links remain authoritative centrally.
			if _, err := tx.ExecContext(ctx, `UPDATE main.users SET (username,username_normalized,name,avatar_url)=
 (SELECT username,username_normalized,name,avatar_url FROM migration_source.users WHERE id=?) WHERE id=?`, owner, owner); err != nil {
				return err
			}
		}
		// Deferred foreign keys retain cycles and composite relationships without
		// replacing rows, renumbering IDs or passing SQLite values through Go.
		for _, table := range report.Tables {
			selection, selected := migrationRowSelection(table, owner)
			if !selected {
				continue
			}
			columns := migrationColumnList(table.Columns, "")
			if table.Destination == migrationSearch {
				columns = `rowid,` + columns
			}
			query := `INSERT INTO main.` + quoteStoreIdentifier(table.Name) + `(` + columns + `) SELECT ` + columns + ` FROM migration_source.` + quoteStoreIdentifier(table.Name) + ` r WHERE ` + selection
			if _, err := tx.ExecContext(ctx, query, migrationSelectionArguments(owner)...); err != nil {
				return fmt.Errorf("copy migration table %s: %w", table.Name, err)
			}
		}
		if err := migrationSequences(ctx, tx, report, owner, true); err != nil {
			return err
		}
		if owner == "" {
			// A missing historic revision gets a fresh destination identity. An
			// existing registration revision is copied byte for byte above.
			for _, table := range report.Tables {
				if table.Name == "web_push_subscriptions" && !migrationHasColumn(table, "revision") {
					if _, err := tx.ExecContext(ctx, `UPDATE web_push_subscriptions SET revision=lower(hex(randomblob(16))) WHERE revision=''`); err != nil {
						return err
					}
				}
			}
		}
		// Verify after every insertion/transform, so a later trigger cannot
		// silently alter a table that already passed comparison.
		for _, table := range report.Tables {
			if selection, selected := migrationRowSelection(table, owner); selected {
				if err := migrationCompareRows(ctx, tx, table, selection, owner); err != nil {
					return err
				}
			}
		}
		if owner != "" {
			stub := UserStorageMigrationTable{Name: "users", Columns: []string{"id", "username", "username_normalized", "name", "avatar_url"}}
			if err := migrationCompareRows(ctx, tx, stub, `r.id=?`, owner); err != nil {
				return err
			}
		}
		if err := migrationDestinationBoundary(ctx, tx, owner, true); err != nil {
			return err
		}
		if err := migrationSequences(ctx, tx, report, owner, false); err != nil {
			return err
		}
		if err := migrationVerifyCopiedDatabase(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func withUserStorageMigrationSource(ctx context.Context, sourcePath string, destination *DB, fn func(*sql.Conn) error) (err error) {
	if err := requireExistingDatabase(sourcePath); err != nil {
		return err
	}
	if destination == nil {
		return errors.New("migration destination is required")
	}
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	dst, err := filepath.Abs(destination.Path())
	if err != nil {
		return err
	}
	if abs == dst {
		return errors.New("migration destination must differ from source")
	}
	sourceInfo, err := os.Stat(abs)
	if err != nil {
		return err
	}
	destinationInfo, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if os.SameFile(sourceInfo, destinationInfo) {
		return errors.New("migration destination aliases source")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro"}
	conn, err := destination.Write().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var temporaryStorage int
	if err := conn.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&temporaryStorage); err != nil {
		return err
	}
	// Sorting full-content parity groups can exceed RAM for large mailboxes.
	// Let SQLite spill offline verification work to private temporary files.
	if _, err := conn.ExecContext(ctx, `PRAGMA temp_store=FILE`); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, restoreErr := conn.ExecContext(cleanup, fmt.Sprintf(`PRAGMA temp_store=%d`, temporaryStorage)); restoreErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("restore migration temporary storage: %w", restoreErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS migration_source`, uri.String()); err != nil {
		return err
	}
	defer func() {
		// Cancellation must not return an attached source to the write pool.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, detachErr := conn.ExecContext(cleanup, `DETACH DATABASE migration_source`); detachErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("detach migration source: %w", detachErr))
		}
	}()
	return fn(conn)
}

func migrationFreshDestination(ctx context.Context, tx *sql.Tx, owner string) error {
	return migrationDestinationBoundary(ctx, tx, owner, false)
}

func migrationDestinationBoundary(ctx context.Context, tx *sql.Tx, owner string, copied bool) error {
	return migrationDestinationBoundaryWithGenerated(ctx, tx, owner, copied, nil)
}

func migrationDestinationBoundaryWithGenerated(ctx context.Context, tx *sql.Tx, owner string, copied bool, generated map[string]bool) error {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM main.schema_version`).Scan(&version); err != nil {
		return err
	}
	if version != CurrentSchemaVersion {
		return ErrUserStoreIdentity
	}
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='gofer_user_store')`).Scan(&owned); err != nil {
		return err
	}
	if owned != (owner != "") {
		return ErrUserStoreIdentity
	}
	if owned {
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM gofer_user_store WHERE singleton=1 AND layout_version=1`).Scan(&actual); err != nil {
			return err
		}
		if actual != owner {
			return ErrUserStoreIdentity
		}
		var valid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.users WHERE id=? AND user_type='webmail' AND is_admin=0)`, owner).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrUserStoreIdentity
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		rule, known := userStorageMigrationTables[name]
		if !known {
			return fmt.Errorf("migration destination has an unknown table: %s", name)
		}
		if rule.destination == migrationRetiredSearch || rule.destination == migrationRetiredEmpty {
			return fmt.Errorf("migration destination contains a retired search table: %s", name)
		}
		if generated != nil {
			policy := UserStorageMigrationTable{Name: name, Columns: strings.Split(rule.columns, ",")}
			if err := migrationSourceColumns(ctx, tx, &policy, CurrentSchemaVersion); err != nil {
				return err
			}
		}
		if rule.destination == migrationSchema || rule.destination == migrationSearchShadow || rule.destination == migrationPlanner {
			continue
		}
		if copied && rule.destination == migrationSequence {
			continue // Exact high-water entries are verified separately.
		}
		if _, selected := migrationRowSelection(UserStorageMigrationTable{Name: name, Destination: rule.destination}, owner); copied && selected {
			continue
		}
		if generated[name] {
			continue
		} // Verified against exact source-derived relations separately.
		want := 0
		if owned && (name == "users" || name == "gofer_user_store") {
			want = 1
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM main.`+quoteStoreIdentifier(name)).Scan(&count); err != nil {
			return err
		}
		if count != want {
			return fmt.Errorf("migration destination is not fresh: %s", name)
		}
	}
	return nil
}

func migrationRowSelection(table UserStorageMigrationTable, owner string) (string, bool) {
	if owner == "" {
		switch table.Destination {
		case migrationCentral, migrationUsers, migrationLegacySenders:
			return `1`, true
		case migrationPreferences:
			return `EXISTS(SELECT 1 FROM migration_source.users u WHERE u.id=r.user_id AND u.user_type='management')`, true
		}
		return "", false
	}
	switch table.Destination {
	case migrationSearch:
		// Retire stale derived rows only in the destination. Existing messages'
		// account bindings passed preflight and retain exact content comparison.
		return `EXISTS(SELECT 1 FROM migration_source.messages m WHERE m.id=r.rowid AND m.account_id IS r.account_id) AND (` + userStorageMigrationTables[table.Name].ownerSQL("migration_source", "r") + `)=?`, true
	case migrationLocal, migrationPreferences:
		return `(` + userStorageMigrationTables[table.Name].ownerSQL("migration_source", "r") + `)=?`, true
	case migrationLegacySenders:
		// Evaluate the original shared-layout permission, before partitioning.
		// An inert global legacy row must never become a new per-user grant.
		return `EXISTS(SELECT 1 FROM migration_source.messages m
 JOIN migration_source.accounts a ON a.id=m.account_id
 JOIN migration_source.remote_content_messages x ON x.message_id=m.id
 WHERE lower(m.from_email)=r.sender_email AND a.user_id=? AND coalesce(a.is_deleting,0)=0
 AND NOT EXISTS(SELECT 1 FROM migration_source.accounts other WHERE other.user_id IS NOT a.user_id AND coalesce(other.is_deleting,0)=0))`, true
	}
	return "", false
}

func migrationSelectionArguments(owner string) []any {
	if owner == "" {
		return nil
	}
	return []any{owner}
}

func migrationHasColumn(table UserStorageMigrationTable, column string) bool {
	for _, name := range table.Columns {
		if name == column {
			return true
		}
	}
	return false
}

func migrationColumnList(columns []string, alias string) string {
	parts := make([]string, len(columns))
	for i, name := range columns {
		parts[i] = alias + quoteStoreIdentifier(name)
	}
	return strings.Join(parts, ",")
}

// Compare storage classes, binary values and multiplicities inside SQLite.
// EXCEPT alone would hide duplicate-count changes; Go scanning DATETIME values
// would normalize historic representations. No mailbox value enters the error.
func migrationCompareRows(ctx context.Context, tx *sql.Tx, table UserStorageMigrationTable, selection, owner string) error {
	return migrationCompareFilteredRows(ctx, tx, table, selection, "1", owner)
}

func migrationCompareFilteredRows(ctx context.Context, tx *sql.Tx, table UserStorageMigrationTable, selection, targetSelection, owner string) error {
	columns := append([]string(nil), table.Columns...)
	if table.Destination == migrationSearch {
		columns = append([]string{"rowid"}, columns...)
	}
	var expressions []string
	for _, column := range columns {
		name := `r.` + quoteStoreIdentifier(column)
		expressions = append(expressions, `typeof(`+name+`)`, name+` COLLATE BINARY`)
	}
	projection := strings.Join(expressions, ",")
	group := projection + `,count(*)`
	source := `SELECT ` + group + ` FROM migration_source.` + quoteStoreIdentifier(table.Name) + ` r WHERE ` + selection + ` GROUP BY ` + projection
	target := `SELECT ` + group + ` FROM main.` + quoteStoreIdentifier(table.Name) + ` r WHERE ` + targetSelection + ` GROUP BY ` + projection
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		var mismatch bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT * FROM (`+pair[0]+` EXCEPT `+pair[1]+`))`, migrationSelectionArguments(owner)...).Scan(&mismatch); err != nil {
			return fmt.Errorf("compare migration table %s: %w", table.Name, err)
		}
		if mismatch {
			return fmt.Errorf("migration content parity failed: %s", table.Name)
		}
	}
	return nil
}

func migrationSequences(ctx context.Context, tx *sql.Tx, report UserStorageMigrationPreflight, owner string, write bool) error {
	present := make(map[string]bool)
	for _, table := range report.Tables {
		present[table.Name] = true
	}
	var expected []any
	for table, primaryKey := range userStorageMigrationSequences {
		if !present[table] {
			continue
		}
		if _, selected := migrationRowSelection(UserStorageMigrationTable{Name: table, Destination: userStorageMigrationTables[table].destination}, owner); !selected {
			continue
		}
		expected = append(expected, table)
		var highWater int64
		query := `SELECT max(coalesce((SELECT max(seq) FROM migration_source.sqlite_sequence WHERE name=?),0),coalesce((SELECT max(` + quoteStoreIdentifier(primaryKey) + `) FROM migration_source.` + quoteStoreIdentifier(table) + `),0),0)`
		if err := tx.QueryRowContext(ctx, query, table).Scan(&highWater); err != nil {
			return err
		}
		if write {
			if _, err := tx.ExecContext(ctx, `DELETE FROM main.sqlite_sequence WHERE name=?`, table); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO main.sqlite_sequence(name,seq) VALUES(?,?)`, table, highWater); err != nil {
				return err
			}
		} else {
			var matches, entries int
			if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(typeof(seq)='integer' AND seq=?),0) FROM main.sqlite_sequence WHERE name=?`, highWater, table).Scan(&entries, &matches); err != nil {
				return err
			}
			if entries != 1 || matches != 1 {
				return fmt.Errorf("migration high-water parity failed: %s", table)
			}
		}
	}
	if !write {
		condition := `1`
		if len(expected) != 0 {
			condition = `name IS NULL OR name NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(expected)), ",") + `)`
		}
		var unexpected bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_sequence WHERE `+condition+`)`, expected...).Scan(&unexpected); err != nil {
			return err
		}
		if unexpected {
			return errors.New("migration destination has an unexpected high-water entry")
		}
	}
	return nil
}

func migrationVerifyCopiedDatabase(ctx context.Context, tx *sql.Tx) error {
	return migrationVerifyDatabaseChecks(ctx, tx, true)
}

func migrationVerifyDatabaseChecks(ctx context.Context, tx *sql.Tx, checkFTS bool) error {
	var result string
	if err := tx.QueryRowContext(ctx, `PRAGMA main.integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("migration destination integrity check failed")
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA main.foreign_key_check`)
	if err != nil {
		return err
	}
	invalid := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if invalid {
		return errors.New("migration destination foreign-key check failed")
	}
	if !checkFTS {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO main.message_search(message_search) VALUES('integrity-check')`)
	return err
}
