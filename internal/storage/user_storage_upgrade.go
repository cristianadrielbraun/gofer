package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

// UpgradeUserStorageLayout brings a completed layout's central database and
// every present user database to CurrentSchemaVersion with the ordinary schema
// upgrades, then restores each user database's boundary guards. The caller
// holds the source and central runtime locks; the user manager lock is held
// here, so no user database is open. Upgrade steps commit with their version,
// so an interrupted upgrade resumes on the next start. Newer schemas, written
// by a later Gofer release, are rejected without changes.
func UpgradeUserStorageLayout(ctx context.Context, path string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	layout, err := ReadUserStorageLayoutMetadata(path)
	if err != nil {
		return err
	}
	if err := migrationRequireRegular(layout.CentralPath); err != nil {
		return err
	}
	if err := migrationRequireDirectory(layout.UserDirectory); err != nil {
		return err
	}
	lock, err := runtimeguard.Acquire(filepath.Join(layout.UserDirectory, "manager"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	// Never upgrade a database the layout does not certify.
	if err := checkLayoutIdentityFile(ctx, layout); err != nil {
		return err
	}
	if err := upgradeStorageDatabase(ctx, layout.CentralPath, ""); err != nil {
		return fmt.Errorf("upgrade central database: %w", err)
	}
	owners, err := presentUserStores(ctx, layout.CentralPath)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		hash := sha256.Sum256([]byte(owner))
		filename := filepath.Join(layout.UserDirectory, hex.EncodeToString(hash[:])+".db")
		if err := migrationRequireRegular(filename); err != nil {
			return err
		}
		if err := upgradeStorageDatabase(ctx, filename, owner); err != nil {
			return fmt.Errorf("upgrade user database %s: %w", filepath.Base(filename), err)
		}
	}
	return nil
}

func checkLayoutIdentityFile(ctx context.Context, layout UserStorageLayout) (err error) {
	db, err := openReadOnlyDB(layout.CentralPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return migrationCheckLayoutIdentity(ctx, tx, layout)
}

func presentUserStores(ctx context.Context, central string) (owners []string, err error) {
	db, err := openReadOnlyDB(central)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM gofer_user_store_directory WHERE state='present' ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// upgradeStorageDatabase upgrades one closed layout database in place. A user
// database must still belong to owner before any writer opens it.
func upgradeStorageDatabase(ctx context.Context, path, owner string) (err error) {
	version, err := layoutSchemaVersion(path, owner)
	if err != nil {
		return err
	}
	switch {
	case version == CurrentSchemaVersion:
		return nil
	case version > CurrentSchemaVersion:
		return fmt.Errorf("schema version %d was written by a newer Gofer release; this release supports %d", version, CurrentSchemaVersion)
	case version < 1:
		return fmt.Errorf("schema version %d cannot be upgraded", version)
	}
	log.Printf("storage: upgrading %s from schema version %d to %d", path, version, CurrentSchemaVersion)
	write, err := openDB(path)
	if err != nil {
		return err
	}
	write.SetMaxOpenConns(1)
	read, err := openDB(path)
	if err != nil {
		return errors.Join(err, write.Close())
	}
	db := &DB{write: write, read: read, path: path}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := db.migrate(); err != nil {
		return err
	}
	if err := db.requireCurrentSchema(); err != nil {
		return err
	}
	if owner == "" {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureUserStoreGuards(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func layoutSchemaVersion(path, owner string) (version int, err error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if owner != "" {
		if err := checkUserStoreOwner(db, owner); err != nil {
			return 0, err
		}
	}
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
