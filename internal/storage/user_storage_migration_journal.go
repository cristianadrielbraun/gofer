package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type migrationSourceSnapshot struct {
	DatabaseDigest string `json:"database_digest"`
	DatabaseBytes  int64  `json:"database_bytes"`
	WALPresent     bool   `json:"wal_present"`
	WALDigest      string `json:"wal_digest"`
	WALBytes       int64  `json:"wal_bytes"`
}

type migrationPreparationJournal struct {
	Version          int                       `json:"version"`
	KeyFingerprint   string                    `json:"key_fingerprint,omitempty"`
	ID               string                    `json:"id"`
	State            string                    `json:"state"`
	SourcePath       string                    `json:"source_path"`
	DestinationPath  string                    `json:"destination_path"`
	WorkingDirectory string                    `json:"working_directory"`
	Directory        string                    `json:"directory"`
	Source           migrationSourceSnapshot   `json:"source"`
	Files            UserStorageMigrationFiles `json:"files"`
	Owners           int64                     `json:"owners_copied"`
	Publication      migrationPublicationProof `json:"publication"`
}

func migrationSnapshotSource(ctx context.Context, path string) (result migrationSourceSnapshot, err error) {
	result.DatabaseDigest, result.DatabaseBytes, err = migrationHashFile(ctx, path)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	result.WALPresent = true
	result.WALDigest, result.WALBytes, err = migrationHashFile(ctx, path+"-wal")
	return result, err
}

func migrationAttemptJournal(ctx context.Context, source, destination, workingDirectory string, files UserStorageMigrationFiles, retry bool, keyFingerprint string) (result migrationPreparationJournal, err error) {
	result = migrationPreparationJournal{Version: 1, State: "preparing", SourcePath: source, DestinationPath: destination, WorkingDirectory: workingDirectory, Files: files, KeyFingerprint: keyFingerprint}
	result.Source, err = migrationSnapshotSource(ctx, source)
	if err != nil {
		return result, err
	}
	previous, readErr := readMigrationPreparationJournal(destination + ".migration.json")
	if readErr == nil {
		if !retry {
			return result, errors.New("migration preparation already exists; explicit retry is required")
		}
		if previous.Version != 1 || (previous.State != "preparing" && previous.State != "verified") || previous.SourcePath != source || previous.DestinationPath != destination || previous.WorkingDirectory != workingDirectory || previous.Source != result.Source || previous.Files != files || previous.KeyFingerprint != keyFingerprint {
			return result, errors.New("migration retry does not match the original source, files or working directory")
		}
		if !validMigrationAttemptID(previous.ID) || !validMigrationAttemptDirectory(destination, previous.Directory, previous.ID) {
			return result, errors.New("migration retry journal has an invalid private attempt identity")
		}
		// A previous attempt may have stopped before its directory was created.
		if info, err := os.Lstat(previous.Directory); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return result, errors.New("migration retry stage is not a real directory")
			}
			bound, err := readMigrationPreparationJournal(filepath.Join(previous.Directory, "PREPARATION.json"))
			expected := previous
			expected.State, expected.Owners = "preparing", 0
			if errors.Is(err, os.ErrNotExist) && previous.State == "preparing" {
				// The process may have stopped after mkdir but before binding. Retain
				// this directory untouched; none of its files are opened or adopted.
			} else if err != nil || bound != expected {
				return result, errors.New("migration retry stage does not match its recorded preparation")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return result, readErr
	} else if retry {
		return result, errors.New("migration retry requires a recognized existing preparation")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, err
	}
	result.ID = hex.EncodeToString(nonce[:])
	result.Directory = destination + ".staging"
	if readErr == nil {
		result.Directory += "-" + result.ID
	}
	if _, err := os.Lstat(result.Directory); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("private migration attempt already exists or cannot be inspected")
	}
	if err := writeMigrationPreparationJournal(destination+".migration.json", result); err != nil {
		return result, err
	}
	return result, nil
}

func validMigrationAttemptID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && id == strings.ToLower(id)
}

func validMigrationAttemptDirectory(destination, directory, id string) bool {
	return directory == destination+".staging" || directory == destination+".staging-"+id
}

func readMigrationPreparationJournal(path string) (result migrationPreparationJournal, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return result, errors.New("migration preparation journal is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, errors.New("migration preparation journal is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, errors.New("migration preparation journal has trailing data")
	}
	return result, nil
}

func writeMigrationPreparationJournal(path string, journal migrationPreparationJournal) (err error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("migration journal destination is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".migration-journal-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	encodeErr := json.NewEncoder(file).Encode(journal)
	if err := errors.Join(encodeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return migrationSyncDirectory(filepath.Dir(path))
}

func migrationSyncDirectory(path string) error {
	// Windows FlushFileBuffers does not support directory handles opened by os.
	// Files are flushed; native crash/power-loss acceptance remains platform-specific.
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// Flush closed private stores before marking a preparation verified. A verified
// journal is still unpublished; no runtime may activate it as a completed layout.
func migrationSyncPrivateStage(ctx context.Context, directory string) error {
	var directories []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("private migration stage contains a nonregular file")
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	})
	if err != nil {
		return err
	}
	for n := len(directories) - 1; n >= 0; n-- {
		if err := migrationSyncDirectory(directories[n]); err != nil {
			return err
		}
	}
	return nil
}
