package storage

import (
	"testing"
	"time"
)

// Old avatar lookup log rows go in batches, oldest first; recent ones stay.
func TestPruneAvatarAttemptLogsRemovesOldRows(t *testing.T) {
	db := newContactsTestDB(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := db.Write().Exec(`
		INSERT INTO avatar_attempt_logs(email, provider, status, created_at) VALUES
			('rosa@example.test', 'gravatar', 'missing', '2026-08-01 10:00:00'),
			('rosa@example.test', 'bimi', 'missing', '2026-08-02 10:00:00'),
			('rosa@example.test', 'gravatar', 'missing', '2026-09-23 10:00:00'),
			('rosa@example.test', 'gravatar', 'missing', '2026-10-20 10:00:00')`); err != nil {
		t.Fatal(err)
	}
	before := now.Add(-AvatarAttemptLogRetention)
	for _, want := range []int{2, 1, 0} {
		n, err := db.PruneAvatarAttemptLogs(t.Context(), before, 2)
		if err != nil || n != want {
			t.Fatalf("PruneAvatarAttemptLogs() = %d, %v; want %d", n, err, want)
		}
	}
	var left string
	if err := db.Read().QueryRow(`SELECT CAST(created_at AS TEXT) FROM avatar_attempt_logs`).Scan(&left); err != nil || left != "2026-10-20 10:00:00" {
		t.Fatalf("row left = %q, %v; want only the recent one", left, err)
	}
}
