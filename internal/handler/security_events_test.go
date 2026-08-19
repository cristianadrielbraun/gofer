package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestSecurityEventViewDataUsesOnlyFriendlyBoundedFields(t *testing.T) {
	now := time.Date(2026, time.August, 19, 18, 0, 0, 0, time.Local)
	views, truncated := securityEventViewData(&auth.SecurityEventList{
		Truncated: true,
		Events: []auth.SecurityEventSummary{
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
		},
	})
	if !truncated || len(views) != 3 {
		t.Fatalf("securityEventViewData() = %#v, %t", views, truncated)
	}
	if views[0].Title != "Sign-in identity connected" ||
		views[0].Detail != "A verified security request was completed." ||
		views[0].OccurredAt != "Aug 19, 2026 at 6:00 PM" ||
		views[0].Client != "Chrome on Linux" || views[0].Status != "Completed" || !views[0].Successful {
		t.Fatalf("successful security event view = %#v", views[0])
	}
	if views[1].Title != "Sign-in attempt failed" ||
		views[1].Detail != "The submitted verification was not accepted." ||
		views[1].Client != "<rejected client>" || views[1].Status != "Failed" || views[1].Successful {
		t.Fatalf("failed security event view = %#v", views[1])
	}
	if views[2].Title != "Security activity failed" ||
		views[2].Detail != "The attempt did not complete." || views[2].Client != "" ||
		strings.Contains(views[2].Title+views[2].Detail, "future_private") {
		t.Fatalf("unknown security event view = %#v", views[2])
	}
}

func TestSecurityEventTitleCoversKnownEventTypesWithoutRawEnumLabels(t *testing.T) {
	for _, eventType := range []auth.AuthEventType{
		auth.AuthEventLoginSucceeded,
		auth.AuthEventLoginFailed,
		auth.AuthEventSessionRevoked,
		auth.AuthEventUserDisabled,
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

func TestSecuritySettingsListsOnlyCurrentUsersSecurityEventsWithoutAuditInternals(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	current, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || current == nil {
		t.Fatalf("load current security session = %#v, %v", current, err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, created_at, updated_at)
		VALUES ('foreign-event-user', 'foreign-events@example.com', 'foreign-events@example.com',
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

	page := getSecuritySettings(t, stack, sessionCookie)
	if page.Code != 200 {
		t.Fatalf("security activity page = %d %q", page.Code, page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		`data-security-events`, `aria-label="Recent security activity"`,
		"Sign-in attempt failed", "The submitted verification was not accepted.",
		"Session signed out", "Requested from this account.", "Chrome on Linux",
		`&lt;script&gt;event-client&lt;/script&gt;`, "Initial security setup completed",
		"Only events affecting this Gofer account are shown",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security activity page missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>event-client</script>`, "Foreign-only browser", "Actor-only browser",
		"own-failed-event", "own-session-event", "foreign-subject-event", "current-actor-only-event",
		current.ID, sessionCookie.Value, "private-provider-subject", "private-target-session",
		"foreign-event-secret", "private-source-hash", "private-request-id",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("security activity page exposed forbidden value %q", forbidden)
		}
	}
}
