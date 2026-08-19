package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSecurityEventQueriesRequireExactFreshSessionAndReturnOnlySubjectEvents(t *testing.T) {
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

	overview, err := manager.GetSecurityEventOverview(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	if overview == nil || overview.TotalEvents != 3 {
		t.Fatalf("GetSecurityEventOverview() = %#v", overview)
	}
	page, err := manager.ListSecurityEventPage(t.Context(), current.Token, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page == nil || page.TotalEvents != 3 || page.Page != 1 || page.TotalPages != 1 ||
		page.PageSize != securityEventPageSize || len(page.Events) != 3 {
		t.Fatalf("ListSecurityEventPage() = %#v", page)
	}
	if page.Events[0].EventType != AuthEventIdentityLinked || !page.Events[0].Success ||
		page.Events[0].Reason != AuthEventReasonChallengeVerified ||
		page.Events[0].UserAgent != "  <security\x00client>  " ||
		!page.Events[0].OccurredAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("newest security event = %#v", page.Events[0])
	}
	if page.Events[1].EventType != AuthEventEnrollmentIssued || !page.Events[1].Success ||
		page.Events[2].EventType != AuthEventLoginFailed || page.Events[2].Success {
		t.Fatalf("security event ordering = %#v", page.Events)
	}

	foreignOverview, err := manager.GetSecurityEventOverview(t.Context(), foreign.Token)
	if err != nil || foreignOverview == nil || foreignOverview.TotalEvents != 2 {
		t.Fatalf("foreign GetSecurityEventOverview() = %#v, %v", foreignOverview, err)
	}
	foreignPage, err := manager.ListSecurityEventPage(t.Context(), foreign.Token, 1)
	if err != nil || foreignPage == nil || len(foreignPage.Events) != 2 {
		t.Fatalf("foreign ListSecurityEventPage() = %#v, %v", foreignPage, err)
	}
	for _, event := range foreignPage.Events {
		if event.UserAgent == "Rejected Browser" || event.UserAgent == "Admin Browser" {
			t.Fatalf("foreign event page exposed another subject: %#v", foreignPage.Events)
		}
	}
	if result, err := manager.GetSecurityEventOverview(t.Context(), "invalid-token"); result != nil || !errors.Is(err, ErrSecuritySessionInvalid) {
		t.Fatalf("invalid GetSecurityEventOverview() = %#v, %v", result, err)
	}
	if result, err := manager.ListSecurityEventPage(t.Context(), "invalid-token", 1); result != nil || !errors.Is(err, ErrSecuritySessionInvalid) {
		t.Fatalf("invalid ListSecurityEventPage() = %#v, %v", result, err)
	}
	clock.now = now.Add(securityStepUpMaximumAge + time.Second)
	if result, err := manager.GetSecurityEventOverview(t.Context(), current.Token); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale GetSecurityEventOverview() = %#v, %v", result, err)
	}
	if result, err := manager.ListSecurityEventPage(t.Context(), current.Token, 1); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale ListSecurityEventPage() = %#v, %v", result, err)
	}
}

func TestListSecurityEventPagePaginatesNewestHistory(t *testing.T) {
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
	totalEvents := securityEventPageSize*2 + 5
	for index := int64(0); index < totalEvents; index++ {
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
	overview, err := manager.GetSecurityEventOverview(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	if overview == nil || overview.TotalEvents != totalEvents {
		t.Fatalf("GetSecurityEventOverview() = %#v", overview)
	}
	for _, test := range []struct {
		requested int64
		wantPage  int64
		wantCount int
		wantFirst string
		wantLast  string
	}{
		{requested: 0, wantPage: 1, wantCount: 20, wantFirst: "Browser 00", wantLast: "Browser 19"},
		{requested: 2, wantPage: 2, wantCount: 20, wantFirst: "Browser 20", wantLast: "Browser 39"},
		{requested: 3, wantPage: 3, wantCount: 5, wantFirst: "Browser 40", wantLast: "Browser 44"},
		{requested: 999, wantPage: 3, wantCount: 5, wantFirst: "Browser 40", wantLast: "Browser 44"},
	} {
		page, err := manager.ListSecurityEventPage(t.Context(), current.Token, test.requested)
		if err != nil {
			t.Fatalf("ListSecurityEventPage(%d): %v", test.requested, err)
		}
		if page == nil || page.TotalEvents != totalEvents || page.TotalPages != 3 ||
			page.Page != test.wantPage || page.PageSize != securityEventPageSize || len(page.Events) != test.wantCount {
			t.Fatalf("ListSecurityEventPage(%d) = %#v", test.requested, page)
		}
		if page.Events[0].UserAgent != test.wantFirst || page.Events[len(page.Events)-1].UserAgent != test.wantLast {
			t.Fatalf("ListSecurityEventPage(%d) ordering = first:%#v last:%#v", test.requested, page.Events[0], page.Events[len(page.Events)-1])
		}
	}
}
