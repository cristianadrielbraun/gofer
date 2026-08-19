package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestListSecurityEventsRequiresExactFreshSessionAndReturnsOnlySubjectEvents(t *testing.T) {
	now := time.Date(2026, time.August, 19, 16, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"current-session", "foreign-session"},
		tokens: []string{"current-token", "foreign-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	insertActiveUser(t, manager, "foreign-person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := manager.CreateAuthenticatedSession(
		t.Context(), "foreign-person", "Foreign Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []*Session{current, foreign} {
		if steppedUp, err := manager.RecordSessionStepUp(
			t.Context(), session.UserID, session.ID, AuthenticationMethodPassword,
		); err != nil || !steppedUp {
			t.Fatalf("RecordSessionStepUp(%q) = %t, %v", session.ID, steppedUp, err)
		}
	}
	for _, event := range []struct {
		id        string
		actor     any
		subject   any
		session   any
		occurred  time.Time
		eventType AuthEventType
		success   int
		reason    AuthEventReason
		agent     string
		metadata  string
	}{
		{
			id: "own-newest", actor: "person", subject: "person", session: current.ID,
			occurred: now.Add(-time.Minute), eventType: AuthEventIdentityLinked, success: 1,
			reason: AuthEventReasonChallengeVerified, agent: "  <security\x00client>  ",
			metadata: `{"provider":"google","subject":"provider-secret"}`,
		},
		{
			id: "own-admin-event", actor: "foreign-person", subject: "person", session: nil,
			occurred: now.Add(-2 * time.Minute), eventType: AuthEventEnrollmentIssued, success: 1,
			reason: AuthEventReasonAdministratorAction, agent: "Admin Browser",
			metadata: `{"token_id":"internal-invitation-id"}`,
		},
		{
			id: "own-older", actor: nil, subject: "person", session: nil,
			occurred: now.Add(-3 * time.Minute), eventType: AuthEventLoginFailed, success: 0,
			reason: AuthEventReasonInvalidCredentials, agent: "Rejected Browser",
			metadata: `{"source":"secret-source"}`,
		},
		{
			id: "actor-only", actor: "person", subject: "foreign-person", session: current.ID,
			occurred: now, eventType: AuthEventSecurityPolicyChanged, success: 1,
			reason: AuthEventReasonAdministratorAction, agent: "Actor Browser", metadata: `{}`,
		},
		{
			id: "foreign-subject", actor: "foreign-person", subject: "foreign-person", session: foreign.ID,
			occurred: now, eventType: AuthEventLoginSucceeded, success: 1,
			reason: AuthEventReasonChallengeVerified, agent: "Foreign Browser", metadata: `{}`,
		},
		{
			id: "no-subject", actor: nil, subject: nil, session: nil,
			occurred: now, eventType: AuthEventSetupTokenIssued, success: 1,
			reason: AuthEventReasonSystemInitialization, agent: "Setup Browser", metadata: `{}`,
		},
	} {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, request_id, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'private-source-hash', 'private-request-id', ?)`,
			event.id, event.occurred, event.actor, event.subject, event.session,
			event.eventType, event.success, event.reason, event.agent, event.metadata,
		); err != nil {
			t.Fatalf("insert event %q: %v", event.id, err)
		}
	}

	list, err := manager.ListSecurityEvents(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil || list.Truncated || len(list.Events) != 3 {
		t.Fatalf("ListSecurityEvents() = %#v", list)
	}
	if list.Events[0].EventType != AuthEventIdentityLinked || !list.Events[0].Success ||
		list.Events[0].Reason != AuthEventReasonChallengeVerified ||
		list.Events[0].UserAgent != "  <security\x00client>  " ||
		!list.Events[0].OccurredAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("newest security event = %#v", list.Events[0])
	}
	if list.Events[1].EventType != AuthEventEnrollmentIssued || !list.Events[1].Success ||
		list.Events[2].EventType != AuthEventLoginFailed || list.Events[2].Success {
		t.Fatalf("security event ordering = %#v", list.Events)
	}

	foreignList, err := manager.ListSecurityEvents(t.Context(), foreign.Token)
	if err != nil || foreignList == nil || len(foreignList.Events) != 2 {
		t.Fatalf("foreign ListSecurityEvents() = %#v, %v", foreignList, err)
	}
	for _, event := range foreignList.Events {
		if event.UserAgent == "Rejected Browser" || event.UserAgent == "Admin Browser" {
			t.Fatalf("foreign event list exposed another subject: %#v", foreignList.Events)
		}
	}
	if list, err := manager.ListSecurityEvents(t.Context(), "invalid-token"); list != nil || !errors.Is(err, ErrSecuritySessionInvalid) {
		t.Fatalf("invalid ListSecurityEvents() = %#v, %v", list, err)
	}
	clock.now = now.Add(securityStepUpMaximumAge + time.Second)
	if list, err := manager.ListSecurityEvents(t.Context(), current.Token); list != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale ListSecurityEvents() = %#v, %v", list, err)
	}
}

func TestListSecurityEventsCapsNewestHistory(t *testing.T) {
	now := time.Date(2026, time.August, 19, 17, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"current-session"},
		tokens: []string{"current-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	for index := 0; index < securityEventListLimit+5; index++ {
		occurredAt := now.Add(-time.Duration(index+1) * time.Minute)
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, subject_user_id, event_type, success, reason, user_agent
			) VALUES (?, ?, 'person', ?, 1, '', ?)`,
			fmt.Sprintf("event-%02d", index), occurredAt,
			AuthEventCredentialChanged, fmt.Sprintf("Browser %02d", index),
		); err != nil {
			t.Fatal(err)
		}
	}
	list, err := manager.ListSecurityEvents(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil || !list.Truncated || len(list.Events) != securityEventListLimit {
		t.Fatalf("capped ListSecurityEvents() = %#v", list)
	}
	if list.Events[0].UserAgent != "Browser 00" ||
		list.Events[len(list.Events)-1].UserAgent != fmt.Sprintf("Browser %02d", securityEventListLimit-1) {
		t.Fatalf("capped event ordering = first:%#v last:%#v", list.Events[0], list.Events[len(list.Events)-1])
	}
}
