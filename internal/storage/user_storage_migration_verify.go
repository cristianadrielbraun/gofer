package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
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
		return migrationVerifyDatabaseChecks(ctx, tx, true, false)
	})
}

// Both database files are opened with SQLite mode=ro. query_only is relaxed on
// this connection solely to build bounded TEMP proof sets; it does not make
// either persistent database writable. Nothing is repaired during this audit.
func migrationVerifyClosedCentral(ctx context.Context, sourcePath, path string, report UserStorageMigrationPreflight, verifyCredentials func(context.Context, *sql.Tx) error) (err error) {
	return migrationVerifyClosedCentralLayout(ctx, sourcePath, path, report, verifyCredentials, nil)
}

func migrationVerifyClosedCentralLayout(ctx context.Context, sourcePath, path string, report UserStorageMigrationPreflight, verifyCredentials func(context.Context, *sql.Tx) error, layout *UserStorageLayout) (err error) {
	if err := requireExistingDatabase(path); err != nil {
		return err
	}
	central, err := OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, central.Close()) }()
	return withUserStorageMigrationSource(ctx, sourcePath, central, func(conn *sql.Conn) (err error) {
		if _, err := conn.ExecContext(ctx, `PRAGMA query_only=OFF`); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, restoreErr := conn.ExecContext(cleanup, `PRAGMA query_only=ON`)
			err = errors.Join(err, restoreErr)
		}()
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, table := range report.Tables {
			if selection, selected := migrationRowSelection(table, ""); selected {
				targetSelection := "1"
				if table.Name == "sender_avatars" {
					targetSelection = `EXISTS(SELECT 1 FROM migration_source.sender_avatars original WHERE original.email_hash=r.email_hash)`
				}
				if err := migrationCompareFilteredRows(ctx, tx, table, selection, targetSelection, ""); err != nil {
					return err
				}
			}
			if table.Name == "web_push_subscriptions" && !migrationHasColumn(table, "revision") {
				var invalid bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.web_push_subscriptions WHERE typeof(revision)<>'text' OR length(revision)<>32 OR revision GLOB '*[^0-9a-f]*')`).Scan(&invalid); err != nil {
					return err
				}
				if invalid {
					return errors.New("final migration has an invalid generated push revision")
				}
			}
		}
		if err := migrationVerifyCentralRouting(ctx, tx); err != nil {
			return err
		}
		if err := migrationVerifyAvatarInterests(ctx, tx); err != nil {
			return err
		}
		if err := verifyCredentials(ctx, tx); err != nil {
			return err
		}
		generated := map[string]bool{
			"gofer_account_directory": true, "gofer_user_store_directory": true,
			"gofer_account_poll_schedule": true, "gofer_account_active_poll": true,
			"gofer_account_service_schedule": true, "gofer_contact_queue_schedule": true,
			"gofer_avatar_interests": true, "gofer_mailbox_credentials": true,
		}
		if layout != nil {
			if err := migrationCheckLayoutIdentity(ctx, tx, *layout); err != nil {
				return err
			}
			generated["gofer_storage_layout"] = true
		}
		if err := migrationDestinationBoundaryWithGenerated(ctx, tx, "", true, generated); err != nil {
			return err
		}
		if err := migrationSequences(ctx, tx, report, "", false); err != nil {
			return err
		}
		return migrationVerifyDatabaseChecks(ctx, tx, true, false)
	})
}

func migrationVerifyCentralRouting(ctx context.Context, tx *sql.Tx) error {
	active := ` FROM migration_source.accounts WHERE coalesce(is_deleting,0)=0`
	for _, relation := range []struct{ name, columns, source string }{
		{"gofer_account_directory", "account_id,user_id,state,created_at,updated_at", `SELECT id account_id,user_id,CASE WHEN coalesce(is_deleting,0)<>0 THEN 'deleting' ELSE 'active' END state,created_at,updated_at FROM migration_source.accounts`},
		{"gofer_user_store_directory", "user_id,state", `SELECT id user_id,'present' state FROM migration_source.users WHERE user_type='webmail' AND is_admin=0`},
		{"gofer_account_poll_schedule", "account_id,next_due_ms,revision", `SELECT id account_id,0 next_due_ms,0 revision` + active},
		{"gofer_account_active_poll", "account_id,next_due_ms,last_attempt_ns,failures,revision", `SELECT id account_id,0 next_due_ms,0 last_attempt_ns,0 failures,0 revision` + active},
		{"gofer_account_service_schedule", "account_id,service,next_due_ms,revision", `SELECT id account_id,'contacts' service,0 next_due_ms,0 revision` + active + ` UNION ALL SELECT id account_id,'calendar' service,0 next_due_ms,0 revision` + active},
		{"gofer_contact_queue_schedule", "user_id,next_due_ms,revision", `SELECT id user_id,0 next_due_ms,0 revision FROM migration_source.users WHERE user_type='webmail' AND is_admin=0`},
	} {
		if err := migrationCompareRelation(ctx, tx, relation.name, strings.Split(relation.columns, ","), relation.source, `SELECT * FROM main.`+quoteStoreIdentifier(relation.name)); err != nil {
			return err
		}
	}
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.gofer_user_store_directory WHERE typeof(created_at)<>'text' OR datetime(created_at) IS NULL OR updated_at IS NOT created_at)`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("final migration has invalid owner-directory timestamps")
	}
	return nil
}

// Compare relation values and storage classes as well as duplicate counts.
func migrationCompareRelation(ctx context.Context, tx *sql.Tx, name string, columns []string, source, target string) error {
	projection := migrationParityProjection(columns)
	invalid, err := migrationMultisetsDiffer(ctx, tx, `SELECT `+projection+` FROM (`+source+`) r`, `SELECT `+projection+` FROM (`+target+`) r`, len(columns))
	if err != nil {
		return fmt.Errorf("verify migration relation %s: %w", name, err)
	}
	if invalid {
		return fmt.Errorf("final migration relation parity failed: %s", name)
	}
	return nil
}

func migrationVerifyAvatarInterests(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE gofer_migration_avatar_audit(email_hash TEXT,user_id TEXT,email TEXT,PRIMARY KEY(email_hash,user_id)) WITHOUT ROWID`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.user_id,lower(trim(m.from_email)) email
 FROM migration_source.messages m JOIN migration_source.accounts a ON a.id=m.account_id
 JOIN migration_source.users u ON u.id=a.user_id
 WHERE coalesce(a.is_deleting,0)=0 AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0
 AND instr(m.from_email,'@')>1 ORDER BY a.user_id,email`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var owner, email string
		if err := rows.Scan(&owner, &email); err != nil {
			rows.Close()
			return err
		}
		email = strings.ToLower(strings.TrimSpace(email))
		hash := avatarresolver.GravatarHash(email)
		if hash == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO temp.gofer_migration_avatar_audit(email_hash,user_id,email) VALUES(?,?,?)`, hash, owner, email); err != nil {
			rows.Close()
			return err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if err := migrationCompareRelation(ctx, tx, "gofer_avatar_interests", []string{"email_hash", "user_id"}, `SELECT email_hash,user_id FROM temp.gofer_migration_avatar_audit`, `SELECT email_hash,user_id FROM main.gofer_avatar_interests`); err != nil {
		return err
	}
	// Original cache rows were already compared byte-for-byte. Newly discovered
	// senders must have only pending defaults, with no fabricated cache contents.
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sender_avatars c
 WHERE NOT EXISTS(SELECT 1 FROM migration_source.sender_avatars original WHERE original.email_hash=c.email_hash)
 AND (NOT EXISTS(SELECT 1 FROM temp.gofer_migration_avatar_audit expected WHERE expected.email_hash=c.email_hash AND expected.email=c.email)
 OR c.source IS NOT 'gravatar' OR c.status IS NOT 'pending' OR c.gravatar_status IS NOT 'unchecked' OR c.bimi_status IS NOT 'unchecked'
 OR c.gravatar_checked_at IS NOT NULL OR c.bimi_checked_at IS NOT NULL OR c.content_type IS NOT '' OR c.image_data IS NOT NULL OR c.storage_path IS NOT ''
 OR c.fetched_at IS NOT NULL OR c.expires_at IS NOT NULL OR c.next_retry_at IS NOT NULL OR c.error IS NOT ''
 OR typeof(c.created_at)<>'text' OR datetime(c.created_at) IS NULL OR c.updated_at IS NOT c.created_at))`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("final migration has unexpected avatar cache rows or contents")
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE temp.gofer_migration_avatar_audit`)
	return err
}
