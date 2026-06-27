package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCheckGmailAPIProfileDetectsChangedHistory(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GOFER_GMAIL_API_SYNC", "1")
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "100",
		Full:         true,
		StartedAt:    time.Now().Add(-time.Minute),
		FinishedAt:   time.Now(),
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/me/profile" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "101"})
	}))
	defer server.Close()
	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	changed, profileHistoryID, err := orchestrator.checkGmailAPIProfile(ctx, "acc")
	if err != nil {
		t.Fatalf("checkGmailAPIProfile() error = %v", err)
	}
	if !changed || profileHistoryID != "101" {
		t.Fatalf("changed/profile = %v/%q, want true/101", changed, profileHistoryID)
	}
	state, err := db.GetGmailPollState(ctx, "acc")
	if err != nil {
		t.Fatalf("GetGmailPollState() error = %v", err)
	}
	if state.ProfileHistoryID != "101" || !state.LastCheckedAt.Valid || !state.LastChangedAt.Valid || state.LastError != "" {
		t.Fatalf("poll state = %#v, want changed profile check", state)
	}
}

func TestCheckGmailAPIProfileSkipsUnchangedHistory(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GOFER_GMAIL_API_SYNC", "1")
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "101",
		Full:         true,
		StartedAt:    time.Now().Add(-time.Minute),
		FinishedAt:   time.Now(),
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "101"})
	}))
	defer server.Close()
	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	changed, profileHistoryID, err := orchestrator.checkGmailAPIProfile(ctx, "acc")
	if err != nil {
		t.Fatalf("checkGmailAPIProfile() error = %v", err)
	}
	if changed || profileHistoryID != "101" {
		t.Fatalf("changed/profile = %v/%q, want false/101", changed, profileHistoryID)
	}
	state, err := db.GetGmailPollState(ctx, "acc")
	if err != nil {
		t.Fatalf("GetGmailPollState() error = %v", err)
	}
	if state.ProfileHistoryID != "101" || !state.LastCheckedAt.Valid || state.LastChangedAt.Valid {
		t.Fatalf("poll state = %#v, want checked without changed timestamp", state)
	}
}

func TestBeginActiveUserSessionTracksActiveUsers(t *testing.T) {
	orchestrator := NewSyncOrchestrator(nil, nil, nil, nil)
	endOne := orchestrator.BeginActiveUserSession("default")
	endTwo := orchestrator.BeginActiveUserSession("default")

	active := orchestrator.activeUserIDs()
	if len(active) != 1 || active[0] != "default" {
		t.Fatalf("active users = %#v, want default once", active)
	}
	endOne()
	active = orchestrator.activeUserIDs()
	if len(active) != 1 || active[0] != "default" {
		t.Fatalf("active users after one close = %#v, want default", active)
	}
	endTwo()
	if active = orchestrator.activeUserIDs(); len(active) != 0 {
		t.Fatalf("active users after all close = %#v, want none", active)
	}
}

func TestPollGmailAPIAccountSkipsProfileCheckWhileSyncRunning(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GOFER_GMAIL_API_SYNC", "1")
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var profileRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		profileRequests++
		_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "101"})
	}))
	defer server.Close()
	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	orchestrator.mu.Lock()
	orchestrator.running["acc"] = &accountSyncRun{done: make(chan struct{})}
	orchestrator.mu.Unlock()

	orchestrator.pollGmailAPIAccountIfDue(ctx, "acc", true)
	if profileRequests != 0 {
		t.Fatalf("profile requests = %d, want no polling while sync is already running", profileRequests)
	}
}

func TestMarkGmailPollCheckPreservesChangeTimestampOnUnchangedCheck(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	changedAt := time.Now().UTC().Add(-time.Minute)
	if err := db.MarkGmailPollCheck(ctx, storage.GmailPollState{
		AccountID:        "acc",
		ProfileHistoryID: "100",
		LastCheckedAt:    sql.NullTime{Time: changedAt, Valid: true},
		LastChangedAt:    sql.NullTime{Time: changedAt, Valid: true},
	}, true, nil); err != nil {
		t.Fatalf("MarkGmailPollCheck(changed) error = %v", err)
	}
	if err := db.MarkGmailPollCheck(ctx, storage.GmailPollState{
		AccountID:        "acc",
		ProfileHistoryID: "100",
		LastCheckedAt:    sql.NullTime{Time: time.Now().UTC(), Valid: true},
	}, false, nil); err != nil {
		t.Fatalf("MarkGmailPollCheck(unchanged) error = %v", err)
	}
	state, err := db.GetGmailPollState(ctx, "acc")
	if err != nil {
		t.Fatalf("GetGmailPollState() error = %v", err)
	}
	if !state.LastChangedAt.Valid || !state.LastChangedAt.Time.Equal(changedAt) {
		t.Fatalf("LastChangedAt = %#v, want preserved %s", state.LastChangedAt, changedAt)
	}
}
