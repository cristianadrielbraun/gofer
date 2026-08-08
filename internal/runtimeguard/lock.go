package runtimeguard

import (
	"errors"
	"fmt"
	"os"
)

var ErrAlreadyLocked = errors.New("Gofer is already using this database")

type Lock struct {
	file *os.File
}

// Acquire takes the non-blocking, cross-process lock shared by the Gofer
// server and local operator mutations. Its parent directory must already
// exist, and the returned lock must remain open for the protected operation.
func Acquire(databasePath string) (*Lock, error) {
	if databasePath == "" {
		return nil, fmt.Errorf("database path is required")
	}
	lockPath := databasePath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock %q: %w", lockPath, err)
	}
	if err := tryLock(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errLockUnavailable) {
			return nil, fmt.Errorf("%w; stop the server or other mutating operator command and try again", ErrAlreadyLocked)
		}
		return nil, fmt.Errorf("lock runtime file %q: %w", lockPath, err)
	}
	return &Lock{file: file}, nil
}

func (lock *Lock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	unlockErr := unlock(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock runtime file: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close runtime lock: %w", closeErr)
	}
	return nil
}
