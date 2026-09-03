package handler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAuthenticationEventRetentionWorkerPrunesExpiredAuditRows(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	now := time.Date(2026, time.September, 2, 9, 0, 0, 0, time.UTC)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			user_type, is_admin, created_at, updated_at
		) VALUES ('administrator', 'administrator', 'administrator', 'Administrator',
			'active', 1, 'management', 1, ?, ?)`, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (
			id, initialized, owner_user_id, auth_event_retention_days
		) VALUES (1, 1, 'administrator', 1)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_events (
			id, occurred_at, event_type, success, reason, metadata_json
		) VALUES
			('expired-event', ?, 'login_succeeded', 1, 'challenge_verified', '{}'),
			('recent-event', ?, 'login_succeeded', 1, 'challenge_verified', '{}')`,
		now.Add(-48*time.Hour), now.Add(-12*time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	h := &Handler{auth: manager}
	h.runAuthenticationEventRetentionAt(t.Context(), now)
	var expired, recent int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE id = 'expired-event'`).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE id = 'recent-event'`).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || recent != 1 {
		t.Fatalf("retained authentication events = expired:%d recent:%d", expired, recent)
	}
}
