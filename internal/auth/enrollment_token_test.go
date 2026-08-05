package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func insertEnrollmentTokenUser(t *testing.T, manager *Manager, id string, status UserStatus, isAdmin bool, now time.Time) {
	t.Helper()
	admin := 0
	if isAdmin {
		admin = 1
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, name, status, auth_version, is_admin,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		id, id+"@example.com", id+"@example.com", id, status, admin, now, now,
	); err != nil {
		t.Fatalf("insert enrollment token user %q: %v", id, err)
	}
}

func insertEnrollmentStepUpSession(t *testing.T, manager *Manager, userID string, stepUpAt, now time.Time) string {
	t.Helper()
	sessionID := userID + "-step-up-session"
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, user_agent, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, 1, 'password', 'single_factor', 'admin browser', ?, ?, ?, ?, ?, 'password', ?)`,
		sessionID, userID, hashToken(sessionID+"-raw"), now, now,
		now.Add(time.Hour), now.Add(24*time.Hour), stepUpAt, now,
	); err != nil {
		t.Fatalf("insert enrollment step-up session for %q: %v", userID, err)
	}
	return sessionID
}

func TestIssueEnrollmentTokenStoresOnlyHashAndReplacesActivePurpose(t *testing.T) {
	now := time.Date(2026, time.August, 5, 22, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"token-one-id", "event-one-id", "token-two-id", "event-two-id"},
		tokens: []string{"raw-token-one", "raw-token-two"},
	})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)

	first, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatalf("IssueEnrollmentToken(first) error = %v", err)
	}
	if first.Token != "raw-token-one" || first.ExpiresAt.Sub(first.CreatedAt) != defaultEnrollmentTokenLifetime {
		t.Fatalf("first enrollment token = %#v", first)
	}
	var storedHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash FROM user_enrollment_tokens WHERE id = ?`, first.ID,
	).Scan(&storedHash); err != nil {
		t.Fatalf("query stored enrollment token: %v", err)
	}
	if storedHash != hashToken(first.Token) || storedHash == first.Token {
		t.Fatalf("stored enrollment token hash = %q", storedHash)
	}

	clock.now = now.Add(5 * time.Minute)
	second, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatalf("IssueEnrollmentToken(second) error = %v", err)
	}
	if second.Token != "raw-token-two" || second.ID == first.ID {
		t.Fatalf("second enrollment token = %#v", second)
	}
	var active, revoked int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT
			SUM(CASE WHEN used_at IS NULL AND revoked_at IS NULL AND expires_at > ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN revoked_at IS NOT NULL THEN 1 ELSE 0 END)
		FROM user_enrollment_tokens WHERE user_id = 'pending' AND purpose = 'enrollment'`,
		clock.now,
	).Scan(&active, &revoked); err != nil {
		t.Fatalf("count rotated enrollment tokens: %v", err)
	}
	if active != 1 || revoked != 1 {
		t.Fatalf("rotated enrollment tokens = active:%d revoked:%d", active, revoked)
	}

	listed, err := manager.ListEnrollmentTokens(t.Context(), "admin", "pending")
	if err != nil {
		t.Fatalf("ListEnrollmentTokens() error = %v", err)
	}
	if len(listed) != 2 || listed[0].ID != second.ID || listed[0].Token != "" || listed[1].ID != first.ID || listed[1].Token != "" || listed[1].RevokedAt == nil {
		t.Fatalf("listed enrollment token metadata = %#v", listed)
	}

	rows, err := manager.db.Read().QueryContext(t.Context(), `SELECT event_type, metadata_json FROM auth_events ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("query enrollment issuance events: %v", err)
	}
	defer rows.Close()
	var eventCount int
	for rows.Next() {
		var eventType, metadata string
		if err := rows.Scan(&eventType, &metadata); err != nil {
			t.Fatal(err)
		}
		if eventType != string(AuthEventEnrollmentIssued) || strings.Contains(metadata, first.Token) || strings.Contains(metadata, second.Token) || strings.Contains(metadata, storedHash) {
			t.Fatalf("enrollment issuance event = type:%q metadata:%q", eventType, metadata)
		}
		eventCount++
	}
	if err := rows.Err(); err != nil || eventCount != 2 {
		t.Fatalf("enrollment issuance event count = %d, %v", eventCount, err)
	}
}

func TestIssueEnrollmentTokenEnforcesAdministratorPurposeAndLifetime(t *testing.T) {
	now := time.Date(2026, time.August, 5, 22, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{
			"non-admin-token", "non-admin-event",
			"active-enrollment-token", "active-enrollment-event",
			"pending-reset-token", "pending-reset-event",
			"disabled-reset-token", "disabled-reset-event",
			"stale-step-up-token", "stale-step-up-event",
		},
		tokens: []string{"non-admin-raw", "active-enrollment-raw", "pending-reset-raw", "disabled-reset-raw", "stale-step-up-raw"},
	})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "ordinary", UserStatusActive, false, now)
	insertEnrollmentTokenUser(t, manager, "active", UserStatusActive, false, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	insertEnrollmentTokenUser(t, manager, "disabled", UserStatusDisabled, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)
	ordinarySessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)

	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "ordinary", ActorSessionID: ordinarySessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	}); token != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("non-admin issuance = %#v, %v", token, err)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "active", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	}); token != nil || !errors.Is(err, ErrEnrollmentTokenTargetInvalid) {
		t.Fatalf("active enrollment issuance = %#v, %v", token, err)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeCredentialReset,
	}); token != nil || !errors.Is(err, ErrEnrollmentTokenTargetInvalid) {
		t.Fatalf("pending reset issuance = %#v, %v", token, err)
	}
	disabledReset, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "disabled", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil || disabledReset == nil || disabledReset.ExpiresAt.Sub(disabledReset.CreatedAt) != defaultCredentialResetTokenLifetime {
		t.Fatalf("disabled reset issuance = %#v, %v", disabledReset, err)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", Purpose: EnrollmentTokenPurposeEnrollment,
	}); token != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("missing-step-up issuance = %#v, %v", token, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-administratorStepUpMaximumAge-time.Second), adminSessionID); err != nil {
		t.Fatal(err)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	}); token != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale-step-up issuance = %#v, %v", token, err)
	}

	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurpose("unknown"),
	}); token != nil || err == nil {
		t.Fatalf("invalid-purpose issuance = %#v, %v", token, err)
	}
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
		Lifetime: maximumEnrollmentTokenLifetime + time.Minute,
	}); token != nil || err == nil {
		t.Fatalf("excessive-lifetime issuance = %#v, %v", token, err)
	}
	if tokens, err := manager.ListEnrollmentTokens(t.Context(), "ordinary", "pending"); tokens != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("non-admin token listing = %#v, %v", tokens, err)
	}
}

func TestEnrollmentTokenGenerationAndEventFailuresPreserveActiveToken(t *testing.T) {
	now := time.Date(2026, time.August, 5, 23, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"original-token", "original-event"},
		tokens: []string{"original-raw"},
	})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)
	original, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatal(err)
	}

	manager.tokens = &deterministicTokenGenerator{
		ids: []string{"failed-token"}, tokenErr: errors.New("random token failure"),
	}
	if replacement, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	}); replacement != nil || err == nil {
		t.Fatalf("token-generation failure = %#v, %v", replacement, err)
	}
	var originalRevokedAt any
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at FROM user_enrollment_tokens WHERE id = ?`, original.ID).Scan(&originalRevokedAt); err != nil {
		t.Fatal(err)
	}
	if originalRevokedAt != nil {
		t.Fatalf("original token revoked after random failure: %#v", originalRevokedAt)
	}

	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_enrollment_event
		BEFORE INSERT ON auth_events
		BEGIN
			SELECT RAISE(ABORT, 'forced enrollment event failure');
		END`); err != nil {
		t.Fatal(err)
	}
	manager.tokens = &deterministicTokenGenerator{
		ids:    []string{"replacement-token", "replacement-event"},
		tokens: []string{"replacement-raw"},
	}
	if replacement, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	}); replacement != nil || err == nil {
		t.Fatalf("event-write failure = %#v, %v", replacement, err)
	}
	var total, active int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), SUM(CASE WHEN revoked_at IS NULL AND used_at IS NULL THEN 1 ELSE 0 END)
		FROM user_enrollment_tokens WHERE user_id = 'pending'`,
	).Scan(&total, &active); err != nil {
		t.Fatal(err)
	}
	if total != 1 || active != 1 {
		t.Fatalf("tokens after issuance rollback = total:%d active:%d", total, active)
	}
}

func TestRevokeEnrollmentTokenIsAuthorizedScopedAndAudited(t *testing.T) {
	now := time.Date(2026, time.August, 5, 23, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"token-id", "issue-event", "foreign-revoke-event", "revoke-event", "repeat-revoke-event", "non-admin-event"},
		tokens: []string{"raw-token"},
	})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "ordinary", UserStatusActive, false, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	insertEnrollmentTokenUser(t, manager, "other", UserStatusPending, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)
	ordinarySessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)
	token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoked, err := manager.RevokeEnrollmentToken(t.Context(), "admin", adminSessionID, "other", token.ID); err != nil || revoked {
		t.Fatalf("foreign-target revocation = %t, %v", revoked, err)
	}
	if revoked, err := manager.RevokeEnrollmentToken(t.Context(), "admin", adminSessionID, "pending", token.ID); err != nil || !revoked {
		t.Fatalf("owned revocation = %t, %v", revoked, err)
	}
	if revoked, err := manager.RevokeEnrollmentToken(t.Context(), "admin", adminSessionID, "pending", token.ID); err != nil || revoked {
		t.Fatalf("repeat revocation = %t, %v", revoked, err)
	}
	if revoked, err := manager.RevokeEnrollmentToken(t.Context(), "ordinary", ordinarySessionID, "pending", token.ID); revoked || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("non-admin revocation = %t, %v", revoked, err)
	}

	var revokedAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&revokedAt); err != nil || !revokedAt.Equal(now) {
		t.Fatalf("stored revocation = %v, %v", revokedAt, err)
	}
	var revokeEvents int
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), COALESCE(MAX(metadata_json), '{}') FROM auth_events
		WHERE event_type = ?`, AuthEventEnrollmentRevoked,
	).Scan(&revokeEvents, &metadata); err != nil {
		t.Fatal(err)
	}
	if revokeEvents != 1 || strings.Contains(metadata, token.Token) || !strings.Contains(metadata, token.ID) {
		t.Fatalf("revocation events = count:%d metadata:%q", revokeEvents, metadata)
	}
}

func TestConcurrentEnrollmentTokenRotationLeavesOneActiveToken(t *testing.T) {
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	const attempts = 12
	ids := make([]string, 0, attempts*2)
	rawTokens := make([]string, 0, attempts)
	for index := range attempts {
		ids = append(ids, fmt.Sprintf("token-%02d", index), fmt.Sprintf("event-%02d", index))
		rawTokens = append(rawTokens, fmt.Sprintf("raw-%02d", index))
	}
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: ids, tokens: rawTokens})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)

	var wait sync.WaitGroup
	errorsByAttempt := make(chan error, attempts)
	issuedTokens := make(chan string, attempts)
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
				UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
			})
			if err != nil {
				errorsByAttempt <- err
				return
			}
			issuedTokens <- token.Token
		}()
	}
	wait.Wait()
	close(errorsByAttempt)
	close(issuedTokens)
	for err := range errorsByAttempt {
		t.Fatalf("concurrent IssueEnrollmentToken() error = %v", err)
	}
	seen := map[string]bool{}
	for token := range issuedTokens {
		if token == "" || seen[token] {
			t.Fatalf("duplicate concurrent raw token %q", token)
		}
		seen[token] = true
	}
	if len(seen) != attempts {
		t.Fatalf("concurrent issued token count = %d, want %d", len(seen), attempts)
	}
	var total, active, revoked, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*),
		       SUM(CASE WHEN used_at IS NULL AND revoked_at IS NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN revoked_at IS NOT NULL THEN 1 ELSE 0 END)
		FROM user_enrollment_tokens WHERE user_id = 'pending'`,
	).Scan(&total, &active, &revoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventEnrollmentIssued).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if total != attempts || active != 1 || revoked != attempts-1 || events != attempts {
		t.Fatalf("concurrent token state = total:%d active:%d revoked:%d events:%d", total, active, revoked, events)
	}
}

func TestCleanupEnrollmentTokensIsBoundedAndPreservesActiveRows(t *testing.T) {
	now := time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	tx, err := manager.db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < enrollmentTokenCleanupBatchSize+1; index++ {
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, 'pending', 'admin', ?, 'enrollment', ?, ?)`,
			fmt.Sprintf("expired-%03d", index), fmt.Sprintf("expired-hash-%03d", index),
			now.Add(-2*time.Hour), now.Add(-time.Hour),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), `
		INSERT INTO user_enrollment_tokens (
			id, user_id, created_by, token_hash, purpose, created_at, expires_at
		) VALUES ('active', 'pending', 'admin', 'active-hash', 'enrollment', ?, ?)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `
		INSERT INTO user_enrollment_tokens (
			id, user_id, created_by, token_hash, purpose, created_at, expires_at, revoked_at
		) VALUES ('recent-revoked', 'pending', 'admin', 'recent-revoked-hash', 'enrollment', ?, ?, ?)`,
		now.Add(-2*time.Hour), now.Add(-time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := manager.CleanupEnrollmentTokens(t.Context()); err != nil {
		t.Fatal(err)
	}
	var expired, active, recentRevoked int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE expires_at <= ?`, now).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = 'active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = 'recent-revoked'`).Scan(&recentRevoked); err != nil {
		t.Fatal(err)
	}
	if expired != 2 || active != 1 || recentRevoked != 1 {
		t.Fatalf("first cleanup = expired:%d active:%d recent-revoked:%d", expired, active, recentRevoked)
	}
	if err := manager.CleanupEnrollmentTokens(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM user_enrollment_tokens
		WHERE used_at IS NULL AND revoked_at IS NULL AND expires_at <= ?`, now).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("second cleanup active-expired count = %d, %v", expired, err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = 'recent-revoked'`).Scan(&recentRevoked); err != nil || recentRevoked != 1 {
		t.Fatalf("recent revoked token retention = %d, %v", recentRevoked, err)
	}
}

func TestEnrollmentTokenMetadataPersistsAcrossDatabaseRestart(t *testing.T) {
	now := time.Date(2026, time.August, 6, 2, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(&Config{Enabled: true}, db, Dependencies{
		Clock: &fixedClock{now: now},
		Tokens: &deterministicTokenGenerator{
			ids: []string{"persistent-token", "persistent-event"}, tokens: []string{"persistent-raw"},
		},
	})
	insertEnrollmentTokenUser(t, manager, "admin", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "admin", now, now)
	issued, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "admin", ActorSessionID: adminSessionID, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := NewManager(&Config{Enabled: true}, reopened, Dependencies{Clock: &fixedClock{now: now.Add(time.Minute)}})
	listed, err := restarted.ListEnrollmentTokens(t.Context(), "admin", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != issued.ID || listed[0].Token != "" || listed[0].Purpose != EnrollmentTokenPurposeEnrollment {
		t.Fatalf("restarted enrollment token metadata = %#v", listed)
	}
	var persistedHash string
	if err := reopened.Read().QueryRowContext(t.Context(), `SELECT token_hash FROM user_enrollment_tokens WHERE id = ?`, issued.ID).Scan(&persistedHash); err != nil {
		t.Fatal(err)
	}
	if persistedHash != hashToken(issued.Token) || persistedHash == issued.Token {
		t.Fatalf("persisted enrollment token hash = %q", persistedHash)
	}
}
