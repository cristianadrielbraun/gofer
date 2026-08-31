package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdministratorSecurityActivityRendersSanitizedPaginatedInstanceEvents(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	current, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || current == nil {
		t.Fatalf("load administrator session = %#v, %v", current, err)
	}
	var administratorUsername string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT username FROM users WHERE id = ?`, current.UserID,
	).Scan(&administratorUsername); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			mfa_required, user_type, is_admin, created_at, updated_at
		) VALUES ('admin-activity-target', 'target-user', 'target-user',
			'Private Target Name', 'active', 1, 0, 'webmail', 0, ?, ?)`, now, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		id        string
		occurred  time.Time
		actor     any
		subject   any
		eventType auth.AuthEventType
		success   int
		reason    auth.AuthEventReason
		agent     string
		metadata  string
	}{
		{
			id: "private-deletion-event-id", occurred: now.Add(3 * time.Minute), actor: current.UserID, subject: nil,
			eventType: auth.AuthEventUserDeleted, success: 1, reason: auth.AuthEventReasonAdministratorAction,
			agent: "Administrator Browser", metadata: `{"target_username":"removed-user","target_user_id":"private-deleted-user-id","mail":"private@example.com"}`,
		},
		{
			id: "private-failed-event-id", occurred: now.Add(2 * time.Minute), actor: nil, subject: "admin-activity-target",
			eventType: auth.AuthEventLoginFailed, success: 0, reason: auth.AuthEventReasonInvalidCredentials,
			agent: "<script>event-client</script>", metadata: `{"submitted_identifier":"private-login-value","message_body":"private mailbox body"}`,
		},
		{
			id: "private-system-event-id", occurred: now.Add(time.Minute), actor: nil, subject: nil,
			eventType: auth.AuthEventSetupTokenIssued, success: 1, reason: auth.AuthEventReasonSystemInitialization,
			agent: "", metadata: `{"setup_token":"private-setup-token"}`,
		},
	} {
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				request_id, event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, 'private-request-id', ?, ?, ?, ?, 'private-source-hash', ?)`,
			event.id, event.occurred, event.actor, event.subject, current.ID,
			event.eventType, event.success, event.reason, event.agent, event.metadata,
		); err != nil {
			t.Fatalf("insert administrator activity event %q: %v", event.id, err)
		}
	}
	for index := 0; index < 55; index++ {
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, 'admin-activity-target', ?, 1, ?, ?, '{}')`,
			fmt.Sprintf("admin-activity-history-%02d", index), now.Add(-time.Duration(index+1)*time.Minute),
			current.UserID, auth.AuthEventCredentialChanged, auth.AuthEventReasonUserAction,
			fmt.Sprintf("History Browser %02d", index),
		); err != nil {
			t.Fatal(err)
		}
	}
	var totalEvents int64
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&totalEvents); err != nil {
		t.Fatal(err)
	}
	totalPages := (totalEvents + 49) / 50

	request := httptest.NewRequest(http.MethodGet, adminSecurityActivityPath, nil)
	request.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator security activity = %d %q", page.Code, page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		`data-admin-security-activity`, "Admin security activity", fmt.Sprintf("%d events", totalEvents),
		`href="/admin/activity"`, `aria-current="page"`, "Instance events",
		"User deleted", "removed-user", "Deleted", administratorUsername,
		"Sign-in attempt failed", "target-user", "System", "Instance",
		`&lt;script&gt;event-client&lt;/script&gt;`, fmt.Sprintf("Page 1 of %d", totalPages),
		`href="/admin/activity?page=2"`, "Raw audit metadata", "private mailbox content are never displayed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator security activity omitted %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>event-client</script>`, "Private Target Name", "private@example.com",
		"private mailbox body", "private-login-value", "private-setup-token",
		"private-deletion-event-id", "private-failed-event-id", "private-system-event-id",
		"private-deleted-user-id", "private-request-id", "private-source-hash",
		current.ID, sessionCookie.Value,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator security activity exposed forbidden value %q", forbidden)
		}
	}
	if page.Header().Get("Cache-Control") != "no-store" ||
		page.Header().Get("Referrer-Policy") != "no-referrer" ||
		page.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("administrator security activity headers = %#v", page.Header())
	}

	partialRequest := httptest.NewRequest(http.MethodGet, adminSecurityActivityPath+"?page=2", nil)
	partialRequest.Header.Set("HX-Request", "true")
	partialRequest.AddCookie(sessionCookie)
	partial := httptest.NewRecorder()
	stack.ServeHTTP(partial, partialRequest)
	if partial.Code != http.StatusOK || !strings.Contains(partial.Body.String(), `id="main-content"`) ||
		strings.Contains(partial.Body.String(), "<!DOCTYPE html>") || !strings.Contains(partial.Body.String(), "History Browser") {
		t.Fatalf("administrator security activity partial = %d %q", partial.Code, partial.Body.String())
	}
}

func TestAdministratorSecurityActivityRejectsMalformedPageAndLocksBeforeQueryingEvents(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	current, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || current == nil {
		t.Fatalf("load administrator session = %#v, %v", current, err)
	}
	malformedRequest := httptest.NewRequest(http.MethodGet, adminSecurityActivityPath+"?page=0", nil)
	malformedRequest.AddCookie(sessionCookie)
	malformed := httptest.NewRecorder()
	stack.ServeHTTP(malformed, malformedRequest)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed administrator activity page = %d %q", malformed.Code, malformed.Body.String())
	}

	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_events (
			id, occurred_at, actor_user_id, subject_user_id, session_id,
			event_type, success, reason, user_agent, metadata_json
		) VALUES ('locked-private-event-id', ?, ?, ?, ?, ?, 1, ?, 'Locked private browser',
			'{"private":"locked-private-metadata"}')`,
		time.Now().UTC().Add(time.Minute), current.UserID, current.UserID, current.ID,
		auth.AuthEventSecurityPolicyChanged, auth.AuthEventReasonAdministratorAction,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), current.ID,
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, adminSecurityActivityPath, nil)
	request.AddCookie(sessionCookie)
	locked := httptest.NewRecorder()
	stack.ServeHTTP(locked, request)
	if locked.Code != http.StatusOK {
		t.Fatalf("locked administrator security activity = %d %q", locked.Code, locked.Body.String())
	}
	html := locked.Body.String()
	for _, want := range []string{
		`data-admin-security-activity-locked`, "Recent administrator verification required",
		"before Gofer requests any instance security events", `data-admin-security-verification`,
		`action="/settings/security/step-up"`, `name="return_to" value="/admin/activity"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("locked administrator activity omitted %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		"Instance events", "Security policy changed", "Locked private browser",
		"locked-private-event-id", "locked-private-metadata",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("locked administrator activity exposed %q", forbidden)
		}
	}
}

func TestAdministratorSecurityActivityExplainsLocalModeWithoutQueryingAuthenticationEvents(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodGet, adminSecurityActivityPath, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), &auth.User{ID: "default"}))
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, request)
	if page.Code != http.StatusOK {
		t.Fatalf("local-mode administrator security activity = %d %q", page.Code, page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		"Managed authentication is not enabled",
		"available when Gofer is running with user management enabled",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local-mode security activity omitted %q", want)
		}
	}
	if strings.Contains(html, "Instance events") {
		t.Fatalf("local-mode security activity rendered the managed event table: %q", html)
	}
}
