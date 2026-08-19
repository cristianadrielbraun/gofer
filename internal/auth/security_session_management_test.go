package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func securitySessionActionReferenceFor(t *testing.T, list *SecuritySessionList, sessionID string) string {
	t.Helper()
	if list == nil {
		t.Fatal("security session list is nil")
	}
	for _, session := range list.Sessions {
		if session.ID == sessionID {
			if !canonicalSecuritySessionActionReference(session.ActionReference) {
				t.Fatalf("security session %q action reference = %q", sessionID, session.ActionReference)
			}
			return session.ActionReference
		}
	}
	t.Fatalf("security session %q was not listed", sessionID)
	return ""
}

func TestRevokeSecuritySessionRequiresBoundReferenceOwnershipAndFreshStepUp(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: []string{
			"current-session", "target-session", "alternate-session", "foreign-session",
			"bound-rejection-event", "current-rejection-event", "stale-rejection-event",
			"successful-revocation-event", "replay-rejection-event",
		},
		tokens: []string{"current-token", "target-token", "alternate-token", "foreign-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	insertActiveUser(t, manager, "other-person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Target Browser", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant,
	)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Alternate Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := manager.CreateAuthenticatedSession(
		t.Context(), "other-person", "Foreign Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []*Session{current, alternate} {
		if steppedUp, err := manager.RecordSessionStepUp(
			t.Context(), session.UserID, session.ID, AuthenticationMethodPassword,
		); err != nil || !steppedUp {
			t.Fatalf("RecordSessionStepUp(%q) = %t, %v", session.ID, steppedUp, err)
		}
	}

	list, err := manager.ListSecuritySessions(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	actionReference := securitySessionActionReferenceFor(t, list, target.ID)
	if result, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, "not-a-canonical-reference", "Rejected Browser",
	); result != nil || !errors.Is(err, ErrSecuritySessionTargetInvalid) {
		t.Fatalf("malformed RevokeSecuritySession() = %#v, %v", result, err)
	}
	if result, err := manager.RevokeSecuritySession(
		t.Context(), alternate.Token, actionReference, "Alternate Browser",
	); result != nil || !errors.Is(err, ErrSecuritySessionTargetInvalid) {
		t.Fatalf("cross-session RevokeSecuritySession() = %#v, %v", result, err)
	}
	currentReference, err := manager.securitySessionActionReference(current.ID, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, currentReference, "Current Browser",
	); result != nil || !errors.Is(err, ErrSecuritySessionTargetInvalid) {
		t.Fatalf("current-session RevokeSecuritySession() = %#v, %v", result, err)
	}

	clock.now = now.Add(securityStepUpMaximumAge + time.Second)
	if result, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, actionReference, "Stale Browser",
	); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale RevokeSecuritySession() = %#v, %v", result, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), target.Token); err != nil || found == nil {
		t.Fatalf("target after rejected revocations = %#v, %v", found, err)
	}
	var rejectedEvents int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&rejectedEvents); err != nil || rejectedEvents != 0 {
		t.Fatalf("events after rejected revocations = %d, %v", rejectedEvents, err)
	}

	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("refresh RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	result, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, actionReference, "  Security Browser/1.0  ",
	)
	if err != nil || result == nil || result.RevokedSessions != 1 {
		t.Fatalf("RevokeSecuritySession() = %#v, %v", result, err)
	}
	if replayed, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, actionReference, "Replay Browser",
	); replayed != nil || !errors.Is(err, ErrSecuritySessionTargetInvalid) {
		t.Fatalf("replayed RevokeSecuritySession() = %#v, %v", replayed, err)
	}
	for _, session := range []*Session{current, alternate, foreign} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("preserved session %q = %#v, %v", session.ID, found, err)
		}
	}
	if found, err := manager.GetSessionByToken(t.Context(), target.Token); err != nil || found != nil {
		t.Fatalf("revoked target session = %#v, %v", found, err)
	}

	var revokedAt time.Time
	var revokedBy, revocationReason string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at, revoked_by, revocation_reason FROM sessions WHERE id = ?`, target.ID,
	).Scan(&revokedAt, &revokedBy, &revocationReason); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Equal(clock.now) || revokedBy != current.UserID || revocationReason != string(SessionRevocationLogout) {
		t.Fatalf("target revocation = at:%v by:%q reason:%q", revokedAt, revokedBy, revocationReason)
	}

	var eventCount, success int
	var eventType, reason, actorID, subjectID, sessionID, userAgent, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), event_type, success, reason, actor_user_id, subject_user_id,
		       session_id, user_agent, metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventSessionRevoked,
	).Scan(&eventCount, &eventType, &success, &reason, &actorID, &subjectID, &sessionID, &userAgent, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || eventType != string(AuthEventSessionRevoked) || success != 1 ||
		reason != string(AuthEventReasonUserAction) || actorID != current.UserID ||
		subjectID != current.UserID || sessionID != current.ID || userAgent != "Security Browser/1.0" ||
		metadata != `{"scope":"single","revoked_sessions":1}` {
		t.Fatalf("session revocation event = count:%d type:%q success:%d reason:%q actor:%q subject:%q session:%q agent:%q metadata:%q",
			eventCount, eventType, success, reason, actorID, subjectID, sessionID, userAgent, metadata)
	}
	for _, secret := range []string{target.ID, target.Token, hashToken(target.Token), actionReference} {
		if strings.Contains(metadata, secret) {
			t.Fatalf("session revocation metadata exposed target value %q: %q", secret, metadata)
		}
	}
}

func TestRevokeOtherSecuritySessionsPreservesCurrentAndAuditsCount(t *testing.T) {
	now := time.Date(2026, time.August, 19, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{
			"current-session", "other-session-one", "other-session-two", "foreign-session",
			"other-revocation-event", "empty-revocation-event",
		},
		tokens: []string{"current-token", "other-token-one", "other-token-two", "foreign-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	insertActiveUser(t, manager, "other-person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	otherOne, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Other Browser One", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	otherTwo, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Other Browser Two", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant,
	)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := manager.CreateAuthenticatedSession(
		t.Context(), "other-person", "Foreign Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}

	result, err := manager.RevokeOtherSecuritySessions(t.Context(), current.Token, "Security Browser")
	if err != nil || result == nil || result.RevokedSessions != 2 {
		t.Fatalf("RevokeOtherSecuritySessions() = %#v, %v", result, err)
	}
	for _, session := range []*Session{current, foreign} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("preserved session %q = %#v, %v", session.ID, found, err)
		}
	}
	for _, session := range []*Session{otherOne, otherTwo} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found != nil {
			t.Fatalf("revoked other session %q = %#v, %v", session.ID, found, err)
		}
	}
	var eventCount int
	var eventSessionID, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), session_id, metadata_json FROM auth_events
		WHERE event_type = ? AND reason = ?`, AuthEventSessionRevoked, AuthEventReasonUserAction,
	).Scan(&eventCount, &eventSessionID, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || eventSessionID != current.ID || metadata != `{"scope":"others","revoked_sessions":2}` {
		t.Fatalf("other-session revocation event = count:%d session:%q metadata:%q", eventCount, eventSessionID, metadata)
	}
	for _, secret := range []string{otherOne.ID, otherOne.Token, otherTwo.ID, otherTwo.Token} {
		if strings.Contains(metadata, secret) {
			t.Fatalf("other-session revocation metadata exposed target value %q: %q", secret, metadata)
		}
	}

	replayed, err := manager.RevokeOtherSecuritySessions(t.Context(), current.Token, "Security Browser")
	if err != nil || replayed == nil || replayed.RevokedSessions != 0 {
		t.Fatalf("replayed RevokeOtherSecuritySessions() = %#v, %v", replayed, err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ? AND reason = ?`,
		AuthEventSessionRevoked, AuthEventReasonUserAction,
	).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("events after empty revocation = %d, %v", eventCount, err)
	}
}

func TestRevokeSecuritySessionConcurrentRequestsRevokeAndAuditOnce(t *testing.T) {
	now := time.Date(2026, time.August, 19, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"current-session", "target-session", "revocation-event-one", "revocation-event-two"},
		tokens: []string{"current-token", "target-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Target Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	list, err := manager.ListSecuritySessions(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	actionReference := securitySessionActionReferenceFor(t, list, target.ID)

	type outcome struct {
		result *SecuritySessionRevocationResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			ready.Done()
			<-start
			result, err := manager.RevokeSecuritySession(
				t.Context(), current.Token, actionReference, "Concurrent Browser",
			)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	ready.Wait()
	close(start)
	var succeeded, rejected int
	for range 2 {
		outcome := <-outcomes
		switch {
		case outcome.err == nil && outcome.result != nil && outcome.result.RevokedSessions == 1:
			succeeded++
		case outcome.result == nil && errors.Is(outcome.err, ErrSecuritySessionTargetInvalid):
			rejected++
		default:
			t.Fatalf("concurrent RevokeSecuritySession() = %#v, %v", outcome.result, outcome.err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent outcomes = succeeded:%d rejected:%d", succeeded, rejected)
	}
	var eventCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ? AND reason = ?`,
		AuthEventSessionRevoked, AuthEventReasonUserAction,
	).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("concurrent event count = %d, %v", eventCount, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), target.Token); err != nil || found != nil {
		t.Fatalf("concurrently revoked target = %#v, %v", found, err)
	}
}

func TestRevokeSecuritySessionRollsBackWhenAuditEventFails(t *testing.T) {
	now := time.Date(2026, time.August, 19, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"current-session", "target-session", "rollback-event"},
		tokens: []string{"current-token", "target-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Current Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Target Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	list, err := manager.ListSecuritySessions(t.Context(), current.Token)
	if err != nil {
		t.Fatal(err)
	}
	actionReference := securitySessionActionReferenceFor(t, list, target.ID)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_user_session_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'session_revoked' AND NEW.reason = 'user_action'
		BEGIN SELECT RAISE(ABORT, 'reject user session event'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RevokeSecuritySession(
		t.Context(), current.Token, actionReference, "Security Browser",
	); result != nil || err == nil {
		t.Fatalf("RevokeSecuritySession(audit failure) = %#v, %v", result, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), target.Token); err != nil || found == nil {
		t.Fatalf("target after audit rollback = %#v, %v", found, err)
	}
	var revokedAt sql.NullTime
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM sessions WHERE id = ?`, target.ID,
	).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid || events != 0 {
		t.Fatalf("audit rollback state = revokedAt:%#v events:%d", revokedAt, events)
	}
}
