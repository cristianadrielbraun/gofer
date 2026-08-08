package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const enrollmentRedemptionTestPassword = "an excellent redeemed passphrase"

func enrollmentRedemptionStack(t *testing.T, status auth.UserStatus, purpose auth.EnrollmentTokenPurpose) (*auth.Manager, *storage.DB, http.Handler, *auth.EnrollmentToken) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
	}, db, auth.Dependencies{BucketHashKey: []byte("redemption-handler-key-32-bytes!")})
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name,
			status, auth_version, is_admin, created_at, updated_at
		) VALUES
			('admin', 'admin@example.com', 'admin@example.com', 'admin', 'admin', 'Admin', 'active', 1, 1, ?, ?),
			('person', 'person@example.com', 'person@example.com', 'person', 'person', 'Person', ?, 1, 0, ?, ?)`,
		now, now, status, now, now,
	); err != nil {
		t.Fatalf("insert redemption users: %v", err)
	}
	adminSession, err := manager.CreateAuthenticatedSession(t.Context(), "admin", "admin browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(admin) error = %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), "admin", adminSession.ID, auth.AuthenticationMethodPassword); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(admin) = %t, %v", steppedUp, err)
	}
	token, err := manager.IssueEnrollmentToken(t.Context(), auth.IssueEnrollmentTokenOptions{
		UserID: "person", CreatedBy: "admin", ActorSessionID: adminSession.ID, Purpose: purpose,
	})
	if err != nil {
		t.Fatalf("IssueEnrollmentToken() error = %v", err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), token
}

func postEnrollmentRedemption(stack http.Handler, token, password, confirmation string) *httptest.ResponseRecorder {
	form := url.Values{
		"token":            {token},
		"new_password":     {password},
		"confirm_password": {confirmation},
	}
	request := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Redemption Handler Test/1.0")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestEnrollmentRedemptionRoutesArePublicLocalAndNoStore(t *testing.T) {
	_, _, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	request := httptest.NewRequest(http.MethodGet, enrollmentRedemptionPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("redemption page = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{
		`action="/account/redeem"`, `name="token"`, `type="password"`,
		`autocomplete="one-time-code"`, `name="new_password"`,
		`name="confirm_password"`, `autocomplete="new-password"`,
		`minlength="15"`, "never included in the page URL",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("redemption page missing %q", required)
		}
	}
	if strings.Contains(body, token.Token) || strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") {
		t.Fatal("redemption page exposed a token or requested a remote font")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("redemption security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestEnrollmentRedemptionCompletesThroughPublicStackAndClearsAuthCookies(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	recorder := postEnrollmentRedemption(stack, token.Token, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != enrollmentRedemptionCompletePath {
		t.Fatalf("successful redemption = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	for _, cookieName := range []string{"gofer_session", "gofer_pre_auth"} {
		cookie := responseCookie(recorder, cookieName, false)
		if cookie == nil || cookie.MaxAge != -1 {
			t.Fatalf("cleared %s cookie = %#v", cookieName, cookie)
		}
	}
	var status auth.UserStatus
	var passwordHash string
	var used int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'person'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	matches, _, err := auth.VerifyPassword(passwordHash, enrollmentRedemptionTestPassword)
	if err != nil || !matches || status != auth.UserStatusActive || used != 1 {
		t.Fatalf("redeemed state = status:%q matches:%t used:%d error:%v", status, matches, used, err)
	}

	request := httptest.NewRequest(http.MethodGet, enrollmentRedemptionCompletePath, nil)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `role="status"`) || !strings.Contains(recorder.Body.String(), `href="/login"`) || strings.Contains(recorder.Body.String(), token.Token) {
		t.Fatalf("completion page = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestEnrollmentRedemptionFailuresDoNotRevealTokenStateOrEchoSecrets(t *testing.T) {
	type testCase struct {
		name  string
		alter func(*storage.DB, *auth.EnrollmentToken)
	}
	tests := []testCase{
		{name: "unknown"},
		{name: "expired", alter: func(db *storage.DB, token *auth.EnrollmentToken) {
			_, _ = db.Write().Exec(`UPDATE user_enrollment_tokens SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute), token.ID)
		}},
		{name: "revoked", alter: func(db *storage.DB, token *auth.EnrollmentToken) {
			_, _ = db.Write().Exec(`UPDATE user_enrollment_tokens SET revoked_at = ? WHERE id = ?`, time.Now().UTC(), token.ID)
		}},
	}
	var referenceBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
			if test.alter != nil {
				test.alter(db, token)
			}
			supplied := token.Token
			if test.name == "unknown" {
				supplied = "unknown-private-token"
			}
			recorder := postEnrollmentRedemption(stack, supplied, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), enrollmentRedemptionFailureMessage) {
				t.Fatalf("generic redemption failure = %d %q", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), supplied) || strings.Contains(recorder.Body.String(), enrollmentRedemptionTestPassword) {
				t.Fatal("redemption failure echoed submitted credentials")
			}
			if referenceBody == "" {
				referenceBody = recorder.Body.String()
			} else if recorder.Body.String() != referenceBody {
				t.Fatal("redemption failure body differs by token state")
			}
		})
	}
}

func TestEnrollmentRedemptionValidatesConfirmationPolicyAndFormSizeBeforeMutation(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	for _, test := range []struct {
		name         string
		password     string
		confirmation string
		message      string
	}{
		{name: "mismatch", password: enrollmentRedemptionTestPassword, confirmation: "a different confirmation passphrase", message: "fields do not match"},
		{name: "policy", password: "short", confirmation: "short", message: auth.ErrPasswordTooShort.Error()},
	} {
		recorder := postEnrollmentRedemption(stack, token.Token, test.password, test.confirmation)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.message) || strings.Contains(recorder.Body.String(), token.Token) || strings.Contains(recorder.Body.String(), test.password) {
			t.Fatalf("%s response = %d %q", test.name, recorder.Code, recorder.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(strings.Repeat("x", enrollmentRedemptionFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), enrollmentRedemptionFailureMessage) {
		t.Fatalf("oversized redemption form = %d %q", recorder.Code, recorder.Body.String())
	}

	var used, credentials int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if used != 0 || credentials != 0 {
		t.Fatalf("rejected form mutation = used:%d credentials:%d", used, credentials)
	}
}

func TestEnrollmentRedemptionPostRemainsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatalf("httpguard.LoadConfig() error = %v", err)
	}
	form := url.Values{
		"token":            {token.Token},
		"new_password":     {enrollmentRedemptionTestPassword},
		"confirm_password": {enrollmentRedemptionTestPassword},
	}
	request := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin redemption = %d %q", recorder.Code, recorder.Body.String())
	}
	var used, credentials int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if used != 0 || credentials != 0 {
		t.Fatalf("cross-origin redemption mutation = used:%d credentials:%d", used, credentials)
	}
}

func TestEnrollmentRedemptionAcceptsPrivacyBrowserNullOriginWithSameOriginMetadata(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatalf("httpguard.LoadConfig() error = %v", err)
	}
	form := url.Values{
		"token":            {token.Token},
		"new_password":     {enrollmentRedemptionTestPassword},
		"confirm_password": {enrollmentRedemptionTestPassword},
	}
	request := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != enrollmentRedemptionCompletePath {
		t.Fatalf("null-origin same-origin redemption = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	var used int
	var passwordHash string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID,
	).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash FROM password_credentials WHERE user_id = 'person'`,
	).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	matches, _, err := auth.VerifyPassword(passwordHash, enrollmentRedemptionTestPassword)
	if err != nil || used != 1 || !matches {
		t.Fatalf("null-origin redemption state = used:%d matches:%t error:%v", used, matches, err)
	}
}
