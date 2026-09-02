package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAdministratorSecurityEventPageRequiresExactVerifiedManagementAdministrator(t *testing.T) {
	now := time.Date(2026, time.August, 31, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	ordinarySessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)

	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "ordinary", ordinarySessionID, AdministratorSecurityEventFilterAll, 1,
	); page != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary administrator event page = %#v, %v", page, err)
	}
	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", ordinarySessionID, AdministratorSecurityEventFilterAll, 1,
	); page != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("cross-user administrator event session = %#v, %v", page, err)
	}
	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", "", AdministratorSecurityEventFilterAll, 1,
	); page != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("missing administrator event session = %#v, %v", page, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`,
		now.Add(-securityStepUpMaximumAge-time.Second), adminSessionID,
	); err != nil {
		t.Fatal(err)
	}
	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", adminSessionID, AdministratorSecurityEventFilterAll, 1,
	); page != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale administrator event page = %#v, %v", page, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, now, adminSessionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'disabled' WHERE id = 'administrator'`); err != nil {
		t.Fatal(err)
	}
	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", adminSessionID, AdministratorSecurityEventFilterAll, 1,
	); page != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("disabled administrator event page = %#v, %v", page, err)
	}
}

func TestAdministratorSecurityEventPagePaginatesSanitizedInstanceHistory(t *testing.T) {
	now := time.Date(2026, time.August, 31, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)

	for _, event := range []struct {
		id        string
		occurred  time.Time
		actor     any
		subject   any
		eventType AuthEventType
		success   int
		reason    AuthEventReason
		agent     string
		metadata  string
	}{
		{
			id: "deleted-user", occurred: now.Add(3 * time.Minute), actor: "administrator", subject: nil,
			eventType: AuthEventUserDeleted, success: 1, reason: AuthEventReasonAdministratorAction,
			agent: "Admin Browser", metadata: `{"target_username":"removed-user","target_user_id":"private-deleted-id","secret":"private-metadata"}`,
		},
		{
			id: "failed-login", occurred: now.Add(2 * time.Minute), actor: nil, subject: "person",
			eventType: AuthEventLoginFailed, success: 0, reason: AuthEventReasonInvalidCredentials,
			agent: "Rejected Browser", metadata: `{"submitted_identifier":"private-login-value"}`,
		},
		{
			id: "system-setup", occurred: now.Add(time.Minute), actor: nil, subject: nil,
			eventType: AuthEventSetupTokenIssued, success: 1, reason: AuthEventReasonSystemInitialization,
			agent: "Setup Browser", metadata: `{"setup_token":"private-setup-value"}`,
		},
	} {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				request_id, event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, 'private-request-id', ?, ?, ?, ?, 'private-source-hash', ?)`,
			event.id, event.occurred, event.actor, event.subject, adminSessionID,
			event.eventType, event.success, event.reason, event.agent, event.metadata,
		); err != nil {
			t.Fatalf("insert administrator security event %q: %v", event.id, err)
		}
	}
	for index := 0; index < 52; index++ {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, metadata_json
			) VALUES (?, ?, 'person', 'person', ?, 1, ?, ?, '{}')`,
			fmt.Sprintf("history-%02d", index), now.Add(-time.Duration(index+1)*time.Minute),
			AuthEventCredentialChanged, AuthEventReasonUserAction, fmt.Sprintf("History Browser %02d", index),
		); err != nil {
			t.Fatal(err)
		}
	}

	page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", adminSessionID, AdministratorSecurityEventFilterAll, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page == nil || page.TotalEvents != 55 || page.Page != 1 || page.TotalPages != 2 ||
		page.PageSize != administratorSecurityEventPageSize || len(page.Events) != 50 {
		t.Fatalf("administrator security event page = %#v", page)
	}
	if event := page.Events[0]; event.EventType != AuthEventUserDeleted || !event.Success ||
		event.ActorUsername != "administrator" || event.SubjectUsername != "removed-user" || !event.SubjectDeleted ||
		event.UserAgent != "Admin Browser" {
		t.Fatalf("deleted-user administrator event = %#v", event)
	}
	if event := page.Events[1]; event.EventType != AuthEventLoginFailed || event.Success ||
		event.ActorUsername != "" || event.SubjectUsername != "person" || event.SubjectDeleted {
		t.Fatalf("failed-login administrator event = %#v", event)
	}
	if event := page.Events[2]; event.EventType != AuthEventSetupTokenIssued ||
		event.ActorUsername != "" || event.SubjectUsername != "" || event.SubjectDeleted {
		t.Fatalf("system administrator event = %#v", event)
	}

	last, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", adminSessionID, AdministratorSecurityEventFilterAll, 999,
	)
	if err != nil || last == nil || last.Page != 2 || last.TotalPages != 2 || len(last.Events) != 5 {
		t.Fatalf("clamped administrator security event page = %#v, %v", last, err)
	}
	if last.Events[len(last.Events)-1].UserAgent != "History Browser 51" {
		t.Fatalf("last administrator security event = %#v", last.Events[len(last.Events)-1])
	}
}

func TestAdministratorSecurityEventPageFiltersServerSideBySupportedCategory(t *testing.T) {
	now := time.Date(2026, time.September, 2, 3, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)

	events := []struct {
		eventType AuthEventType
		success   int
	}{
		{AuthEventLoginSucceeded, 1},
		{AuthEventLoginFailed, 0},
		{AuthEventSessionRevoked, 1},
		{AuthEventStepUpFailed, 0},
		{AuthEventRecoveryUsed, 1},
		{AuthEventCredentialResetRequested, 1},
		{AuthEventCredentialResetCompleted, 1},
		{AuthEventLocalRecoveryStarted, 1},
		{AuthEventSecurityPolicyChanged, 1},
		{AuthEventIdentityLinked, 1},
		{AuthEventIdentityUnlinked, 1},
		{AuthEventCredentialChanged, 0},
	}
	for index, event := range events {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, metadata_json
			) VALUES (?, ?, 'administrator', 'person', ?, ?, ?, 'Filter Browser', '{}')`,
			fmt.Sprintf("filter-event-%02d", index), now.Add(time.Duration(index)*time.Second),
			event.eventType, event.success, AuthEventReasonUserAction,
		); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		filter AdministratorSecurityEventFilter
		want   map[AuthEventType]bool
	}{
		{
			filter: AdministratorSecurityEventFilterFailures,
			want: map[AuthEventType]bool{
				AuthEventLoginFailed:       true,
				AuthEventStepUpFailed:      true,
				AuthEventCredentialChanged: true,
			},
		},
		{
			filter: AdministratorSecurityEventFilterRecovery,
			want: map[AuthEventType]bool{
				AuthEventRecoveryUsed:             true,
				AuthEventCredentialResetRequested: true,
				AuthEventCredentialResetCompleted: true,
				AuthEventLocalRecoveryStarted:     true,
			},
		},
		{
			filter: AdministratorSecurityEventFilterPolicy,
			want:   map[AuthEventType]bool{AuthEventSecurityPolicyChanged: true},
		},
		{
			filter: AdministratorSecurityEventFilterIdentities,
			want: map[AuthEventType]bool{
				AuthEventIdentityLinked:   true,
				AuthEventIdentityUnlinked: true,
			},
		},
		{
			filter: AdministratorSecurityEventFilterSessions,
			want: map[AuthEventType]bool{
				AuthEventLoginSucceeded: true,
				AuthEventLoginFailed:    true,
				AuthEventSessionRevoked: true,
				AuthEventStepUpFailed:   true,
			},
		},
	}
	for _, test := range tests {
		t.Run(string(test.filter), func(t *testing.T) {
			page, err := manager.ListAdministratorSecurityEventPage(
				t.Context(), "administrator", adminSessionID, test.filter, 1,
			)
			if err != nil {
				t.Fatal(err)
			}
			if page.TotalEvents != int64(len(test.want)) || len(page.Events) != len(test.want) {
				t.Fatalf("filtered administrator event page = %#v", page)
			}
			for _, event := range page.Events {
				if !test.want[event.EventType] {
					t.Fatalf("filter %q returned event %q", test.filter, event.EventType)
				}
			}
		})
	}

	if page, err := manager.ListAdministratorSecurityEventPage(
		t.Context(), "administrator", adminSessionID, "not-supported", 1,
	); page != nil || err == nil {
		t.Fatalf("invalid administrator event filter = %#v, %v", page, err)
	}
}

func TestDeletedSecurityEventUsernameOnlyProjectsBoundedDeletionMetadata(t *testing.T) {
	if got := deletedSecurityEventUsername(
		AuthEventUserDeleted, `{"target_username":"removed-user","target_user_id":"private-id"}`,
	); got != "removed-user" {
		t.Fatalf("deleted username = %q", got)
	}
	for _, test := range []struct {
		eventType AuthEventType
		metadata  string
	}{
		{eventType: AuthEventLoginFailed, metadata: `{"target_username":"not-a-deletion"}`},
		{eventType: AuthEventUserDeleted, metadata: `{"target_username":7}`},
		{eventType: AuthEventUserDeleted, metadata: `{"target_username":"` + strings.Repeat("a", usernameMaximumLength+1) + `"}`},
		{eventType: AuthEventUserDeleted, metadata: `{`},
	} {
		if got := deletedSecurityEventUsername(test.eventType, test.metadata); got != "" {
			t.Fatalf("unsafe deleted username projection = %q for %q", got, test.metadata)
		}
	}
}
