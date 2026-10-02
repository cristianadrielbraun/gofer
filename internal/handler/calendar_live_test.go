package handler

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// TestLiveCalDAVReadOnly is opt-in and never updates the database or provider.
// Select exactly one source with GOFER_LIVE_CALDAV_SOURCE and supply an existing
// database through GOFER_LIVE_CALDAV_DB. Its saved credentials remain local.
func TestLiveCalDAVReadOnly(t *testing.T) {
	dbPath := os.Getenv("GOFER_LIVE_CALDAV_DB")
	if dbPath == "" {
		t.Skip("set GOFER_LIVE_CALDAV_DB and GOFER_LIVE_CALDAV_SOURCE for a read-only provider check")
	}
	sourceID := os.Getenv("GOFER_LIVE_CALDAV_SOURCE")
	if sourceID == "" {
		t.Fatal("GOFER_LIVE_CALDAV_SOURCE must identify the calendar to check")
	}
	db, err := storage.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var userID string
	if err := db.Read().QueryRowContext(ctx, `SELECT user_id FROM calendar_sources WHERE id = ? AND provider = 'caldav' AND is_selected = 1 AND is_deleted = 0`, sourceID).Scan(&userID); err != nil {
		t.Fatal("selected CalDAV source not found")
	}
	sources, err := db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.ID == sourceID {
			source = candidate
		}
	}
	if source.ID == "" {
		t.Fatal("CalDAV account is not active")
	}
	var key []byte
	if value := os.Getenv("GOFER_SECRET_KEY"); value != "" {
		key, err = hex.DecodeString(value)
	} else {
		key, err = os.ReadFile(filepath.Join(filepath.Dir(dbPath), "secret.key"))
	}
	if err != nil || len(key) != 32 {
		t.Fatal("the existing encryption key could not be loaded")
	}
	accountStore, err := config.NewAccountStore(db, key)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, accountStore: accountStore}
	credentials := h.calendarCredentialsForSource(ctx, userID, source)
	if credentials.err != nil {
		t.Fatalf("saved CalDAV authentication could not be loaded: %v", credentials.err)
	}
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		t.Fatal("calendar endpoint is outside the configured CalDAV origin")
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	page, err := listCalDAVCalendarEvents(ctx, source, credentials.username, credentials.password, calendar.EventQuery{WindowStart: start, WindowEnd: start.AddDate(0, 1, 0)})
	if err != nil {
		t.Fatalf("live CalDAV read failed: %v", err)
	}
	allDay := 0
	for _, event := range page.Events {
		if event.AllDay {
			allDay++
		}
	}
	t.Logf("read-only CalDAV fetch passed: %d events (%d all-day); cache and provider unchanged", len(page.Events), allDay)
}
