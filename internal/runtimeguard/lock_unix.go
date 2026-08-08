//go:build !windows

package runtimeguard

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errLockUnavailable = errors.New("runtime lock is unavailable")

func tryLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errLockUnavailable
	}
	return err
}

func unlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
