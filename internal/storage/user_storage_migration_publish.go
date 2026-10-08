package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

type migrationPublicationProof struct {
	Version       int    `json:"version"`
	CentralDigest string `json:"central_digest"`
	CentralBytes  int64  `json:"central_bytes"`
	UsersDigest   string `json:"users_digest"`
	UsersBytes    int64  `json:"users_bytes"`
	UsersFiles    int64  `json:"users_files"`
}

// PublishUserStorageMigration revalidates a verified private preparation under
// both runtime locks, then publishes closed files and writes the manifest last.
// Explicit retry resumes a publishing journal; it never merges arbitrary files
// or rewrites an already completed database to match the original source.
func PublishUserStorageMigration(ctx context.Context, options UserStorageMigrationOptions) (UserStorageLayout, error) {
	return publishUserStorageMigration(ctx, options, nil)
}

func publishUserStorageMigration(ctx context.Context, options UserStorageMigrationOptions, afterStep func(string) error) (layout UserStorageLayout, err error) {
	if ctx == nil || options.ValidateSource == nil || options.VerifyCredentials == nil {
		return layout, errors.New("publication requires source and credential verifiers")
	}
	if err := ctx.Err(); err != nil {
		return layout, err
	}
	sourcePath, err := canonicalMigrationPath(options.SourcePath)
	if err != nil {
		return layout, err
	}
	destination, err := canonicalMigrationPath(options.DestinationPath)
	if err != nil {
		return layout, err
	}
	if sourcePath == destination || filepath.Dir(sourcePath) != filepath.Dir(destination) {
		return layout, errors.New("publication requires distinct databases in the original data directory")
	}
	sourceLock, err := runtimeguard.Acquire(sourcePath)
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, sourceLock.Close()) }()
	destinationLock, err := runtimeguard.Acquire(destination)
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, destinationLock.Close()) }()
	journal, err := readMigrationPreparationJournal(destination + ".migration.json")
	if err != nil {
		return layout, err
	}
	cwd := options.WorkingDirectory
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return layout, err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return layout, err
	}
	if journal.Version != 1 || journal.SourcePath != sourcePath || journal.DestinationPath != destination || journal.WorkingDirectory != cwd || !validMigrationAttemptID(journal.ID) || !validMigrationAttemptDirectory(destination, journal.Directory, journal.ID) || journal.Owners < 0 {
		return layout, errors.New("publication does not match its recorded preparation")
	}
	if journal.State != "verified" && journal.State != "publishing" && journal.State != "published" {
		return layout, errors.New("migration preparation has not passed final verification")
	}
	if journal.State != "verified" && !options.Retry {
		return layout, errors.New("interrupted or completed publication requires explicit retry")
	}
	if _, statErr := os.Lstat(destination + ".layout.json"); statErr == nil {
		if journal.State == "verified" {
			return layout, errors.New("unrecognized completed destination already exists")
		}
		if err := migrationRequireDirectory(destination + ".users"); err != nil {
			return layout, err
		}
		managerLock, err := runtimeguard.Acquire(filepath.Join(destination+".users", "manager"))
		if err != nil {
			return layout, err
		}
		defer func() { err = errors.Join(err, managerLock.Close()) }()
		layout, err = LoadUserStorageLayout(ctx, destination)
		if err != nil {
			return layout, err
		}
		if layout.ID != journal.ID || layout.SourcePath != sourcePath || layout.WorkingDirectory != cwd || layout.OwnersAtMigration != journal.Owners {
			return layout, errors.New("completed layout differs from its publication journal")
		}
		journal.State = "published"
		return layout, writeMigrationPreparationJournal(destination+".migration.json", journal)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return layout, statErr
	}
	if journal.State == "published" {
		return layout, errors.New("completed publication manifest is missing")
	}
	if journal.State == "verified" {
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Lstat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
				return layout, errors.New("publication destination has an unrecognized SQLite sidecar")
			}
		}
	}
	if err := migrationRequireDirectory(journal.Directory); err != nil {
		return layout, err
	}
	bound, err := readMigrationPreparationJournal(filepath.Join(journal.Directory, "PREPARATION.json"))
	if err != nil {
		return layout, err
	}
	expected := journal
	expected.State, expected.Owners, expected.Publication = "preparing", 0, migrationPublicationProof{}
	if bound != expected {
		return layout, errors.New("private publication directory does not match its preparation")
	}
	if err := migrationRequireRegular(filepath.Join(journal.Directory, "INCOMPLETE")); err != nil {
		return layout, err
	}
	privateCentral := filepath.Join(journal.Directory, "central.db")
	privateUsers := filepath.Join(journal.Directory, "users")
	centralPath, err := migrationPublicationPath(privateCentral, destination, false, journal.State == "publishing")
	if err != nil {
		return layout, err
	}
	usersPath, err := migrationPublicationPath(privateUsers, destination+".users", true, journal.State == "publishing")
	if err != nil {
		return layout, err
	}
	managerLock, err := runtimeguard.Acquire(filepath.Join(usersPath, "manager"))
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, managerLock.Close()) }()
	if err := migrationRequireRegular(sourcePath); err != nil {
		return layout, err
	}
	source, err := openUserStorageMigrationSource(ctx, sourcePath)
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	report, err := inspectUserStorageMigration(ctx, source)
	if err != nil {
		return layout, err
	}
	if report.Owners != journal.Owners {
		return layout, errors.New("publication owner count differs from preparation")
	}
	if err := options.ValidateSource(ctx, source); err != nil {
		return layout, err
	}
	if err := migrationVerifyPublicationSource(ctx, source, journal); err != nil {
		return layout, err
	}
	if err := migrationRequireClosedDatabase(centralPath); err != nil {
		return layout, err
	}
	if _, _, _, err := migrationPublicationUsers(ctx, usersPath); err != nil {
		return layout, err
	}
	if err := migrationVerifyClosedUserStores(ctx, source, usersPath, report); err != nil {
		return layout, err
	}
	layout = UserStorageLayout{Version: 1, ID: journal.ID, SourcePath: sourcePath, CentralPath: destination, UserDirectory: destination + ".users", BlobDirectory: filepath.Join(filepath.Dir(sourcePath), "accounts"), WorkingDirectory: cwd, SourceSchemaVersion: report.SchemaVersion, SchemaVersion: CurrentSchemaVersion, OwnersAtMigration: report.Owners}
	marker, err := migrationHasLayoutIdentity(ctx, centralPath)
	if err != nil {
		return layout, err
	}
	var existingLayout *UserStorageLayout
	if marker {
		existingLayout = &layout
	}
	if journal.State == "publishing" && !marker {
		return layout, errors.New("publishing database has no layout identity")
	}
	if err := migrationVerifyClosedCentralLayout(ctx, sourcePath, centralPath, report, options.VerifyCredentials, existingLayout); err != nil {
		return layout, err
	}
	if !marker {
		if err := migrationWriteLayoutIdentity(ctx, centralPath, layout); err != nil {
			return layout, err
		}
	}
	proof, err := migrationSnapshotPublication(ctx, centralPath, usersPath)
	if err != nil {
		return layout, err
	}
	if proof.UsersFiles != report.Owners {
		return layout, errors.New("publication contains unexpected user database files")
	}
	if journal.State == "publishing" && journal.Publication != proof {
		return layout, errors.New("interrupted publication files differ from the verified copies")
	}
	// Recheck source and blobs after every destination verifier and before moves.
	if err := migrationVerifyPublicationSource(ctx, source, journal); err != nil {
		return layout, err
	}
	if err := migrationSyncPrivateStage(ctx, journal.Directory); err != nil {
		return layout, err
	}
	if usersPath != privateUsers {
		if err := migrationSyncPrivateStage(ctx, usersPath); err != nil {
			return layout, err
		}
	}
	if err := migrationSyncFile(centralPath); err != nil {
		return layout, err
	}
	journal.State, journal.Publication = "publishing", proof
	if err := writeMigrationPreparationJournal(destination+".migration.json", journal); err != nil {
		return layout, err
	}
	checkpoint := func(step string) error {
		if afterStep != nil {
			if err := afterStep(step); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := checkpoint("journal"); err != nil {
		return layout, err
	}
	if centralPath == privateCentral {
		if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
			if err := os.Link(privateCentral, destination); err != nil {
				return layout, err
			}
		} else if err != nil {
			return layout, err
		} // A recognized interrupted hard link was checked above.
		if err := migrationSyncDirectory(filepath.Dir(destination)); err != nil {
			return layout, err
		}
		if err := checkpoint("central-link"); err != nil {
			return layout, err
		}
		if err := os.Remove(privateCentral); err != nil {
			return layout, err
		}
		if err := migrationSyncDirectory(journal.Directory); err != nil {
			return layout, err
		}
	}
	if err := checkpoint("central-moved"); err != nil {
		return layout, err
	}
	if usersPath == privateUsers {
		if _, err := os.Lstat(destination + ".users"); !errors.Is(err, os.ErrNotExist) {
			return layout, errors.New("user-store destination appeared during publication")
		}
		// Windows prevents renaming a directory containing an open lock file.
		// Keep both database runtime locks; actual activation requires those locks
		// and a completed manifest. Reacquire the moved manager lock before publication.
		if runtime.GOOS == "windows" {
			if err := managerLock.Close(); err != nil {
				return layout, err
			}
		}
		if err := os.Rename(privateUsers, destination+".users"); err != nil {
			return layout, err
		}
		if runtime.GOOS == "windows" {
			managerLock, err = runtimeguard.Acquire(filepath.Join(destination+".users", "manager"))
			if err != nil {
				return layout, err
			}
		}
		if err := migrationSyncDirectory(filepath.Dir(destination)); err != nil {
			return layout, err
		}
		if err := migrationSyncDirectory(journal.Directory); err != nil {
			return layout, err
		}
	}
	if err := checkpoint("users-moved"); err != nil {
		return layout, err
	}
	finalProof, err := migrationSnapshotPublication(ctx, destination, destination+".users")
	if err != nil {
		return layout, err
	}
	if finalProof != proof {
		return layout, errors.New("published file copies differ from the verified preparation")
	}
	if err := migrationValidateLayoutPaths(layout, destination); err != nil {
		return layout, err
	}
	if err := checkpoint("before-manifest"); err != nil {
		return layout, err
	}
	if err := migrationPublishJSON(destination+".layout.json", layout); err != nil {
		return layout, err
	}
	if err := checkpoint("manifest"); err != nil {
		return layout, err
	}
	journal.State = "published"
	if err := writeMigrationPreparationJournal(destination+".migration.json", journal); err != nil {
		return layout, err
	}
	return layout, nil
}

func migrationVerifyPublicationSource(ctx context.Context, source *DB, journal migrationPreparationJournal) error {
	snapshot, err := migrationSnapshotSource(ctx, source.Path())
	if err != nil {
		return err
	}
	if snapshot != journal.Source {
		return errors.New("original database changed since migration preparation")
	}
	files, err := InspectUserStorageMigrationFiles(ctx, source, journal.WorkingDirectory)
	if err != nil {
		return err
	}
	if files != journal.Files {
		return errors.New("original files changed since migration preparation")
	}
	return nil
}

// The only accepted double file name is our own interrupted hard link. Existing
// directories are never adopted just because they are empty or look similar.
func migrationPublicationPath(private, final string, directory, resume bool) (string, error) {
	original, originalErr := os.Lstat(private)
	published, publishedErr := os.Lstat(final)
	if originalErr != nil && !errors.Is(originalErr, os.ErrNotExist) {
		return "", originalErr
	}
	if publishedErr != nil && !errors.Is(publishedErr, os.ErrNotExist) {
		return "", publishedErr
	}
	if publishedErr == nil && !resume {
		return "", errors.New("publication destination already exists")
	}
	check := migrationRequireRegular
	if directory {
		check = migrationRequireDirectory
	}
	if originalErr == nil {
		if err := check(private); err != nil {
			return "", err
		}
	}
	if publishedErr == nil {
		if err := check(final); err != nil {
			return "", err
		}
	}
	if originalErr == nil && publishedErr == nil {
		if directory || !os.SameFile(original, published) {
			return "", errors.New("publication has conflicting private and destination paths")
		}
		return private, nil
	}
	if originalErr == nil {
		return private, nil
	}
	if publishedErr == nil {
		return final, nil
	}
	return "", errors.New("publication copy is missing from both private and final locations")
}

func migrationHasLayoutIdentity(ctx context.Context, path string) (present bool, err error) {
	db, err := OpenReadOnly(path)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	err = db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='gofer_storage_layout')`).Scan(&present)
	return present, err
}

func migrationWriteLayoutIdentity(ctx context.Context, path string, layout UserStorageLayout) (err error) {
	digest, err := migrationLayoutDigest(layout)
	if err != nil {
		return err
	}
	db, err := OpenExisting(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migrationLayoutSchema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_storage_layout(singleton,layout_version,layout_id,manifest_digest) VALUES(1,1,?,?)`, layout.ID, digest); err != nil {
		return err
	}
	return tx.Commit()
}

func migrationRequireClosedDatabase(path string) error {
	if err := migrationRequireRegular(path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("publication has a nonregular SQLite sidecar")
		}
		if suffix == "-wal" && info.Size() != 0 {
			return errors.New("publication requires closed, checkpointed SQLite files")
		}
	}
	return nil
}

func migrationSnapshotPublication(ctx context.Context, central, users string) (result migrationPublicationProof, err error) {
	result.Version = 1
	if err := migrationRequireClosedDatabase(central); err != nil {
		return result, err
	}
	result.CentralDigest, result.CentralBytes, err = migrationHashFile(ctx, central)
	if err != nil {
		return result, err
	}
	result.UsersDigest, result.UsersBytes, result.UsersFiles, err = migrationPublicationUsers(ctx, users)
	return result, err
}

func migrationPublicationUsers(ctx context.Context, directory string) (digest string, size, files int64, err error) {
	if err := migrationRequireDirectory(directory); err != nil {
		return "", 0, 0, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", 0, 0, err
	}
	hash := sha256.New()
	migrationDigestFields(hash, "gofer-publication-users-v1")
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, err
		}
		name := entry.Name()
		path := filepath.Join(directory, name)
		if err := migrationRequireRegular(path); err != nil {
			return "", 0, 0, err
		}
		if name == "manager.lock" {
			continue
		}
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			suffix := "-shm"
			if strings.HasSuffix(name, "-wal") {
				suffix = "-wal"
			}
			base := strings.TrimSuffix(name, suffix)
			if !migrationUserFilename(base) {
				return "", 0, 0, errors.New("publication has an unexpected user sidecar")
			}
			if err := migrationRequireClosedDatabase(filepath.Join(directory, base)); err != nil {
				return "", 0, 0, err
			}
			continue
		}
		if !migrationUserFilename(name) {
			return "", 0, 0, errors.New("publication has an unexpected user file")
		}
		if err := migrationRequireClosedDatabase(path); err != nil {
			return "", 0, 0, err
		}
		value, bytes, err := migrationHashFile(ctx, path)
		if err != nil {
			return "", 0, 0, err
		}
		migrationDigestFields(hash, name, fmt.Sprint(bytes), value)
		size += bytes
		files++
	}
	return hex.EncodeToString(hash.Sum(nil)), size, files, nil
}

func migrationUserFilename(name string) bool {
	if len(name) != 67 || !strings.HasSuffix(name, ".db") {
		return false
	}
	value := strings.TrimSuffix(name, ".db")
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func migrationSyncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// MigrateUserStorage prepares and publishes a new layout. Explicit retry starts
// a fresh copy for an interrupted preparation, or resumes the recorded closed
// files once publication began. It never guesses which arbitrary files to reuse.
func MigrateUserStorage(ctx context.Context, options UserStorageMigrationOptions) (UserStorageLayout, error) {
	if ctx == nil || options.ValidateSource == nil || options.ImportCredentials == nil || options.VerifyCredentials == nil {
		return UserStorageLayout{}, errors.New("migration requires source and credential adapters")
	}
	if err := ctx.Err(); err != nil {
		return UserStorageLayout{}, err
	}
	destination, err := canonicalMigrationPath(options.DestinationPath)
	if err != nil {
		return UserStorageLayout{}, err
	}
	journal, err := readMigrationPreparationJournal(destination + ".migration.json")
	if err == nil {
		if !options.Retry {
			return UserStorageLayout{}, errors.New("migration already has recorded work; explicit retry is required")
		}
		switch journal.State {
		case "verified", "publishing", "published":
			return PublishUserStorageMigration(ctx, options)
		case "preparing": // Stage validates the recorded source and retains old files.
		default:
			return UserStorageLayout{}, errors.New("migration journal has an unrecognized state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return UserStorageLayout{}, err
	}
	if _, err := StageUserStorageMigration(ctx, options); err != nil {
		return UserStorageLayout{}, err
	}
	return PublishUserStorageMigration(ctx, options)
}
