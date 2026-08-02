package handler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestContactSyncWorkerCancelsMissingContactBeforeProviderWork(t *testing.T) {
	ctx := t.Context()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name) VALUES ('user-a', 'a@example.com', 'User A');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('account-a', 'user-a', 'gmail', 'a@example.com');
	`); err != nil {
		t.Fatalf("seed user and account: %v", err)
	}
	opID, err := db.EnqueueContactSyncOperation(ctx, "user-a", models.Contact{
		ID:               "deleted-contact",
		Email:            "contact@example.com",
		GoferSyncEnabled: true,
		SaveTargets:      []string{"account:account-a"},
	}, nil)
	if err != nil || opID == "" {
		t.Fatalf("EnqueueContactSyncOperation() = %q, %v", opID, err)
	}
	ops, err := db.ClaimContactSyncOperations(ctx, 1, time.Minute)
	if err != nil || len(ops) != 1 {
		t.Fatalf("ClaimContactSyncOperations() = %#v, %v", ops, err)
	}

	h := &Handler{db: db}
	h.processContactSyncOperation(ctx, ops[0])

	var status string
	if err := db.Read().QueryRowContext(ctx, `SELECT status FROM contact_sync_operations WHERE id = ?`, opID).Scan(&status); err != nil {
		t.Fatalf("query operation status: %v", err)
	}
	if status != "done" {
		t.Fatalf("operation status = %q, want done cancellation", status)
	}
}
