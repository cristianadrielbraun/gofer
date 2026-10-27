package authoperator

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// startTestControlServer stands in for a running Gofer: it holds the runtime
// lock and answers on the control socket until the test ends.
func startTestControlServer(t *testing.T, databasePath string) {
	t.Helper()
	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	listener, err := ListenControl(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	stop := ServeControl(listener, db)
	t.Cleanup(func() {
		if err := stop(t.Context()); err != nil {
			t.Errorf("stop control server: %v", err)
		}
	})
}

func TestInvitationRunsThroughRunningServer(t *testing.T) {
	setManagedInvitationEnvironment(t)
	databasePath := createOperatorTestDatabase(t)
	startTestControlServer(t, databasePath)

	info, err := os.Stat(filepath.Dir(ControlSocketPath(databasePath)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("control directory mode = %v, want 0700", info.Mode().Perm())
	}

	var stdout, stderr bytes.Buffer
	args := []string{"invite", "create", "--name", "Ada Example", "--username", "ada"}
	if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(invite create through server) code = %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 || !strings.Contains(stdout.String(), "invitation_token: ") ||
		!strings.Contains(stdout.String(), `username: "ada"`) {
		t.Fatalf("invitation through server output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}

	reader, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var status string
	if err := reader.Read().QueryRow(`SELECT status FROM users WHERE username_normalized = 'ada'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("invited user through server = status:%q err:%v", status, err)
	}
}

func TestRunningServerReportsCommandFailures(t *testing.T) {
	setManagedInvitationEnvironment(t)
	databasePath := createOperatorTestDatabase(t)
	startTestControlServer(t, databasePath)

	var stdout, stderr bytes.Buffer
	args := []string{"recover", "--username", "nobody", "--confirm", "nobody"}
	if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(recover unknown through server) code = %d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), `auth recover: no user has username "nobody"`) {
		t.Fatalf("failed recovery through server output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
}

func TestStaleControlSocketFallsBackToLocalCommand(t *testing.T) {
	setManagedInvitationEnvironment(t)
	databasePath := createOperatorTestDatabase(t)
	// A Gofer that crashed leaves its socket file with nobody listening.
	listener, err := ListenControl(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ControlSocketPath(databasePath)); err != nil {
		t.Fatalf("stale socket was not left behind: %v", err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"invite", "create", "--name", "Ada Example", "--username", "ada"}
	if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(invite create with stale socket) code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "invitation_token: ") {
		t.Fatalf("invitation with stale socket output = %q", stdout.String())
	}
}

func TestControlSocketRefusesReadOnlyAndUnknownCommands(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, args := range [][]string{{"status"}, {"users", "list"}, {"help"}, {"storage", "migrate"}} {
		var stdout, stderr bytes.Buffer
		if code := runControlCommand(t.Context(), args, db, &stdout, &stderr); code != 2 {
			t.Fatalf("runControlCommand(%v) code = %d", args, code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "did not recognize") {
			t.Fatalf("runControlCommand(%v) output = stdout:%q stderr:%q", args, stdout.String(), stderr.String())
		}
	}
}
