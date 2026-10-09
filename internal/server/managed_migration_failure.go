package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// A conversion that fails on the original database's contents fails the same
// way on every start, after minutes of checks. The failure is remembered next
// to the database so later starts report it at once, until the database or the
// Gofer executable changes. Interruptions and environment problems such as
// disk space are never remembered, so they are always retried.
type migrationFailure struct {
	Error      string    `json:"error"`
	FailedAt   time.Time `json:"failed_at"`
	Database   fileStamp `json:"database"`
	Executable fileStamp `json:"executable"`
}

type fileStamp struct {
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.Size == b.Size && a.Modified.Equal(b.Modified)
}

func migrationFailurePath(path string) string { return path + ".migration-failed.json" }

func migrationStamps(path string) (database, executable fileStamp, err error) {
	stamp := func(name string) (fileStamp, error) {
		info, err := os.Stat(name)
		if err != nil {
			return fileStamp{}, err
		}
		return fileStamp{Size: info.Size(), Modified: info.ModTime()}, nil
	}
	if database, err = stamp(path); err != nil {
		return
	}
	program, err := os.Executable()
	if err != nil {
		return
	}
	executable, err = stamp(program)
	return
}

// checkRecordedMigrationFailure returns the remembered failure while nothing
// has changed, and forgets it otherwise.
func checkRecordedMigrationFailure(path string) error {
	file, err := os.Open(migrationFailurePath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failure migrationFailure
	decodeErr := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&failure)
	if err := file.Close(); err != nil {
		return err
	}
	database, executable, err := migrationStamps(path)
	if err != nil {
		return err
	}
	if decodeErr == nil && failure.Database.equal(database) && failure.Executable.equal(executable) {
		return fmt.Errorf("per-user storage migration failed on %s and was not retried because neither the database nor Gofer has changed since: %s; fix the cause, or delete %s to retry anyway",
			failure.FailedAt.Local().Format("2006-01-02 15:04:05"), failure.Error, migrationFailurePath(path))
	}
	return forgetMigrationFailure(path)
}

func recordMigrationFailure(path string, cause error) error {
	database, executable, err := migrationStamps(path)
	if err != nil {
		return err
	}
	data, err := json.Marshal(migrationFailure{Error: cause.Error(), FailedAt: time.Now().UTC(), Database: database, Executable: executable})
	if err != nil {
		return err
	}
	return os.WriteFile(migrationFailurePath(path), append(data, '\n'), 0600)
}

func forgetMigrationFailure(path string) error {
	if err := os.Remove(migrationFailurePath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
