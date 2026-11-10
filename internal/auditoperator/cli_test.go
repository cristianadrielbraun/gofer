package auditoperator

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var secret = []byte("operator-test-secret-of-32-bytes")

func seed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, time.November, 1, 9, 0, 0, 0, time.UTC)
	writer := audit.NewWriter(audit.DeriveKey(secret), audit.WithClock(func() time.Time { return at }))
	for _, event := range []audit.Event{
		{Action: "user.disabled", Actor: audit.Actor{Kind: audit.ActorAdmin, ID: "admin-1", Label: "ada.admin"}, Target: audit.Target{Kind: "user", ID: "u-1", Label: "milo.rivers"}},
		{Action: "security.proxy_added", Actor: audit.Actor{Kind: audit.ActorAdmin, ID: "admin-1"}, Target: audit.Target{Kind: "proxy", Label: "Office"}},
	} {
		tx, err := db.Write().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.WriteTx(context.Background(), tx, event); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		at = at.Add(24 * time.Hour)
	}
	return path
}

func run(t *testing.T, path string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, path, func() ([]byte, error) { return secret, nil }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVerifyReportsAnIntactOrChangedLog(t *testing.T) {
	path := seed(t)
	if code, out, errOut := run(t, path, "audit", "verify"); code != 0 || !strings.Contains(out, "Verified 2 events, 1 to 2") {
		t.Fatalf("verify = %d %q %q", code, out, errOut)
	}
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE audit_events SET target_label = 'someone.else' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if code, out, _ := run(t, path, "audit", "verify"); code != 1 || !strings.Contains(out, "event 2") {
		t.Fatalf("verify after an edit = %d %q", code, out)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"audit", "verify"}, path, func() ([]byte, error) { return []byte("another secret"), nil }, &stdout, &stderr); code != 1 {
		t.Fatalf("verify with another key = %d %q", code, stdout.String())
	}
}

func TestVerifyAnEmptyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if code, out, _ := run(t, path, "audit", "verify"); code != 0 || !strings.Contains(out, "empty") {
		t.Fatalf("verify an empty log = %d %q", code, out)
	}
}

func TestExportWritesTheLog(t *testing.T) {
	path := seed(t)
	code, out, errOut := run(t, path, "audit", "export")
	if code != 0 || !strings.HasPrefix(out, "seq,id,occurred_at") || strings.Count(out, "\n") != 3 {
		t.Fatalf("csv export = %d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, path, "audit", "export", "--format", "jsonl", "--since", "2026-11-02T00:00:00Z")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, `"action":"security.proxy_added"`) {
		t.Fatalf("jsonl export since = %d %q", code, out)
	}
	code, out, _ = run(t, path, "audit", "export", "--format", "jsonl", "--action", "user.")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "milo.rivers") {
		t.Fatalf("jsonl export of people = %d %q", code, out)
	}
	for _, args := range [][]string{
		{"audit", "export", "--format", "xml"},
		{"audit", "export", "--since", "yesterday"},
		{"audit", "export", "extra"},
		{"audit", "rewrite"},
		{"users"},
	} {
		if code, _, _ := run(t, path, args...); code != 2 {
			t.Errorf("%v = %d, want a usage error", args, code)
		}
	}
	if code, out, _ := run(t, path, "--help"); code != 0 || !strings.Contains(out, "gofer admin audit verify") {
		t.Fatalf("help = %d %q", code, out)
	}
}
