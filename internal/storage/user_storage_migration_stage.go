package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

// Adapters authenticate source keys/files and import central grants. They must
// not contact providers, modify the source or retain borrowed database handles.
type UserStorageMigrationOptions struct {
	SourcePath        string
	DestinationPath   string
	WorkingDirectory  string
	Retry             bool
	ValidateSource    func(context.Context, *DB) error
	ImportCredentials func(context.Context, *sql.Tx) error
}

type UserStorageMigrationStage struct {
	Directory        string                        `json:"directory"`
	Source           UserStorageMigrationPreflight `json:"source"`
	Owners           int64                         `json:"owners_copied"`
	Files            UserStorageMigrationFiles     `json:"files"`
	WorkingDirectory string                        `json:"working_directory"`
	AttemptID        string                        `json:"attempt_id"`
}

// StageUserStorageMigration prepares private files, not a completed layout or
// activation command. Failed stages are retained and never overwritten. Final
// publication requires revalidating source/files/layout under runtime locks,
// or using the private engine under a coordinator that retains its locks.
func StageUserStorageMigration(ctx context.Context, options UserStorageMigrationOptions) (result UserStorageMigrationStage, err error) {
	if ctx == nil || options.ValidateSource == nil || options.ImportCredentials == nil {
		return result, errors.New("migration source and credential verifiers are required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	sourcePath, err := canonicalMigrationPath(options.SourcePath)
	if err != nil {
		return result, err
	}
	destinationPath, err := canonicalMigrationPath(options.DestinationPath)
	if err != nil {
		return result, err
	}
	if sourcePath == destinationPath || filepath.Dir(sourcePath) != filepath.Dir(destinationPath) {
		return result, errors.New("migration requires a different destination in the source data directory")
	}
	if err := requireExistingDatabase(sourcePath); err != nil {
		return result, err
	}
	sourceLock, err := runtimeguard.Acquire(sourcePath)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, sourceLock.Close()) }()
	destinationLock, err := runtimeguard.Acquire(destinationPath)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, destinationLock.Close()) }()
	for _, path := range []string{destinationPath, destinationPath + ".users", destinationPath + ".layout.json"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return result, errors.New("migration destination already exists or cannot be inspected")
		}
	}
	source, err := openUserStorageMigrationSource(ctx, sourcePath)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	report, err := inspectUserStorageMigration(ctx, source)
	if err != nil {
		return result, err
	}
	if err := options.ValidateSource(ctx, source); err != nil {
		return result, err
	}
	workingDirectory := options.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return result, err
		}
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return result, err
	}
	files, err := InspectUserStorageMigrationFiles(ctx, source, workingDirectory)
	if err != nil {
		return result, err
	}
	journal, err := migrationAttemptJournal(ctx, sourcePath, destinationPath, workingDirectory, files, options.Retry)
	if err != nil {
		return result, err
	}
	result, err = stageUserStorageMigration(ctx, source, journal.Directory, report, options.ImportCredentials, func(directory string) error {
		return writeMigrationPreparationJournal(filepath.Join(directory, "PREPARATION.json"), journal)
	})
	result.AttemptID = journal.ID
	result.Files, result.WorkingDirectory = files, workingDirectory
	if err != nil {
		return result, err
	}
	verified, err := InspectUserStorageMigrationFiles(ctx, source, workingDirectory)
	if err != nil {
		return result, err
	}
	if files != verified {
		return result, errors.New("migration source files changed during staging")
	}
	snapshot, err := migrationSnapshotSource(ctx, sourcePath)
	if err != nil {
		return result, err
	}
	if snapshot != journal.Source {
		return result, errors.New("migration source database changed during staging")
	}
	if err := migrationVerifyClosedUserStores(ctx, source, filepath.Join(result.Directory, "users"), report); err != nil {
		return result, err
	}
	if err := migrationSyncPrivateStage(ctx, result.Directory); err != nil {
		return result, err
	}
	journal.State, journal.Owners = "verified", result.Owners
	if err := writeMigrationPreparationJournal(destinationPath+".migration.json", journal); err != nil {
		return result, err
	}
	return result, nil
}

func canonicalMigrationPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("migration database path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func stageUserStorageMigration(ctx context.Context, source *DB, directory string, report UserStorageMigrationPreflight, importCredentials func(context.Context, *sql.Tx) error, bindPreparation func(string) error) (result UserStorageMigrationStage, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return result, fmt.Errorf("create private migration stage: %w", err)
	}
	result.Directory, result.Source = directory, report
	if err := bindPreparation(directory); err != nil {
		return result, err
	}
	// Written before any database; it never certifies successful migration.
	marker, err := os.OpenFile(filepath.Join(directory, "INCOMPLETE"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	_, writeErr := marker.WriteString("Unpublished per-user database migration\n")
	if err := errors.Join(writeErr, marker.Sync(), marker.Close()); err != nil {
		return result, err
	}
	centralPath := filepath.Join(directory, "central.db")
	file, err := os.OpenFile(centralPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	if err := file.Close(); err != nil {
		return result, err
	}
	central, err := New(centralPath)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, central.Close()) }()
	if err := copyUserStorageMigrationRows(ctx, source.Path(), central, report, ""); err != nil {
		return result, err
	}
	stores, err := NewUserStores(central, UserStoreOptions{Directory: filepath.Join(directory, "users"), MaxOpen: 1})
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = errors.Join(err, stores.Close(cleanup))
	}()
	if _, err := NewAccountRouting(stores); err != nil {
		return result, err
	}
	cursor := ""
	for {
		owners, err := migrationOwnerPage(ctx, source, cursor)
		if err != nil {
			return result, err
		}
		if len(owners) == 0 {
			break
		}
		for _, owner := range owners {
			lease, err := stores.Acquire(ctx, owner)
			if err != nil {
				return result, err
			}
			err = copyUserStorageMigrationRows(ctx, source.Path(), lease.DB(), report, owner)
			lease.Release()
			if err != nil {
				return result, err
			}
			result.Owners++
		}
		cursor = owners[len(owners)-1]
	}
	if result.Owners != report.Owners {
		return result, errors.New("migration stage owner parity failed")
	}
	err = withUserStorageMigrationSource(ctx, source.Path(), central, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.gofer_account_directory(account_id,user_id,state,created_at,updated_at)
 SELECT id,user_id,CASE WHEN coalesce(is_deleting,0)<>0 THEN 'deleting' ELSE 'active' END,created_at,updated_at FROM migration_source.accounts;
 INSERT INTO main.gofer_account_poll_schedule(account_id) SELECT account_id FROM main.gofer_account_directory WHERE state='active';
 INSERT INTO main.gofer_account_active_poll(account_id) SELECT account_id FROM main.gofer_account_directory WHERE state='active';
 INSERT INTO main.gofer_account_service_schedule(account_id,service) SELECT account_id,'contacts' FROM main.gofer_account_directory WHERE state='active';
 INSERT INTO main.gofer_account_service_schedule(account_id,service) SELECT account_id,'calendar' FROM main.gofer_account_directory WHERE state='active';
 INSERT INTO main.gofer_contact_queue_schedule(user_id) SELECT id FROM main.users WHERE user_type='webmail' AND is_admin=0;`); err != nil {
			return err
		}
		var accounts, owners int64
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM gofer_account_directory),(SELECT count(*) FROM gofer_user_store_directory WHERE state='present')`).Scan(&accounts, &owners); err != nil {
			return err
		}
		if accounts != report.Accounts || owners != report.Owners {
			return errors.New("migration directory parity failed")
		}
		if err := migrationBuildAvatarInterests(ctx, tx); err != nil {
			return err
		}
		if err := importCredentials(ctx, tx); err != nil {
			return err
		}
		if err := migrationVerifyCopiedDatabase(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	})
	return result, err
}

func migrationBuildAvatarInterests(ctx context.Context, tx *sql.Tx) error {
	// These are discovery hints only. Use the existing automatic sender scan's
	// normalization and account eligibility, retaining all original cache rows.
	// The temporary expected set keeps verification bounded by SQLite, not an
	// in-memory set of every user/sender pair.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE gofer_migration_avatar_expected(email_hash TEXT,user_id TEXT,PRIMARY KEY(email_hash,user_id)) WITHOUT ROWID`); err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.sender_avatars(email_hash,email,status) VALUES(?,?,'pending') ON CONFLICT(email_hash) DO NOTHING`, hash, email); err != nil {
			rows.Close()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.gofer_avatar_interests(email_hash,user_id) VALUES(?,?)`, hash, owner); err != nil {
			rows.Close()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO temp.gofer_migration_avatar_expected(email_hash,user_id) VALUES(?,?)`, hash, owner); err != nil {
			rows.Close()
			return err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"main.gofer_avatar_interests", "temp.gofer_migration_avatar_expected"}, {"temp.gofer_migration_avatar_expected", "main.gofer_avatar_interests"}} {
		var invalid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT email_hash,user_id FROM `+pair[0]+` EXCEPT SELECT email_hash,user_id FROM `+pair[1]+`)`).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return errors.New("migration avatar discovery parity failed")
		}
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE temp.gofer_migration_avatar_expected`)
	return err
}

func migrationOwnerPage(ctx context.Context, source *DB, after string) ([]string, error) {
	rows, err := source.Read().QueryContext(ctx, `SELECT id FROM users WHERE user_type='webmail' AND is_admin=0 AND id>? ORDER BY id LIMIT 64`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}
