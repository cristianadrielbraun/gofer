package runtimeguard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusiveAndReusable(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	first, err := Acquire(databasePath)
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	if _, err := os.Stat(databasePath + ".lock"); err != nil {
		first.Close()
		t.Fatalf("runtime lock file was not created: %v", err)
	}
	second, err := Acquire(databasePath)
	if second != nil || !errors.Is(err, ErrAlreadyLocked) {
		first.Close()
		t.Fatalf("Acquire(second) = %#v, %v; want ErrAlreadyLocked", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	reused, err := Acquire(databasePath)
	if err != nil {
		t.Fatalf("Acquire(after release) error = %v", err)
	}
	if err := reused.Close(); err != nil {
		t.Fatalf("Close(reused) error = %v", err)
	}
}

func TestAcquireDoesNotCreateMissingParentDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	lock, err := Acquire(filepath.Join(directory, "gofer.db"))
	if lock != nil || err == nil {
		t.Fatalf("Acquire(missing parent) = %#v, %v", lock, err)
	}
	if _, statErr := os.Stat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("Acquire created missing parent: %v", statErr)
	}
}

func TestAcquireExcludesAnotherProcess(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	command := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	command.Env = append(os.Environ(),
		"GOFER_RUNTIME_LOCK_HELPER=1",
		"GOFER_RUNTIME_LOCK_DATABASE="+databasePath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		_ = stdin.Close()
		_ = command.Wait()
		t.Fatalf("lock helper readiness = %q, %v", line, err)
	}
	lock, err := Acquire(databasePath)
	if lock != nil || !errors.Is(err, ErrAlreadyLocked) {
		_ = stdin.Close()
		_ = command.Wait()
		t.Fatalf("Acquire(while helper holds lock) = %#v, %v", lock, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err = Acquire(databasePath)
	if err != nil {
		t.Fatalf("Acquire(after helper exit) error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv("GOFER_RUNTIME_LOCK_HELPER") != "1" {
		return
	}
	lock, err := Acquire(os.Getenv("GOFER_RUNTIME_LOCK_DATABASE"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
