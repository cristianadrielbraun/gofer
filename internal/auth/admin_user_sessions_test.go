package auth

import (
	"errors"
	"testing"
	"time"
)

func TestSignOutAdministratorUserEndsOnlyWorkingSessions(t *testing.T) {
	now := time.Date(2026, time.November, 10, 15, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	var working []*Session
	for _, agent := range []string{"Laptop browser", "Phone browser"} {
		session, err := manager.CreateAuthenticatedSession(t.Context(), "person", agent, AuthenticationMethodPassword, AssuranceLevelSingleFactor)
		if err != nil {
			t.Fatal(err)
		}
		working = append(working, session)
	}
	expired, err := manager.CreateAuthenticatedSession(t.Context(), "person", "Old browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET idle_expires_at = ? WHERE id = ?`, now.Add(-time.Minute), expired.ID); err != nil {
		t.Fatal(err)
	}

	options := SignOutAdministratorUserOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "person"}
	result, err := manager.SignOutAdministratorUser(t.Context(), options)
	if err != nil || result == nil || result.RevokedSessions != 2 {
		t.Fatalf("SignOutAdministratorUser() = %#v, %v", result, err)
	}
	for _, session := range working {
		if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored != nil {
			t.Fatalf("signed-out session %q = %#v, %v", session.ID, stored, err)
		}
	}
	var expiredRevoked int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at IS NOT NULL FROM sessions WHERE id = ?`, expired.ID).Scan(&expiredRevoked); err != nil || expiredRevoked != 0 {
		t.Fatalf("expired session revoked = %d, %v", expiredRevoked, err)
	}
	// Signing out changes nothing else: they can sign in again.
	user, err := manager.GetUserByID(t.Context(), "person")
	if err != nil || user == nil || user.Status != UserStatusActive || user.AuthVersion != 1 {
		t.Fatalf("signed-out person = %#v, %v", user, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), adminSession.Token); err != nil || stored == nil {
		t.Fatalf("administrator's own session = %#v, %v", stored, err)
	}

	var eventID, actor, subject, sessionID, reason, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT id, COALESCE(actor_user_id, ''), COALESCE(subject_user_id, ''), COALESCE(session_id, ''), reason, metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventSessionRevoked,
	).Scan(&eventID, &actor, &subject, &sessionID, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if actor != "administrator" || subject != "person" || sessionID != adminSession.ID ||
		reason != string(AuthEventReasonAdministratorAction) || metadata != `{"scope":"all","revoked_sessions":2}` {
		t.Fatalf("sign-out event = actor:%q subject:%q session:%q reason:%q metadata:%q", actor, subject, sessionID, reason, metadata)
	}
	records := auditRecords(t, manager)
	if len(records) != 1 || records[0].Action != "user.signed_out" || records[0].Target.ID != "person" ||
		records[0].Target.Label != "person" || records[0].CorrelationID != eventID ||
		records[0].Metadata["revoked_sessions"] != float64(2) {
		t.Fatalf("sign-out audit records = %+v", records)
	}

	// Someone signed in nowhere has nothing to end, and nothing is recorded.
	again, err := manager.SignOutAdministratorUser(t.Context(), options)
	if err != nil || again == nil || again.RevokedSessions != 0 {
		t.Fatalf("SignOutAdministratorUser(again) = %#v, %v", again, err)
	}
	if records := auditRecords(t, manager); len(records) != 1 {
		t.Fatalf("audit records after signing out no one = %+v", records)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSessionRevoked).Scan(&events); err != nil || events != 1 {
		t.Fatalf("sign-out events = %d, %v", events, err)
	}
}

func TestSignOutAdministratorUserRequiresVerifiedAdministratorAndEligibleTarget(t *testing.T) {
	now := time.Date(2026, time.November, 10, 15, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	for _, id := range []string{"away", "joining", "leaving"} {
		insertActiveUser(t, manager, id, false, now)
	}
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySession, err := manager.CreateAuthenticatedSession(t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE users SET status = 'disabled' WHERE id = 'away'`,
		`UPDATE users SET status = 'pending' WHERE id = 'joining'`,
		`UPDATE users SET status = 'disabled', deletion_pending = 1 WHERE id = 'leaving'`,
	} {
		if _, err := manager.db.Write().ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}

	options := SignOutAdministratorUserOptions{ActorUserID: "ordinary", ActorSessionID: ordinarySession.ID, TargetUserID: "away"}
	if result, err := manager.SignOutAdministratorUser(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary sign-out = %#v, %v", result, err)
	}
	options.ActorUserID, options.ActorSessionID = "administrator", staleAdmin.ID
	if result, err := manager.SignOutAdministratorUser(t.Context(), options); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale administrator sign-out = %#v, %v", result, err)
	}
	options.ActorSessionID = adminSession.ID
	for _, target := range []string{"administrator", "missing", "away", "joining", "leaving", ""} {
		options.TargetUserID = target
		if result, err := manager.SignOutAdministratorUser(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserSignOutTargetInvalid) {
			t.Fatalf("sign-out of %q = %#v, %v", target, result, err)
		}
	}
	if records := auditRecords(t, manager); len(records) != 0 {
		t.Fatalf("audit records after refused sign-outs = %+v", records)
	}
}

func TestSignOutAdministratorUserSessionEndsOnlyTheChosenOne(t *testing.T) {
	now := time.Date(2026, time.November, 12, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	otherAdminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	laptop, err := manager.CreateAuthenticatedSession(t.Context(), "person", "Laptop browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := manager.CreateAuthenticatedSession(t.Context(), "person", "Phone browser", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant)
	if err != nil {
		t.Fatal(err)
	}

	sessions, err := manager.ListAdministratorUserSessions(t.Context(), "administrator", adminSession.ID, "person")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("ListAdministratorUserSessions() = %+v, %v", sessions, err)
	}
	var laptopReference string
	for _, session := range sessions {
		if session.Reference == "" || session.Reference == laptop.ID || session.Reference == phone.ID {
			t.Fatalf("a session's reference is empty or its ID: %+v", session)
		}
		if session.UserAgent == "Laptop browser" {
			laptopReference = session.Reference
		}
	}
	// Another administrator session gets other references, which this one
	// can't use.
	elsewhere, err := manager.ListAdministratorUserSessions(t.Context(), "administrator", otherAdminSession.ID, "person")
	if err != nil || len(elsewhere) != 2 || elsewhere[0].Reference == sessions[0].Reference {
		t.Fatalf("references from another administrator session = %+v, %v", elsewhere, err)
	}
	options := SignOutAdministratorUserSessionOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "person"}
	for _, reference := range []string{"", "not-a-reference", elsewhere[0].Reference, elsewhere[1].Reference} {
		options.Reference = reference
		if result, err := manager.SignOutAdministratorUserSession(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserSessionGone) {
			t.Fatalf("sign out with reference %q = %+v, %v", reference, result, err)
		}
	}
	options.Reference = laptopReference
	stale := options
	stale.ActorSessionID = staleAdmin.ID
	if result, err := manager.SignOutAdministratorUserSession(t.Context(), stale); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale sign out = %+v, %v", result, err)
	}

	result, err := manager.SignOutAdministratorUserSession(t.Context(), options)
	if err != nil || result == nil || result.UserAgent != "Laptop browser" {
		t.Fatalf("SignOutAdministratorUserSession() = %+v, %v", result, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), laptop.Token); err != nil || stored != nil {
		t.Fatalf("signed-out laptop = %+v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), phone.Token); err != nil || stored == nil {
		t.Fatalf("the phone was signed out too: %+v, %v", stored, err)
	}
	if again, err := manager.SignOutAdministratorUserSession(t.Context(), options); again != nil || !errors.Is(err, ErrAdministratorUserSessionGone) {
		t.Fatalf("signing out an ended session = %+v, %v", again, err)
	}
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT metadata_json FROM auth_events WHERE event_type = ? AND subject_user_id = 'person'`, AuthEventSessionRevoked).Scan(&metadata); err != nil || metadata != `{"scope":"single","revoked_sessions":1}` {
		t.Fatalf("sign-out event = %q, %v", metadata, err)
	}
	records := auditRecords(t, manager)
	if len(records) != 1 || records[0].Action != "user.session_signed_out" || records[0].Target.ID != "person" || records[0].Metadata["user_agent"] != "Laptop browser" {
		t.Fatalf("audit records = %+v", records)
	}

	// Not oneself, and not someone who can't be signed out.
	own, err := manager.ListAdministratorUserSessions(t.Context(), "administrator", adminSession.ID, "administrator")
	if err != nil || len(own) == 0 {
		t.Fatalf("own sessions = %+v, %v", own, err)
	}
	self := options
	self.TargetUserID, self.Reference = "administrator", own[0].Reference
	if result, err := manager.SignOutAdministratorUserSession(t.Context(), self); result != nil || !errors.Is(err, ErrAdministratorUserSignOutTargetInvalid) {
		t.Fatalf("signing oneself out = %+v, %v", result, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'disabled' WHERE id = 'person'`); err != nil {
		t.Fatal(err)
	}
	options.Reference = sessions[1].Reference
	if result, err := manager.SignOutAdministratorUserSession(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserSignOutTargetInvalid) {
		t.Fatalf("signing someone disabled out = %+v, %v", result, err)
	}
}
