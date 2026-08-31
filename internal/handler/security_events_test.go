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

func TestSecurityEventViewDataUsesOnlyFriendlyBoundedFields(t *testing.T) {
	now := time.Date(2026, time.August, 19, 18, 0, 0, 0, time.Local)
	viewData := securityEventViewData([]auth.SecurityEventSummary{
		{
			OccurredAt: now, EventType: auth.AuthEventIdentityLinked, Success: true,
			Reason:    auth.AuthEventReasonChallengeVerified,
			UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36",
		},
		{
			OccurredAt: now.Add(-time.Minute), EventType: auth.AuthEventLoginFailed, Success: false,
			Reason: auth.AuthEventReasonInvalidCredentials, UserAgent: "  <rejected\x00client>  ",
		},
		{
			OccurredAt: now.Add(-2 * time.Minute), EventType: auth.AuthEventType("future_private_event"), Success: false,
			Reason: auth.AuthEventReason("future_private_reason"),
		},
	})
	if len(viewData) != 3 {
		t.Fatalf("securityEventViewData() = %#v", viewData)
	}
	if viewData[0].Title != "Sign-in identity connected" ||
		viewData[0].Detail != "A verified security request was completed." ||
		viewData[0].OccurredAt != "Aug 19, 2026 at 6:00 PM" ||
		viewData[0].Client != "Chrome on Linux" || viewData[0].Status != "Completed" || !viewData[0].Successful {
		t.Fatalf("successful security event view = %#v", viewData[0])
	}
	if viewData[1].Title != "Sign-in attempt failed" ||
		viewData[1].Detail != "The submitted verification was not accepted." ||
		viewData[1].Client != "<rejected client>" || viewData[1].Status != "Failed" || viewData[1].Successful {
		t.Fatalf("failed security event view = %#v", viewData[1])
	}
	if viewData[2].Title != "Security activity failed" ||
		viewData[2].Detail != "The attempt did not complete." || viewData[2].Client != "" ||
		strings.Contains(viewData[2].Title+viewData[2].Detail, "future_private") {
		t.Fatalf("unknown security event view = %#v", viewData[2])
	}
}

func TestSecurityActivityPageViewDataBuildsBoundedPagination(t *testing.T) {
	data := securityActivityPageViewData(&auth.SecurityEventPage{
		Events:      make([]auth.SecurityEventSummary, 5),
		TotalEvents: 45,
		Page:        3,
		TotalPages:  3,
		PageSize:    20,
	})
	if data.TotalEvents != 45 || data.Page != 3 || data.TotalPages != 3 ||
		data.FirstEvent != 41 || data.LastEvent != 45 || !data.HasPrevious || data.HasNext ||
		data.PreviousPage != 2 || data.NextPage != 4 || len(data.Events) != 5 {
		t.Fatalf("securityActivityPageViewData() = %#v", data)
	}
	empty := securityActivityPageViewData(&auth.SecurityEventPage{Page: 1, TotalPages: 1, PageSize: 20})
	if empty.FirstEvent != 0 || empty.LastEvent != 0 || empty.HasPrevious || empty.HasNext {
		t.Fatalf("empty securityActivityPageViewData() = %#v", empty)
	}
}

func TestParseSecurityActivityPageRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		query string
		want  int64
		ok    bool
	}{
		{query: "", want: 1, ok: true},
		{query: "?page=2", want: 2, ok: true},
		{query: "?page=0"},
		{query: "?page=-1"},
		{query: "?page=abc"},
		{query: "?page="},
		{query: "?page=1&page=2"},
	} {
		request := httptest.NewRequest(http.MethodGet, securityActivityPath+test.query, nil)
		page, err := parseSecurityActivityPage(request)
		if (err == nil) != test.ok || page != test.want {
			t.Fatalf("parseSecurityActivityPage(%q) = %d, %v", test.query, page, err)
		}
	}
}

func TestSecurityEventTitleCoversKnownEventTypesWithoutRawEnumLabels(t *testing.T) {
	for _, eventType := range []auth.AuthEventType{
		auth.AuthEventLoginSucceeded,
		auth.AuthEventLoginFailed,
		auth.AuthEventSessionRevoked,
		auth.AuthEventUserDisabled,
		auth.AuthEventUserEnabled,
		auth.AuthEventCredentialChanged,
		auth.AuthEventRecoveryUsed,
		auth.AuthEventEnrollmentIssued,
		auth.AuthEventEnrollmentRevoked,
		auth.AuthEventEnrollmentCompleted,
		auth.AuthEventCredentialResetCompleted,
		auth.AuthEventLocalRecoveryStarted,
		auth.AuthEventSetupTokenIssued,
		auth.AuthEventSetupTokenRotated,
		auth.AuthEventSetupTokenVerified,
		auth.AuthEventSetupTokenVerificationFailed,
		auth.AuthEventSetupCompleted,
		auth.AuthEventStepUpSucceeded,
		auth.AuthEventStepUpFailed,
		auth.AuthEventSecurityPolicyChanged,
		auth.AuthEventIdentityLinked,
		auth.AuthEventIdentityUnlinked,
	} {
		title := securityEventTitle(eventType, true)
		if strings.TrimSpace(title) == "" || strings.Contains(title, string(eventType)) {
			t.Fatalf("securityEventTitle(%q) = %q", eventType, title)
		}
	}
}

func TestSecurityActivityDialogPaginatesOnlyCurrentUsersEventsWithoutAuditInternals(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	current, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || current == nil {
		t.Fatalf("load current security session = %#v, %v", current, err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, created_at, updated_at)
		VALUES ('foreign-event-user', 'foreign-events', 'foreign-events',
		        'Foreign events', 'active', 1, ?, ?)`, now, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		id        string
		actor     any
		subject   any
		session   any
		occurred  time.Time
		eventType auth.AuthEventType
		success   int
		reason    auth.AuthEventReason
		agent     string
		metadata  string
	}{
		{
			id: "own-failed-event", actor: current.UserID, subject: current.UserID, session: current.ID,
			occurred: now.Add(2 * time.Second), eventType: auth.AuthEventLoginFailed, success: 0,
			reason: auth.AuthEventReasonInvalidCredentials, agent: "<script>event-client</script>",
			metadata: `{"provider_subject":"private-provider-subject"}`,
		},
		{
			id: "own-session-event", actor: current.UserID, subject: current.UserID, session: current.ID,
			occurred: now.Add(time.Second), eventType: auth.AuthEventSessionRevoked, success: 1,
			reason:   auth.AuthEventReasonUserAction,
			agent:    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36",
			metadata: `{"target_session_id":"private-target-session"}`,
		},
		{
			id: "foreign-subject-event", actor: "foreign-event-user", subject: "foreign-event-user", session: nil,
			occurred: now.Add(3 * time.Second), eventType: auth.AuthEventLoginSucceeded, success: 1,
			reason: auth.AuthEventReasonChallengeVerified, agent: "Foreign-only browser",
			metadata: `{"private":"foreign-event-secret"}`,
		},
		{
			id: "current-actor-only-event", actor: current.UserID, subject: "foreign-event-user", session: current.ID,
			occurred: now.Add(4 * time.Second), eventType: auth.AuthEventSecurityPolicyChanged, success: 1,
			reason: auth.AuthEventReasonAdministratorAction, agent: "Actor-only browser", metadata: `{}`,
		},
	} {
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, request_id, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'private-source-hash', 'private-request-id', ?)`,
			event.id, event.occurred, event.actor, event.subject, event.session,
			event.eventType, event.success, event.reason, event.agent, event.metadata,
		); err != nil {
			t.Fatalf("insert security event %q: %v", event.id, err)
		}
	}
	for index := 0; index < 25; index++ {
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{}')`,
			fmt.Sprintf("owned-history-%02d", index), now.Add(-time.Duration(index+1)*time.Minute),
			current.UserID, current.UserID, current.ID, auth.AuthEventCredentialChanged,
			auth.AuthEventReasonUserAction, fmt.Sprintf("Owned history %02d", index),
		); err != nil {
			t.Fatalf("insert paginated security event %d: %v", index, err)
		}
	}
	var totalEvents int64
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE subject_user_id = ?`, current.UserID,
	).Scan(&totalEvents); err != nil {
		t.Fatal(err)
	}
	totalPages := (totalEvents + 19) / 20

	page := getSecuritySettings(t, stack, sessionCookie)
	if page.Code != 200 {
		t.Fatalf("security activity page = %d %q", page.Code, page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		`data-security-events`, "Your security activity", fmt.Sprintf("%d events", totalEvents),
		"View activity", `hx-get="/settings/security/activity?page=1"`,
		`hx-target="#security-activity-dialog-body"`, `id="security-activity-dialog"`,
		"Loading security activity…",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security activity page missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		"Sign-in attempt failed", "The submitted verification was not accepted.",
		"Session signed out", "Requested from this account.", "Chrome on Linux",
		`<script>event-client</script>`, `&lt;script&gt;event-client&lt;/script&gt;`,
		"Foreign-only browser", "Actor-only browser", "Owned history",
		"own-failed-event", "own-session-event", "foreign-subject-event", "current-actor-only-event",
		current.ID, sessionCookie.Value, "private-provider-subject", "private-target-session",
		"foreign-event-secret", "private-source-hash", "private-request-id",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("security activity page exposed forbidden value %q", forbidden)
		}
	}

	firstRequest := httptest.NewRequest(http.MethodGet, securityActivityPath+"?page=1", nil)
	firstRequest.Header.Set("HX-Request", "true")
	firstRequest.AddCookie(sessionCookie)
	firstPage := httptest.NewRecorder()
	stack.ServeHTTP(firstPage, firstRequest)
	if firstPage.Code != http.StatusOK {
		t.Fatalf("first security activity page = %d %q", firstPage.Code, firstPage.Body.String())
	}
	firstHTML := firstPage.Body.String()
	for _, want := range []string{
		`data-security-activity-page`, `aria-label="Security activity events"`,
		"Sign-in attempt failed", "The submitted verification was not accepted.",
		"Session signed out", "Requested from this account.", "Chrome on Linux",
		`&lt;script&gt;event-client&lt;/script&gt;`,
		fmt.Sprintf("Page 1 of %d", totalPages), `hx-get="/settings/security/activity?page=2"`, "Next",
		"Only events affecting this Gofer account are shown",
	} {
		if !strings.Contains(firstHTML, want) {
			t.Fatalf("first security activity page missing %q: %q", want, firstHTML)
		}
	}
	for _, forbidden := range []string{
		`<script>event-client</script>`, "Foreign-only browser", "Actor-only browser",
		"own-failed-event", "own-session-event", "foreign-subject-event", "current-actor-only-event",
		current.ID, sessionCookie.Value, "private-provider-subject", "private-target-session",
		"foreign-event-secret", "private-source-hash", "private-request-id",
	} {
		if strings.Contains(firstHTML, forbidden) {
			t.Fatalf("first security activity page exposed forbidden value %q", forbidden)
		}
	}
	if firstPage.Header().Get("Cache-Control") != "no-store" ||
		firstPage.Header().Get("Referrer-Policy") != "no-referrer" ||
		firstPage.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("security activity response headers = %#v", firstPage.Header())
	}

	secondRequest := httptest.NewRequest(http.MethodGet, securityActivityPath+"?page=2", nil)
	secondRequest.Header.Set("HX-Request", "true")
	secondRequest.AddCookie(sessionCookie)
	secondPage := httptest.NewRecorder()
	stack.ServeHTTP(secondPage, secondRequest)
	if secondPage.Code != http.StatusOK {
		t.Fatalf("second security activity page = %d %q", secondPage.Code, secondPage.Body.String())
	}
	secondHTML := secondPage.Body.String()
	for _, want := range []string{
		fmt.Sprintf("Page 2 of %d", totalPages), `hx-get="/settings/security/activity?page=1"`,
		"Previous", "Owned history",
	} {
		if !strings.Contains(secondHTML, want) {
			t.Fatalf("second security activity page missing %q: %q", want, secondHTML)
		}
	}
}

func TestSecurityActivityEndpointRequiresHTMXAndFreshVerification(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	direct := getSecuritySettingsPath(t, stack, securityActivityPath+"?page=1", sessionCookie)
	if direct.Code != http.StatusSeeOther || direct.Header().Get("Location") != "/settings/security" {
		t.Fatalf("direct security activity request = %d location %q", direct.Code, direct.Header().Get("Location"))
	}

	malformedRequest := httptest.NewRequest(http.MethodGet, securityActivityPath+"?page=0", nil)
	malformedRequest.Header.Set("HX-Request", "true")
	malformedRequest.AddCookie(sessionCookie)
	malformed := httptest.NewRecorder()
	stack.ServeHTTP(malformed, malformedRequest)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed security activity request = %d %q", malformed.Code, malformed.Body.String())
	}

	current, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || current == nil {
		t.Fatalf("load current security session = %#v, %v", current, err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), current.ID,
	); err != nil {
		t.Fatal(err)
	}
	staleRequest := httptest.NewRequest(http.MethodGet, securityActivityPath+"?page=1", nil)
	staleRequest.Header.Set("HX-Request", "true")
	staleRequest.AddCookie(sessionCookie)
	stale := httptest.NewRecorder()
	stack.ServeHTTP(stale, staleRequest)
	if stale.Code != http.StatusNoContent || stale.Header().Get("HX-Redirect") != "/settings/security?verification_required=1" || stale.Body.Len() != 0 {
		t.Fatalf("stale security activity request = %d redirect %q body %q", stale.Code, stale.Header().Get("HX-Redirect"), stale.Body.String())
	}
}
