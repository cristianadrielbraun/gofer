package auth

import (
	"errors"
	"testing"
	"time"
)

func TestAdministratorUserDeletionIsConfirmedResumableAndLocalOnly(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users
		SET status = 'disabled', disabled_at = ?, disabled_by = 'administrator'
		WHERE id = 'person';
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES ('person', 'private-password-hash');
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('person-google', 'person', 'google', 'https://accounts.google.com', 'private-subject');
		INSERT INTO accounts (id, user_id, provider, email_address, encrypted_password)
		VALUES ('person-mailbox', 'person', 'imap', 'private@example.com', x'01');
		INSERT INTO signatures (id, user_id, name, html_body)
		VALUES ('person-signature', 'person', 'Private', '<p>private signature</p>');
		INSERT INTO contact_profiles (id, user_id, display_name)
		VALUES ('person-contact', 'person', 'Private Contact')`, now,
	); err != nil {
		t.Fatal(err)
	}
	resetToken, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "person", CreatedBy: "administrator", ActorSessionID: adminSession.ID,
		Purpose: EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil {
		t.Fatal(err)
	}

	options := PrepareAdministratorUserDeletionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "person", Confirmation: "wrong",
	}
	if result, err := manager.PrepareAdministratorUserDeletion(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserDeletionConfirmationInvalid) {
		t.Fatalf("wrong confirmation = %#v, %v", result, err)
	}
	var pending, startedEvents int
	if err := manager.db.Read().QueryRow(`SELECT deletion_pending FROM users WHERE id = 'person'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventUserDeletionStarted).Scan(&startedEvents); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || startedEvents != 0 {
		t.Fatalf("rejected deletion changed state pending=%d events=%d", pending, startedEvents)
	}

	options.Confirmation = "person"
	prepared, err := manager.PrepareAdministratorUserDeletion(t.Context(), options)
	if err != nil || prepared == nil || prepared.Resumed || prepared.Username != "person" ||
		len(prepared.AccountIDs) != 1 || prepared.AccountIDs[0] != "person-mailbox" {
		t.Fatalf("PrepareAdministratorUserDeletion() = %#v, %v", prepared, err)
	}
	var accountDeleting int
	if err := manager.db.Read().QueryRow(`SELECT deletion_pending FROM users WHERE id = 'person'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT is_deleting FROM accounts WHERE id = 'person-mailbox'`).Scan(&accountDeleting); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || accountDeleting != 1 {
		t.Fatalf("prepared deletion state pending=%d accountDeleting=%d", pending, accountDeleting)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "person", CreatedBy: "administrator", ActorSessionID: adminSession.ID,
		Purpose: EnrollmentTokenPurposeCredentialReset,
	}); token != nil || !errors.Is(err, ErrEnrollmentTokenTargetInvalid) {
		t.Fatalf("credential reset during deletion = %#v, %v", token, err)
	}
	if changed, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "person", Required: true,
	}); changed != nil || !errors.Is(err, ErrAdministratorUserMFATargetInvalid) {
		t.Fatalf("MFA policy during deletion = %#v, %v", changed, err)
	}
	if recovered, err := manager.RecoverUserLocally(t.Context(), "person"); recovered != nil || !errors.Is(err, ErrLocalRecoveryTargetInvalid) {
		t.Fatalf("local recovery during deletion = %#v, %v", recovered, err)
	}
	if redeemed, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: resetToken.Token, NewPassword: "replacement password that remains private",
		Purpose: EnrollmentTokenPurposeCredentialReset,
	}); redeemed != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("credential redemption during deletion = %#v, %v", redeemed, err)
	}
	if err := manager.RequestPasswordReset(t.Context(), PasswordResetRequestOptions{
		Identifier: "person", Source: "198.51.100.20", UserAgent: "Deletion test",
	}); err != nil {
		t.Fatal(err)
	}
	var resetRequested int
	if err := manager.db.Read().QueryRow(`SELECT password_reset_requested_at IS NOT NULL FROM users WHERE id = 'person'`).Scan(&resetRequested); err != nil || resetRequested != 0 {
		t.Fatalf("password reset request during deletion = %d, %v", resetRequested, err)
	}
	listed, err := manager.ListPendingUserDeletionIDs(t.Context())
	if err != nil || len(listed) != 1 || listed[0] != "person" {
		t.Fatalf("ListPendingUserDeletionIDs() = %#v, %v", listed, err)
	}
	resumed, err := manager.PrepareAdministratorUserDeletion(t.Context(), options)
	if err != nil || resumed == nil || !resumed.Resumed {
		t.Fatalf("resumed deletion = %#v, %v", resumed, err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventUserDeletionStarted).Scan(&startedEvents); err != nil || startedEvents != 1 {
		t.Fatalf("deletion start events = %d, %v", startedEvents, err)
	}
	if deleted, err := manager.CompleteAdministratorUserDeletion(t.Context(), "person", adminSession.ID); deleted || !errors.Is(err, ErrAdministratorUserDeletionIncomplete) {
		t.Fatalf("completion with mailbox = %t, %v", deleted, err)
	}

	if _, err := manager.db.Write().Exec(`DELETE FROM accounts WHERE id = 'person-mailbox'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.CompleteAdministratorUserDeletion(t.Context(), "person", adminSession.ID)
	if err != nil || !deleted {
		t.Fatalf("CompleteAdministratorUserDeletion() = %t, %v", deleted, err)
	}
	if again, err := manager.CompleteAdministratorUserDeletion(t.Context(), "person", adminSession.ID); err != nil || again {
		t.Fatalf("idempotent completion = %t, %v", again, err)
	}
	for table, query := range map[string]string{
		"users":                `SELECT COUNT(*) FROM users WHERE id = 'person'`,
		"password credentials": `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`,
		"identities":           `SELECT COUNT(*) FROM auth_identities WHERE user_id = 'person'`,
		"signatures":           `SELECT COUNT(*) FROM signatures WHERE user_id = 'person'`,
		"contacts":             `SELECT COUNT(*) FROM contact_profiles WHERE user_id = 'person'`,
	} {
		var count int
		if err := manager.db.Read().QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("remaining %s = %d, %v", table, count, err)
		}
	}
	var deletedEvents int
	var startSubject, deletedSubject, startMetadata, deletedMetadata string
	if err := manager.db.Read().QueryRow(`
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventUserDeleted,
	).Scan(&deletedEvents); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`
		SELECT COALESCE(subject_user_id, ''), metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventUserDeletionStarted,
	).Scan(&startSubject, &startMetadata); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`
		SELECT COALESCE(subject_user_id, ''), metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventUserDeleted,
	).Scan(&deletedSubject, &deletedMetadata); err != nil {
		t.Fatal(err)
	}
	wantMetadata := `{"target_user_id":"person","target_username":"person","remote_provider_data_changed":false}`
	if deletedEvents != 1 || startSubject != "" || deletedSubject != "" ||
		startMetadata != wantMetadata || deletedMetadata != wantMetadata {
		t.Fatalf("retained deletion audit = events:%d startSubject:%q deletedSubject:%q start:%q deleted:%q",
			deletedEvents, startSubject, deletedSubject, startMetadata, deletedMetadata)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), adminSession.Token); err != nil || stored == nil {
		t.Fatalf("administrator session after target deletion = %#v, %v", stored, err)
	}
}

func TestAdministratorUserDeletionRejectsIneligibleTargets(t *testing.T) {
	now := time.Date(2026, time.August, 29, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertActiveUser(t, manager, "active-target", false, now)
	insertActiveUser(t, manager, "disabled-target", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	ordinarySession, err := manager.CreateAuthenticatedSession(
		t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'disabled' WHERE id = 'disabled-target';
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at)
		VALUES (1, 1, 'disabled-target', ?)`, now); err != nil {
		t.Fatal(err)
	}

	base := PrepareAdministratorUserDeletionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "active-target", Confirmation: "active-target",
	}
	if result, err := manager.PrepareAdministratorUserDeletion(t.Context(), base); result != nil || !errors.Is(err, ErrAdministratorUserDeletionTargetInvalid) {
		t.Fatalf("active target = %#v, %v", result, err)
	}
	base.TargetUserID, base.Confirmation = "administrator", "administrator"
	if result, err := manager.PrepareAdministratorUserDeletion(t.Context(), base); result != nil || !errors.Is(err, ErrAdministratorUserDeletionTargetInvalid) {
		t.Fatalf("self target = %#v, %v", result, err)
	}
	base.TargetUserID, base.Confirmation = "disabled-target", "disabled-target"
	if result, err := manager.PrepareAdministratorUserDeletion(t.Context(), base); result != nil || !errors.Is(err, ErrAdministratorUserDeletionTargetInvalid) {
		t.Fatalf("setup owner target = %#v, %v", result, err)
	}
	base.ActorUserID, base.ActorSessionID = "ordinary", ordinarySession.ID
	if result, err := manager.PrepareAdministratorUserDeletion(t.Context(), base); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary actor = %#v, %v", result, err)
	}
}

func TestAdministratorUserDeletionRollsBackWhenAuditRecordingFails(t *testing.T) {
	now := time.Date(2026, time.August, 29, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'disabled' WHERE id = 'person';
		INSERT INTO accounts (id, user_id, email_address) VALUES ('person-mailbox', 'person', 'private@example.com');
		CREATE TRIGGER reject_user_deletion_start
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'user_deletion_started'
		BEGIN SELECT RAISE(ABORT, 'reject deletion start'); END`); err != nil {
		t.Fatal(err)
	}

	result, err := manager.PrepareAdministratorUserDeletion(t.Context(), PrepareAdministratorUserDeletionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "person", Confirmation: "person",
	})
	if result != nil || err == nil {
		t.Fatalf("PrepareAdministratorUserDeletion(audit failure) = %#v, %v", result, err)
	}
	var pending, accountDeleting int
	if err := manager.db.Read().QueryRow(`SELECT deletion_pending FROM users WHERE id = 'person'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT is_deleting FROM accounts WHERE id = 'person-mailbox'`).Scan(&accountDeleting); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || accountDeleting != 0 {
		t.Fatalf("audit failure left deletion state pending=%d accountDeleting=%d", pending, accountDeleting)
	}
}
