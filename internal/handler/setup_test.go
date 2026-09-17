package handler

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func setupEntryStack(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, string) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
	}, db, auth.Dependencies{BucketHashKey: []byte("setup-handler-test-key-32-bytes!")})
	provision, err := manager.EnsureSetupToken(t.Context(), "")
	if err != nil || provision == nil || provision.Token == "" {
		t.Fatalf("EnsureSetupToken() = %#v, %v", provision, err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), provision.Token
}

func postSetup(stack http.Handler, token string) *httptest.ResponseRecorder {
	form := url.Values{"token": {token}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Setup Handler Test/1.0")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSetupOwner(stack http.Handler, cookie *http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, setupOwnerPath, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSetupPassword(stack http.Handler, cookie *http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, setupPasswordPath, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSetupMFA(stack http.Handler, cookie *http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, setupMFAPath, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSetupRecovery(stack http.Handler, cookie *http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, setupRecoveryPath, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSetupReview(stack http.Handler, cookie *http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, setupReviewPath, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Setup Completion Browser/1.0")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func prepareHandlerVerifiedMFA(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, *http.Cookie) {
	t.Helper()
	manager, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	if setupCookie == nil {
		t.Fatal("setup entry did not return pre-authentication cookie")
	}
	if recorder := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	}); recorder.Code != http.StatusSeeOther {
		t.Fatalf("owner prerequisite = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder := postSetupPassword(stack, setupCookie, url.Values{
		"password": {"correct horse battery staple for owner"}, "password_confirmation": {"correct horse battery staple for owner"},
	}); recorder.Code != http.StatusSeeOther {
		t.Fatalf("password prerequisite = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder := postSetupMFA(stack, setupCookie, url.Values{"action": {"start"}}); recorder.Code != http.StatusSeeOther {
		t.Fatalf("MFA start prerequisite = %d %q", recorder.Code, recorder.Body.String())
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil {
		t.Fatalf("MFA prerequisite state = %#v, %v", state, err)
	}
	if recorder := postSetupMFA(stack, setupCookie, url.Values{
		"action": {"confirm"}, "code": {setupHandlerTOTPCode(t, state.Draft.TOTPSecret)},
	}); recorder.Code != http.StatusSeeOther {
		t.Fatalf("MFA confirmation prerequisite = %d %q", recorder.Code, recorder.Body.String())
	}
	return manager, db, stack, setupCookie
}

func prepareHandlerSetupReview(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, *http.Cookie) {
	t.Helper()
	manager, db, stack, setupCookie := prepareHandlerVerifiedMFA(t)
	generated := postSetupRecovery(stack, setupCookie, url.Values{"action": {"generate"}})
	if generated.Code != http.StatusOK {
		t.Fatalf("recovery generation prerequisite = %d %q", generated.Code, generated.Body.String())
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil || state.Draft.RecoveryBatchID == "" {
		t.Fatalf("recovery prerequisite state = %#v, %v", state, err)
	}
	acknowledged := postSetupRecovery(stack, setupCookie, url.Values{
		"action": {"acknowledge"}, "batch_id": {state.Draft.RecoveryBatchID}, "saved": {"yes"},
	})
	if acknowledged.Code != http.StatusSeeOther {
		t.Fatalf("recovery acknowledgement prerequisite = %d %q", acknowledged.Code, acknowledged.Body.String())
	}
	return manager, db, stack, setupCookie
}

func setupHandlerTOTPCode(t *testing.T, secret string) string {
	return setupHandlerTOTPCodeAt(t, secret, time.Now().UTC())
}

func setupHandlerTOTPCodeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: 30, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestSetupEntryIsPublicLocalNoStoreAndSecretFree(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodGet, setupPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("setup entry = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{
		`action="/setup"`, `name="token"`, `type="password"`,
		`autocomplete="one-time-code"`, "never included in the page URL", "browser storage",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("setup entry missing %q", required)
		}
	}
	if strings.Contains(body, setupToken) || strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") {
		t.Fatal("setup entry exposed a token or requested a remote font")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("setup entry security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestSetupTokenVerificationCreatesProtectedContinuationWithoutCompletingSetup(t *testing.T) {
	manager, db, stack, setupToken := setupEntryStack(t)
	recorder := postSetup(stack, setupToken)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("successful setup verification = %d location:%q body:%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	setupCookie := responseCookie(recorder, "gofer_pre_auth", true)
	if setupCookie == nil || setupCookie.Value == "" || setupCookie.Value == setupToken || !setupCookie.HttpOnly || !setupCookie.Secure || setupCookie.SameSite != http.SameSiteLaxMode || setupCookie.MaxAge != 600 || setupCookie.Path != "/" {
		t.Fatalf("setup access cookie = %#v", setupCookie)
	}
	for _, cookieName := range []string{"gofer_session", "gofer_auth_return_to"} {
		cleared := responseCookie(recorder, cookieName, false)
		if cleared == nil || cleared.MaxAge != -1 {
			t.Fatalf("setup verification did not clear %s: %#v", cookieName, cleared)
		}
	}
	if strings.Contains(recorder.Body.String(), setupToken) || strings.Contains(recorder.Body.String(), setupCookie.Value) {
		t.Fatal("setup verification response exposed a bearer secret")
	}

	var initialized int
	var setupHash, challengeHash string
	var attempts int64
	if err := db.Read().QueryRow(`
		SELECT initialized, setup_token_hash, setup_attempts
		FROM auth_system_state WHERE id = 1`,
	).Scan(&initialized, &setupHash, &attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT challenge_hash FROM auth_challenges
		WHERE purpose = 'enrollment' AND user_id IS NULL AND consumed_at IS NULL`,
	).Scan(&challengeHash); err != nil {
		t.Fatal(err)
	}
	wantSetupHash := sha256.Sum256([]byte(setupToken))
	wantChallengeHash := sha256.Sum256([]byte(setupCookie.Value))
	if initialized != 0 || setupHash != fmt.Sprintf("%x", wantSetupHash) || attempts != 0 ||
		challengeHash != fmt.Sprintf("%x", wantChallengeHash) || challengeHash == setupCookie.Value {
		t.Fatalf("setup verification persistence = initialized:%d setupHash:%q attempts:%d challengeHash:%q", initialized, setupHash, attempts, challengeHash)
	}
	active, err := manager.GetActiveSetupAccess(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || active == nil {
		t.Fatalf("GetActiveSetupAccess() = %#v, %v", active, err)
	}
	request := httptest.NewRequest(http.MethodGet, setupPath, nil)
	request.AddCookie(setupCookie)
	alreadyVerified := httptest.NewRecorder()
	stack.ServeHTTP(alreadyVerified, request)
	if alreadyVerified.Code != http.StatusSeeOther || alreadyVerified.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("verified setup entry = %d location:%q", alreadyVerified.Code, alreadyVerified.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	request.AddCookie(setupCookie)
	ownerRecorder := httptest.NewRecorder()
	stack.ServeHTTP(ownerRecorder, request)
	if ownerRecorder.Code != http.StatusOK || !strings.Contains(ownerRecorder.Body.String(), "Setup access verified") || !strings.Contains(ownerRecorder.Body.String(), "Create the management owner") || !strings.Contains(ownerRecorder.Body.String(), `action="/setup/owner"`) || strings.Contains(ownerRecorder.Body.String(), setupToken) || strings.Contains(ownerRecorder.Body.String(), setupCookie.Value) {
		t.Fatalf("protected setup owner page = %d %q", ownerRecorder.Code, ownerRecorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	withoutCookie := httptest.NewRecorder()
	stack.ServeHTTP(withoutCookie, request)
	if withoutCookie.Code != http.StatusSeeOther || withoutCookie.Header().Get("Location") != setupPath {
		t.Fatalf("unverified setup owner request = %d location:%q", withoutCookie.Code, withoutCookie.Header().Get("Location"))
	}
	cleared := responseCookie(withoutCookie, "gofer_pre_auth", false)
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("unverified setup owner request did not clear stale cookie: %#v", cleared)
	}
}

func TestFreshSetupOwnerDraftIsProtectedEncryptedAndNonMutating(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	if setupCookie == nil {
		t.Fatal("setup verification did not issue continuation cookie")
	}

	saved := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Cristian Braun"},
		"username": {"Cristian.B"},
	})
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("fresh owner save = %d location:%q body:%q", saved.Code, saved.Header().Get("Location"), saved.Body.String())
	}
	var payload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 || bytes.Contains(payload, []byte("Cristian")) || bytes.Contains(payload, []byte("cristian@example.com")) {
		t.Fatalf("fresh owner payload is missing or plaintext: %q", payload)
	}
	var users, credentials, initialized int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM password_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT initialized FROM auth_system_state WHERE id = 1`).Scan(&initialized); err != nil {
		t.Fatal(err)
	}
	if users != 0 || credentials != 0 || initialized != 0 {
		t.Fatalf("owner draft partially initialized = users:%d credentials:%d initialized:%d", users, credentials, initialized)
	}

	request := httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	request.AddCookie(setupCookie)
	review := httptest.NewRecorder()
	stack.ServeHTTP(review, request)
	for _, want := range []string{"Management owner profile ready", "Cristian Braun", "Cristian.B", "No user, role, credential, or mailbox ownership has changed yet"} {
		if review.Code != http.StatusOK || !strings.Contains(review.Body.String(), want) {
			t.Fatalf("fresh owner review missing %q: %d %q", want, review.Code, review.Body.String())
		}
	}
	if strings.Contains(review.Body.String(), setupToken) || strings.Contains(review.Body.String(), setupCookie.Value) {
		t.Fatal("owner review exposed setup bearer material")
	}
}

func TestSetupPasswordRequiresCurrentOwnerDraftAndNeverEchoesSecrets(t *testing.T) {
	manager, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	if setupCookie == nil {
		t.Fatal("setup verification did not issue continuation cookie")
	}

	request := httptest.NewRequest(http.MethodGet, setupPasswordPath, nil)
	request.AddCookie(setupCookie)
	missingDraft := httptest.NewRecorder()
	stack.ServeHTTP(missingDraft, request)
	if missingDraft.Code != http.StatusSeeOther || missingDraft.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("password without owner draft = %d location:%q", missingDraft.Code, missingDraft.Header().Get("Location"))
	}
	request = httptest.NewRequest(http.MethodPost, setupPasswordPath, strings.NewReader(strings.Repeat("x", setupFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.AddCookie(setupCookie)
	oversizedWithoutDraft := httptest.NewRecorder()
	stack.ServeHTTP(oversizedWithoutDraft, request)
	if oversizedWithoutDraft.Code != http.StatusSeeOther || oversizedWithoutDraft.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("oversized password without owner draft = %d location:%q", oversizedWithoutDraft.Code, oversizedWithoutDraft.Header().Get("Location"))
	}

	owner := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Cristian Braun"},
		"username": {"cristian"},
	})
	if owner.Code != http.StatusSeeOther || owner.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("owner continuation = %d location:%q", owner.Code, owner.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, setupPasswordPath, nil)
	request.AddCookie(setupCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	for _, want := range []string{
		"Choose the owner password", `action="/setup/password"`, `autocomplete="new-password"`,
		`minlength="15"`, "administrator MFA and recovery codes",
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("password page missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}
	request = httptest.NewRequest(http.MethodGet, setupPasswordPath+"?password=must-not-survive", nil)
	request.AddCookie(setupCookie)
	query := httptest.NewRecorder()
	stack.ServeHTTP(query, request)
	if query.Code != http.StatusSeeOther || query.Header().Get("Location") != setupPasswordPath || strings.Contains(query.Body.String(), "must-not-survive") {
		t.Fatalf("password query stripping = %d location:%q body:%q", query.Code, query.Header().Get("Location"), query.Body.String())
	}

	var ownerPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&ownerPayload); err != nil {
		t.Fatal(err)
	}
	const mismatchedPassword = "correct horse battery staple for owner"
	mismatch := postSetupPassword(stack, setupCookie, url.Values{
		"password": {mismatchedPassword}, "password_confirmation": {"a different secure owner passphrase"},
	})
	if mismatch.Code != http.StatusUnprocessableEntity || !strings.Contains(mismatch.Body.String(), "does not match") || strings.Contains(mismatch.Body.String(), mismatchedPassword) {
		t.Fatalf("password mismatch = %d %q", mismatch.Code, mismatch.Body.String())
	}

	const weakPassword = "passwordpassword"
	weak := postSetupPassword(stack, setupCookie, url.Values{
		"password": {weakPassword}, "password_confirmation": {weakPassword},
	})
	if weak.Code != http.StatusUnprocessableEntity || !strings.Contains(weak.Body.String(), auth.ErrPasswordCommon.Error()) || strings.Contains(weak.Body.String(), `value="`+weakPassword+`"`) {
		t.Fatalf("weak password = %d %q", weak.Code, weak.Body.String())
	}
	var rejectedPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&rejectedPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rejectedPayload, ownerPayload) {
		t.Fatal("rejected password replaced the owner draft")
	}

	const password = "correct horse battery staple for owner"
	saved := postSetupPassword(stack, setupCookie, url.Values{
		"password": {password}, "password_confirmation": {password},
	})
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != setupPasswordPath || strings.Contains(saved.Body.String(), password) {
		t.Fatalf("password save = %d location:%q body:%q", saved.Code, saved.Header().Get("Location"), saved.Body.String())
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || !state.PasswordReady || state.Draft == nil || state.Draft.PasswordHash == "" {
		t.Fatalf("saved password state = %#v, %v", state, err)
	}

	request = httptest.NewRequest(http.MethodGet, setupPasswordPath, nil)
	request.AddCookie(setupCookie)
	review := httptest.NewRecorder()
	stack.ServeHTTP(review, request)
	if review.Code != http.StatusOK || !strings.Contains(review.Body.String(), "Owner password ready for final enrollment") ||
		strings.Contains(review.Body.String(), password) || strings.Contains(review.Body.String(), state.Draft.PasswordHash) {
		t.Fatalf("prepared password review = %d %q", review.Code, review.Body.String())
	}

	var payload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(password)) || bytes.Contains(payload, []byte(state.Draft.PasswordHash)) {
		t.Fatal("stored setup payload exposed password material")
	}
	var users, credentials, sessions, initialized int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &credentials,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 0 || credentials != 0 || sessions != 0 || initialized != 0 {
		t.Fatalf("password setup partially enrolled = users:%d credentials:%d sessions:%d initialized:%d", users, credentials, sessions, initialized)
	}
}

func TestSetupPasswordPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	owner := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	})
	if owner.Code != http.StatusSeeOther {
		t.Fatalf("owner prerequisite = %d %q", owner.Code, owner.Body.String())
	}
	var originalPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple for owner"
	form := url.Values{"password": {password}, "password_confirmation": {password}}
	request := httptest.NewRequest(http.MethodPost, setupPasswordPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(setupCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") || strings.Contains(recorder.Body.String(), password) {
		t.Fatalf("cross-origin password submission = %d %q", recorder.Code, recorder.Body.String())
	}
	var storedPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedPayload, originalPayload) {
		t.Fatal("cross-origin password submission replaced the owner draft")
	}
}

func TestSetupMFARequiresPreparedPasswordAndExplicitStart(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)

	request := httptest.NewRequest(http.MethodGet, setupMFAPath, nil)
	request.AddCookie(setupCookie)
	withoutOwner := httptest.NewRecorder()
	stack.ServeHTTP(withoutOwner, request)
	if withoutOwner.Code != http.StatusSeeOther || withoutOwner.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("MFA without owner/password = %d location:%q", withoutOwner.Code, withoutOwner.Header().Get("Location"))
	}

	owner := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	})
	if owner.Code != http.StatusSeeOther {
		t.Fatalf("owner prerequisite = %d %q", owner.Code, owner.Body.String())
	}
	password := postSetupPassword(stack, setupCookie, url.Values{
		"password":              {"correct horse battery staple for owner"},
		"password_confirmation": {"correct horse battery staple for owner"},
	})
	if password.Code != http.StatusSeeOther {
		t.Fatalf("password prerequisite = %d %q", password.Code, password.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, setupPasswordPath, nil)
	request.AddCookie(setupCookie)
	passwordPage := httptest.NewRecorder()
	stack.ServeHTTP(passwordPage, request)
	if passwordPage.Code != http.StatusOK || !strings.Contains(passwordPage.Body.String(), `action="/setup/mfa"`) || !strings.Contains(passwordPage.Body.String(), `value="start"`) {
		t.Fatalf("password continuation missing MFA start = %d %q", passwordPage.Code, passwordPage.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, setupMFAPath, nil)
	request.AddCookie(setupCookie)
	withoutStart := httptest.NewRecorder()
	stack.ServeHTTP(withoutStart, request)
	if withoutStart.Code != http.StatusSeeOther || withoutStart.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("MFA without explicit start = %d location:%q", withoutStart.Code, withoutStart.Header().Get("Location"))
	}
}

func TestSetupMFAStartConfirmReplaceIsEncryptedAndNonMutating(t *testing.T) {
	manager, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	})
	postSetupPassword(stack, setupCookie, url.Values{
		"password":              {"correct horse battery staple for owner"},
		"password_confirmation": {"correct horse battery staple for owner"},
	})

	started := postSetupMFA(stack, setupCookie, url.Values{"action": {"start"}})
	if started.Code != http.StatusSeeOther || started.Header().Get("Location") != setupMFAPath {
		t.Fatalf("MFA start = %d location:%q body:%q", started.Code, started.Header().Get("Location"), started.Body.String())
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil || !state.TOTPStarted || state.TOTPReady || state.Draft.TOTPSecret == "" {
		t.Fatalf("started MFA state = %#v, %v", state, err)
	}
	firstSecret := state.Draft.TOTPSecret
	request := httptest.NewRequest(http.MethodGet, setupMFAPath, nil)
	request.AddCookie(setupCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	for _, want := range []string{
		"Secure the owner account", `action="/setup/mfa"`, `src="data:image/png;base64,`,
		`name="code"`, `inputmode="numeric"`, `autocomplete="one-time-code"`, `pattern="[0-9]{6}"`,
		"SHA1", "6 digits", "30 seconds", "never sent to a third-party QR service",
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("MFA enrollment page missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), firstSecret) {
		t.Fatal("MFA page rendered the ungrouped seed outside its manual-key presentation")
	}
	request = httptest.NewRequest(http.MethodGet, setupMFAPath+"?secret=must-not-survive", nil)
	request.AddCookie(setupCookie)
	query := httptest.NewRecorder()
	stack.ServeHTTP(query, request)
	if query.Code != http.StatusSeeOther || query.Header().Get("Location") != setupMFAPath || strings.Contains(query.Body.String(), "must-not-survive") {
		t.Fatalf("MFA query stripping = %d location:%q body:%q", query.Code, query.Header().Get("Location"), query.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, setupMFAPath, strings.NewReader(strings.Repeat("x", setupFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.AddCookie(setupCookie)
	oversized := httptest.NewRecorder()
	stack.ServeHTTP(oversized, request)
	if oversized.Code != http.StatusUnprocessableEntity || !strings.Contains(oversized.Body.String(), "too large or invalid") {
		t.Fatalf("oversized MFA form = %d %q", oversized.Code, oversized.Body.String())
	}

	invalid := postSetupMFA(stack, setupCookie, url.Values{"action": {"confirm"}, "code": {"000000"}})
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), "invalid or has already been used") || strings.Contains(invalid.Body.String(), `value="000000"`) {
		t.Fatalf("invalid MFA confirmation = %d %q", invalid.Code, invalid.Body.String())
	}
	validCode := setupHandlerTOTPCode(t, firstSecret)
	confirmed := postSetupMFA(stack, setupCookie, url.Values{"action": {"confirm"}, "code": {validCode}})
	if confirmed.Code != http.StatusSeeOther || confirmed.Header().Get("Location") != setupMFAPath || strings.Contains(confirmed.Body.String(), validCode) {
		t.Fatalf("MFA confirmation = %d location:%q body:%q", confirmed.Code, confirmed.Header().Get("Location"), confirmed.Body.String())
	}
	state, err = manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || !state.TOTPReady || state.Draft.TOTPConfirmedStep == nil {
		t.Fatalf("confirmed MFA state = %#v, %v", state, err)
	}

	request = httptest.NewRequest(http.MethodGet, setupMFAPath, nil)
	request.AddCookie(setupCookie)
	review := httptest.NewRecorder()
	stack.ServeHTTP(review, request)
	if review.Code != http.StatusOK || !strings.Contains(review.Body.String(), "Authenticator verified for final enrollment") ||
		!strings.Contains(review.Body.String(), "Next: recovery codes") || strings.Contains(review.Body.String(), "data:image/png") || strings.Contains(review.Body.String(), firstSecret) {
		t.Fatalf("confirmed MFA review = %d %q", review.Code, review.Body.String())
	}

	restarted := postSetupMFA(stack, setupCookie, url.Values{"action": {"restart"}})
	if restarted.Code != http.StatusSeeOther || restarted.Header().Get("Location") != setupMFAPath {
		t.Fatalf("MFA restart = %d location:%q", restarted.Code, restarted.Header().Get("Location"))
	}
	replaced, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || !replaced.TOTPStarted || replaced.TOTPReady || replaced.Draft.TOTPSecret == firstSecret {
		t.Fatalf("replaced MFA state = %#v, %v", replaced, err)
	}

	var payload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(firstSecret)) || bytes.Contains(payload, []byte(replaced.Draft.TOTPSecret)) {
		t.Fatal("encrypted setup payload exposed a TOTP seed")
	}
	var users, passwords, totps, sessions, initialized int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 0 || passwords != 0 || totps != 0 || sessions != 0 || initialized != 0 {
		t.Fatalf("MFA setup partially enrolled = users:%d passwords:%d totps:%d sessions:%d initialized:%d", users, passwords, totps, sessions, initialized)
	}
}

func TestSetupRecoveryRequiresVerifiedTOTPAndExplicitGeneration(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)

	request := httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(setupCookie)
	withoutOwner := httptest.NewRecorder()
	stack.ServeHTTP(withoutOwner, request)
	if withoutOwner.Code != http.StatusSeeOther || withoutOwner.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("recovery without owner/password = %d location:%q", withoutOwner.Code, withoutOwner.Header().Get("Location"))
	}
	postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	})
	postSetupPassword(stack, setupCookie, url.Values{
		"password": {"correct horse battery staple for owner"}, "password_confirmation": {"correct horse battery staple for owner"},
	})
	request = httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(setupCookie)
	withoutTOTP := httptest.NewRecorder()
	stack.ServeHTTP(withoutTOTP, request)
	if withoutTOTP.Code != http.StatusSeeOther || withoutTOTP.Header().Get("Location") != setupMFAPath {
		t.Fatalf("recovery without verified TOTP = %d location:%q", withoutTOTP.Code, withoutTOTP.Header().Get("Location"))
	}

	_, _, verifiedStack, verifiedCookie := prepareHandlerVerifiedMFA(t)
	request = httptest.NewRequest(http.MethodGet, setupMFAPath, nil)
	request.AddCookie(verifiedCookie)
	mfaPage := httptest.NewRecorder()
	verifiedStack.ServeHTTP(mfaPage, request)
	if mfaPage.Code != http.StatusOK || !strings.Contains(mfaPage.Body.String(), `href="/setup/recovery"`) {
		t.Fatalf("verified MFA page missing recovery continuation = %d %q", mfaPage.Code, mfaPage.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(verifiedCookie)
	initial := httptest.NewRecorder()
	verifiedStack.ServeHTTP(initial, request)
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), "Generate recovery codes") ||
		!strings.Contains(initial.Body.String(), "ten 120-bit pseudorandom codes") || strings.Contains(initial.Body.String(), "Recovery codes were already shown") {
		t.Fatalf("initial recovery page = %d %q", initial.Code, initial.Body.String())
	}
}

func TestSetupRecoveryGenerateAcknowledgeAndReplaceIsOneTimeAndNonMutating(t *testing.T) {
	manager, db, stack, setupCookie := prepareHandlerVerifiedMFA(t)
	var originalEvents int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events`).Scan(&originalEvents); err != nil {
		t.Fatal(err)
	}
	generated := postSetupRecovery(stack, setupCookie, url.Values{"action": {"generate"}})
	if generated.Code != http.StatusOK || generated.Header().Get("Cache-Control") != "no-store" || generated.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("recovery generation = %d headers:%v body:%q", generated.Code, generated.Header(), generated.Body.String())
	}
	body := generated.Body.String()
	for _, want := range []string{
		"These codes are shown only in this response", "password manager or print this page now",
		`name="action" value="acknowledge"`, `name="batch_id"`, `name="saved" type="checkbox"`,
		"Generate replacement codes", "never logged, placed in URLs, stored in browser storage",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("generated recovery page missing %q: %q", want, body)
		}
	}
	codePattern := regexp.MustCompile(`<code[^>]*>([0-9A-HJKMNP-TV-Z]{4}(?:-[0-9A-HJKMNP-TV-Z]{4}){5})</code>`)
	matches := codePattern.FindAllStringSubmatch(body, -1)
	if len(matches) != 10 {
		t.Fatalf("rendered recovery codes = %d, want 10: %q", len(matches), body)
	}
	firstCodes := make([]string, 0, len(matches))
	for _, match := range matches {
		firstCodes = append(firstCodes, match[1])
	}

	ownerState, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || ownerState == nil || ownerState.Draft == nil || !ownerState.RecoveryGenerated || ownerState.RecoveryReady || ownerState.Draft.RecoveryBatchID == "" {
		t.Fatalf("generated recovery handler state = %#v, %v", ownerState, err)
	}
	firstBatchID := ownerState.Draft.RecoveryBatchID
	for _, code := range firstCodes {
		if strings.Contains(string(ownerState.Draft.RecoveryCodeHashes[0]), code) {
			t.Fatal("stored recovery hash retained plaintext")
		}
	}
	repeated := postSetupRecovery(stack, setupCookie, url.Values{"action": {"generate"}})
	if repeated.Code != http.StatusSeeOther || repeated.Header().Get("Location") != setupRecoveryPath || codePattern.MatchString(repeated.Body.String()) {
		t.Fatalf("repeated non-replacement generation = %d location:%q body:%q", repeated.Code, repeated.Header().Get("Location"), repeated.Body.String())
	}
	repeatedState, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || repeatedState.Draft.RecoveryBatchID != firstBatchID {
		t.Fatalf("repeated generation replaced batch = %#v, %v", repeatedState, err)
	}
	var payload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, code := range firstCodes {
		if bytes.Contains(payload, []byte(code)) || bytes.Contains(payload, []byte(strings.ReplaceAll(code, "-", ""))) {
			t.Fatal("handler persisted a plaintext recovery code")
		}
	}

	request := httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(setupCookie)
	refreshed := httptest.NewRecorder()
	stack.ServeHTTP(refreshed, request)
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), "Recovery codes were already shown") {
		t.Fatalf("recovery refresh = %d %q", refreshed.Code, refreshed.Body.String())
	}
	for _, code := range firstCodes {
		if strings.Contains(refreshed.Body.String(), code) {
			t.Fatal("recovery GET redisplayed plaintext code")
		}
	}

	unchecked := postSetupRecovery(stack, setupCookie, url.Values{
		"action": {"acknowledge"}, "batch_id": {firstBatchID},
	})
	if unchecked.Code != http.StatusUnprocessableEntity || !strings.Contains(unchecked.Body.String(), "Confirm that you saved") {
		t.Fatalf("unchecked recovery acknowledgement = %d %q", unchecked.Code, unchecked.Body.String())
	}
	acknowledged := postSetupRecovery(stack, setupCookie, url.Values{
		"action": {"acknowledge"}, "batch_id": {firstBatchID}, "saved": {"yes"},
	})
	if acknowledged.Code != http.StatusSeeOther || acknowledged.Header().Get("Location") != setupRecoveryPath {
		t.Fatalf("recovery acknowledgement = %d location:%q body:%q", acknowledged.Code, acknowledged.Header().Get("Location"), acknowledged.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(setupCookie)
	review := httptest.NewRecorder()
	stack.ServeHTTP(review, request)
	if review.Code != http.StatusOK || !strings.Contains(review.Body.String(), "Recovery codes acknowledged for final enrollment") ||
		!strings.Contains(review.Body.String(), "Next: final setup review") || strings.Contains(review.Body.String(), `name="batch_id"`) {
		t.Fatalf("acknowledged recovery review = %d %q", review.Code, review.Body.String())
	}

	replaced := postSetupRecovery(stack, setupCookie, url.Values{"action": {"regenerate"}})
	if replaced.Code != http.StatusOK || !strings.Contains(replaced.Body.String(), "These codes are shown only in this response") {
		t.Fatalf("recovery replacement = %d %q", replaced.Code, replaced.Body.String())
	}
	replacementMatches := codePattern.FindAllStringSubmatch(replaced.Body.String(), -1)
	if len(replacementMatches) != 10 {
		t.Fatalf("replacement recovery codes = %d", len(replacementMatches))
	}
	replacedState, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || replacedState.Draft.RecoveryBatchID == firstBatchID || replacedState.RecoveryReady || replacedState.Draft.RecoveryAcknowledged {
		t.Fatalf("replacement recovery state = %#v, %v", replacedState, err)
	}
	stale := postSetupRecovery(stack, setupCookie, url.Values{
		"action": {"acknowledge"}, "batch_id": {firstBatchID}, "saved": {"yes"},
	})
	if stale.Code != http.StatusUnprocessableEntity || !strings.Contains(stale.Body.String(), "no longer current") {
		t.Fatalf("stale recovery acknowledgement = %d %q", stale.Code, stale.Body.String())
	}

	var users, passwords, totps, recoveryCodes, sessions, events, initialized int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM recovery_codes`:                    &recoveryCodes,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT COUNT(*) FROM auth_events`:                       &events,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 0 || passwords != 0 || totps != 0 || recoveryCodes != 0 || sessions != 0 || events != originalEvents || initialized != 0 {
		t.Fatalf("recovery handler partially enrolled = users:%d passwords:%d totps:%d recovery:%d sessions:%d events:%d initialized:%d",
			users, passwords, totps, recoveryCodes, sessions, events, initialized)
	}
}

func TestSetupRecoveryStripsQueriesBoundsFormsAndRejectsCrossOrigin(t *testing.T) {
	_, db, stack, setupCookie := prepareHandlerVerifiedMFA(t)
	request := httptest.NewRequest(http.MethodGet, setupRecoveryPath+"?code=must-not-survive", nil)
	request.AddCookie(setupCookie)
	query := httptest.NewRecorder()
	stack.ServeHTTP(query, request)
	if query.Code != http.StatusSeeOther || query.Header().Get("Location") != setupRecoveryPath || strings.Contains(query.Body.String(), "must-not-survive") {
		t.Fatalf("recovery query stripping = %d location:%q body:%q", query.Code, query.Header().Get("Location"), query.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, setupRecoveryPath, strings.NewReader(strings.Repeat("x", setupFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.AddCookie(setupCookie)
	oversized := httptest.NewRecorder()
	stack.ServeHTTP(oversized, request)
	if oversized.Code != http.StatusUnprocessableEntity || !strings.Contains(oversized.Body.String(), "too large or invalid") {
		t.Fatalf("oversized recovery form = %d %q", oversized.Code, oversized.Body.String())
	}

	var originalPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"action": {"generate"}}
	request = httptest.NewRequest(http.MethodPost, setupRecoveryPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(setupCookie)
	blocked := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(blocked, request)
	if blocked.Code != http.StatusForbidden || !strings.Contains(blocked.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin recovery generation = %d %q", blocked.Code, blocked.Body.String())
	}
	var storedPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedPayload, originalPayload) {
		t.Fatal("cross-origin recovery generation replaced setup draft")
	}
}

func TestSetupReviewRequiresAcknowledgedRecoveryAndIsLinkedFromRecovery(t *testing.T) {
	_, _, stack, setupCookie := prepareHandlerVerifiedMFA(t)
	request := httptest.NewRequest(http.MethodGet, setupReviewPath, nil)
	request.AddCookie(setupCookie)
	withoutRecovery := httptest.NewRecorder()
	stack.ServeHTTP(withoutRecovery, request)
	if withoutRecovery.Code != http.StatusSeeOther || withoutRecovery.Header().Get("Location") != setupRecoveryPath {
		t.Fatalf("review without recovery acknowledgement = %d location:%q", withoutRecovery.Code, withoutRecovery.Header().Get("Location"))
	}

	_, _, readyStack, readyCookie := prepareHandlerSetupReview(t)
	request = httptest.NewRequest(http.MethodGet, setupRecoveryPath, nil)
	request.AddCookie(readyCookie)
	recoveryPage := httptest.NewRecorder()
	readyStack.ServeHTTP(recoveryPage, request)
	if recoveryPage.Code != http.StatusOK || !strings.Contains(recoveryPage.Body.String(), `href="/setup/review"`) || !strings.Contains(recoveryPage.Body.String(), "Review final setup") {
		t.Fatalf("acknowledged recovery page missing review link = %d %q", recoveryPage.Code, recoveryPage.Body.String())
	}
}

func TestSetupReviewIsSecretFreeReadOnlyAndShowsExactFreshImpact(t *testing.T) {
	manager, db, stack, setupCookie := prepareHandlerSetupReview(t)
	ownerState, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || ownerState == nil || ownerState.Draft == nil {
		t.Fatalf("review owner state = %#v, %v", ownerState, err)
	}
	var originalPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, setupReviewPath, nil)
	request.AddCookie(setupCookie)
	review := httptest.NewRecorder()
	stack.ServeHTTP(review, request)
	if review.Code != http.StatusOK || review.Header().Get("Cache-Control") != "no-store" || review.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("setup review = %d headers:%v body:%q", review.Code, review.Header(), review.Body.String())
	}
	body := review.Body.String()
	for _, want := range []string{
		"Review owner setup", "Create a new active owner and administrator",
		"Owner", "owner", "Prepared security",
		"0 existing password credential(s) will be replaced",
		"0 active TOTP credential(s) will be replaced",
		"0 unused recovery code(s) will be replaced",
		"0 active passkey(s) and 0 app-login identity record(s) remain attached",
		"Initialize a fresh owner without creating or claiming a synthetic default user",
		"0 currently unrevoked session(s) across the instance will be revoked at cutover",
		"Review only — nothing has been committed", `href="/setup/recovery"`,
		`method="post" action="/setup/review"`, "Complete setup and sign in",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("setup review missing %q: %q", want, body)
		}
	}
	for _, secret := range []string{
		ownerState.Draft.PasswordHash, ownerState.Draft.TOTPSecret,
		ownerState.Draft.RecoveryBatchID, ownerState.Draft.RecoveryCodeHashes[0],
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("setup review exposed prepared secret %q", secret)
		}
	}
	if strings.Contains(body, `name="batch_id"`) {
		t.Fatal("setup review rendered recovery batch material")
	}
	var storedPayload []byte
	var users, passwords, totps, recoveryCodes, sessions, events, initialized int
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM recovery_codes`:                    &recoveryCodes,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
		`SELECT COUNT(*) FROM auth_events`:                       &events,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(storedPayload, originalPayload) || users != 0 || passwords != 0 || totps != 0 || recoveryCodes != 0 || sessions != 0 || initialized != 0 || events != 2 {
		t.Fatalf("review handler mutated setup = payloadChanged:%t users:%d passwords:%d totps:%d recovery:%d sessions:%d events:%d initialized:%d",
			!bytes.Equal(storedPayload, originalPayload), users, passwords, totps, recoveryCodes, sessions, events, initialized)
	}

	request = httptest.NewRequest(http.MethodGet, setupReviewPath+"?secret=must-not-survive", nil)
	request.AddCookie(setupCookie)
	query := httptest.NewRecorder()
	stack.ServeHTTP(query, request)
	if query.Code != http.StatusSeeOther || query.Header().Get("Location") != setupReviewPath || strings.Contains(query.Body.String(), "must-not-survive") {
		t.Fatalf("review query stripping = %d location:%q body:%q", query.Code, query.Header().Get("Location"), query.Body.String())
	}

}

func TestSetupCompletionSignsInOwnerConsumesSetupAndRejectsReplay(t *testing.T) {
	manager, _, stack, setupCookie := prepareHandlerSetupReview(t)
	completed := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/" ||
		completed.Header().Get("Cache-Control") != "no-store" || completed.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("setup completion = %d location:%q headers:%v body:%q",
			completed.Code, completed.Header().Get("Location"), completed.Header(), completed.Body.String())
	}
	sessionCookie := responseCookie(completed, "gofer_session", true)
	if sessionCookie == nil || sessionCookie.Value == "" || !sessionCookie.HttpOnly || !sessionCookie.Secure ||
		sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.MaxAge != 30*24*60*60 || sessionCookie.Path != "/" {
		t.Fatalf("setup completion session cookie = %#v", sessionCookie)
	}
	clearedPreAuth := responseCookie(completed, "gofer_pre_auth", false)
	if clearedPreAuth == nil || clearedPreAuth.MaxAge != -1 {
		t.Fatalf("setup completion did not clear pre-auth cookie: %#v", clearedPreAuth)
	}
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil || session.AuthenticationMethod != auth.AuthenticationMethodPassword ||
		session.AssuranceLevel != auth.AssuranceLevelMultiFactor || session.StepUpMethod != auth.AuthenticationMethodTOTP {
		t.Fatalf("setup completion session = %#v, %v", session, err)
	}
	state, err := manager.SetupState(t.Context())
	if err != nil || !state.Initialized || state.OwnerUserID != session.UserID || state.TokenConfigured || state.CutoverVersion != 1 {
		t.Fatalf("completed setup state = %#v, %v", state, err)
	}

	request := httptest.NewRequest(http.MethodGet, setupPath, nil)
	notFound := httptest.NewRecorder()
	stack.ServeHTTP(notFound, request)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("setup after completion = %d %q", notFound.Code, notFound.Body.String())
	}
	replay := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if replay.Code != http.StatusNotFound || responseCookie(replay, "gofer_session", true) != nil {
		t.Fatalf("replayed setup completion = %d cookies:%#v body:%q", replay.Code, replay.Result().Cookies(), replay.Body.String())
	}
}

func TestSetupCompletionRejectsInvalidActionAndBlocksUnassignedMailbox(t *testing.T) {
	_, db, stack, setupCookie := prepareHandlerSetupReview(t)
	invalid := postSetupReview(stack, setupCookie, url.Values{"action": {"preview"}})
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), "Setup was not completed") ||
		!strings.Contains(invalid.Body.String(), "explicit setup completion action") {
		t.Fatalf("invalid completion action = %d %q", invalid.Code, invalid.Body.String())
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address) VALUES ('completion-orphan', NULL, 'orphan@example.com')`); err != nil {
		t.Fatal(err)
	}
	blocked := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "Setup completion is blocked") ||
		!strings.Contains(blocked.Body.String(), "Setup remains uninitialized") ||
		strings.Contains(blocked.Body.String(), "Complete setup and sign in") || responseCookie(blocked, "gofer_session", true) != nil {
		t.Fatalf("blocked completion = %d cookies:%#v body:%q", blocked.Code, blocked.Result().Cookies(), blocked.Body.String())
	}
	var initialized, users, sessions int
	for query, target := range map[string]*int{
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if initialized != 0 || users != 0 || sessions != 0 {
		t.Fatalf("blocked completion mutated setup = initialized:%d users:%d sessions:%d", initialized, users, sessions)
	}
}

func TestSetupCompletionPostIsBoundedQueryFreeAndProtectedByCanonicalOrigin(t *testing.T) {
	_, db, stack, setupCookie := prepareHandlerSetupReview(t)
	var originalPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}

	oversized := postSetupReview(stack, setupCookie, url.Values{"action": {strings.Repeat("x", setupFormMaximumBytes)}})
	if oversized.Code != http.StatusUnprocessableEntity || !strings.Contains(oversized.Body.String(), "too large or invalid") {
		t.Fatalf("oversized setup completion = %d %q", oversized.Code, oversized.Body.String())
	}

	queryRequest := httptest.NewRequest(http.MethodPost, setupReviewPath+"?secret=must-not-survive", strings.NewReader("action=complete"))
	queryRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	queryRequest.Header.Set("Origin", "https://gofer.example")
	queryRequest.AddCookie(setupCookie)
	queryResponse := httptest.NewRecorder()
	stack.ServeHTTP(queryResponse, queryRequest)
	if queryResponse.Code != http.StatusSeeOther || queryResponse.Header().Get("Location") != setupReviewPath ||
		strings.Contains(queryResponse.Body.String(), "must-not-survive") {
		t.Fatalf("completion query stripping = %d location:%q body:%q",
			queryResponse.Code, queryResponse.Header().Get("Location"), queryResponse.Body.String())
	}

	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, setupReviewPath, strings.NewReader("action=complete"))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(setupCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") ||
		responseCookie(recorder, "gofer_session", true) != nil {
		t.Fatalf("cross-origin setup completion = %d cookies:%#v body:%q", recorder.Code, recorder.Result().Cookies(), recorder.Body.String())
	}
	var storedPayload []byte
	var initialized, users, sessions int
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	for query, target := range map[string]*int{
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
	} {
		if err := db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(storedPayload, originalPayload) || initialized != 0 || users != 0 || sessions != 0 {
		t.Fatalf("rejected completion mutated setup = payloadChanged:%t initialized:%d users:%d sessions:%d",
			!bytes.Equal(storedPayload, originalPayload), initialized, users, sessions)
	}
}

func TestSetupReviewBlocksUnassignedMailboxWithoutGuessingOwnership(t *testing.T) {
	_, db, stack, setupCookie := prepareHandlerSetupReview(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address) VALUES ('orphan', NULL, 'orphan@example.com')`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, setupReviewPath, nil)
	request.AddCookie(setupCookie)
	blocked := httptest.NewRecorder()
	stack.ServeHTTP(blocked, request)
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "Setup completion is blocked") ||
		!strings.Contains(blocked.Body.String(), "without a valid user owner") || !strings.Contains(blocked.Body.String(), "will not guess, delete, merge, or reassign") {
		t.Fatalf("blocked setup review = %d %q", blocked.Code, blocked.Body.String())
	}
	var owner any
	if err := db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'orphan'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		t.Fatalf("blocked review assigned orphan mailbox to %#v", owner)
	}
}

func TestSetupMFAPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	})
	postSetupPassword(stack, setupCookie, url.Values{
		"password":              {"correct horse battery staple for owner"},
		"password_confirmation": {"correct horse battery staple for owner"},
	})
	var originalPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"action": {"start"}}
	request := httptest.NewRequest(http.MethodPost, setupMFAPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(setupCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin MFA start = %d %q", recorder.Code, recorder.Body.String())
	}
	var storedPayload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedPayload, originalPayload) {
		t.Fatal("cross-origin MFA start replaced the password draft")
	}
}

func TestLegacyDefaultSetupCreatesSeparateManagementOwnerWithoutMovingData(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name, status, is_admin)
		VALUES ('default', 'local', 'local', 'Local User', 'active', 0);
		INSERT INTO accounts (id, user_id, email_address) VALUES ('mailbox', 'default', 'mail@example.com');
		INSERT INTO app_settings (user_id, key, value) VALUES ('default', 'theme', 'dark')`); err != nil {
		t.Fatal(err)
	}
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	request := httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	request.AddCookie(setupCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	for _, want := range []string{"Create the management owner", "used only for Gofer administration", "Existing webmail users"} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("legacy owner page missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}

	saved := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Cristian Braun"},
		"username": {"cristian"},
	})
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("legacy owner save = %d %q", saved.Code, saved.Body.String())
	}
	var username, name, accountOwner, settingOwner string
	if err := db.Read().QueryRow(`SELECT username, name FROM users WHERE id = 'default'`).Scan(&username, &name); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'mailbox'`).Scan(&accountOwner); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT user_id FROM app_settings WHERE key = 'theme'`).Scan(&settingOwner); err != nil {
		t.Fatal(err)
	}
	if username != "local" || name != "Local User" || accountOwner != "default" || settingOwner != "default" {
		t.Fatalf("legacy draft changed data = username:%q name:%q account:%q setting:%q", username, name, accountOwner, settingOwner)
	}
}

func TestExistingSetupUsersRemainSeparateFromNewManagementOwner(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name, status, user_type, is_admin)
		VALUES
			('person-a', 'person-a', 'person-a', 'Person A', 'active', 'webmail', 0),
			('person-b', 'person-b', 'person-b', 'Person B', 'active', 'management', 1)`); err != nil {
		t.Fatal(err)
	}
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	request := httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	request.AddCookie(setupCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	for _, want := range []string{"Create the management owner", "Existing webmail users", `value="create"`} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("existing owner page missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}

	collision := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"person-b"},
	})
	if collision.Code != http.StatusUnprocessableEntity || !strings.Contains(collision.Body.String(), "already used by another Gofer user") {
		t.Fatalf("owner collision = %d %q", collision.Code, collision.Body.String())
	}

	selected := postSetupOwner(stack, setupCookie, url.Values{
		"owner_target": {"create"}, "name": {"New Management Owner"},
		"username": {"new-owner"},
	})
	if selected.Code != http.StatusSeeOther || selected.Header().Get("Location") != setupPasswordPath {
		t.Fatalf("explicit owner selection = %d %q", selected.Code, selected.Body.String())
	}
	var names string
	if err := db.Read().QueryRow(`SELECT group_concat(name, '|') FROM (SELECT name FROM users ORDER BY id)`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "Person A|Person B" {
		t.Fatalf("explicit selection mutated users: %q", names)
	}
}

func TestSetupOwnerPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	entry := postSetup(stack, setupToken)
	setupCookie := responseCookie(entry, "gofer_pre_auth", true)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"owner_target": {"create"}, "name": {"Owner"}, "username": {"owner"},
	}
	request := httptest.NewRequest(http.MethodPost, setupOwnerPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(setupCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin owner submission = %d %q", recorder.Code, recorder.Body.String())
	}
	var payload []byte
	if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatalf("cross-origin owner submission stored payload: %x", payload)
	}
}

func TestSetupTokenFailuresAreIndistinguishableAndDoNotEchoSecrets(t *testing.T) {
	type testCase struct {
		name  string
		alter func(*storage.DB)
		token func(string) string
	}
	tests := []testCase{
		{name: "unknown", token: func(string) string { return "unknown-private-setup-token" }},
		{name: "expired", alter: func(db *storage.DB) {
			_, _ = db.Write().Exec(`UPDATE auth_system_state SET setup_expires_at = ? WHERE id = 1`, time.Now().UTC().Add(-time.Minute))
		}},
		{name: "blocked", alter: func(db *storage.DB) {
			_, _ = db.Write().Exec(`UPDATE auth_system_state SET setup_attempts = 10 WHERE id = 1`)
		}},
	}
	var referenceBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, db, stack, setupToken := setupEntryStack(t)
			if test.alter != nil {
				test.alter(db)
			}
			submitted := setupToken
			if test.token != nil {
				submitted = test.token(setupToken)
			}
			recorder := postSetup(stack, submitted)
			if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), setupTokenFailureMessage) {
				t.Fatalf("generic setup failure = %d %q", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), submitted) || strings.Contains(recorder.Body.String(), setupToken) {
				t.Fatal("setup failure echoed submitted or stored credentials")
			}
			if responseCookie(recorder, "gofer_pre_auth", true) != nil {
				t.Fatal("setup failure issued an access cookie")
			}
			if referenceBody == "" {
				referenceBody = recorder.Body.String()
			} else if recorder.Body.String() != referenceBody {
				t.Fatal("setup failure body differs by token state")
			}
			var challenges int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil || challenges != 0 {
				t.Fatalf("rejected setup challenge count = %d, %v", challenges, err)
			}
		})
	}
}

func TestSetupRoutesDisappearAfterInitialization(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	if _, err := db.Write().Exec(`
		UPDATE auth_system_state
		SET initialized = 1, setup_token_hash = NULL, setup_expires_at = NULL
		WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: setupPath},
		{method: http.MethodPost, path: setupPath},
		{method: http.MethodGet, path: setupOwnerPath},
		{method: http.MethodPost, path: setupOwnerPath},
		{method: http.MethodGet, path: setupPasswordPath},
		{method: http.MethodPost, path: setupPasswordPath},
		{method: http.MethodGet, path: setupMFAPath},
		{method: http.MethodPost, path: setupMFAPath},
		{method: http.MethodGet, path: setupRecoveryPath},
		{method: http.MethodPost, path: setupRecoveryPath},
		{method: http.MethodGet, path: setupReviewPath},
		{method: http.MethodPost, path: setupReviewPath},
	} {
		var request *http.Request
		if test.method == http.MethodPost {
			form := url.Values{"token": {setupToken}}
			request = httptest.NewRequest(test.method, test.path, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			request = httptest.NewRequest(test.method, test.path, nil)
		}
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), setupToken) || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("initialized %s %s = %d %q", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
	var challenges, events int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type LIKE 'setup_token_verification%' OR event_type = 'setup_token_verified'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 || events != 0 {
		t.Fatalf("initialized setup routes mutated state = challenges:%d events:%d", challenges, events)
	}
}

func TestSetupPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"token": {setupToken}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin setup = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("cross-origin setup mutation = attempts:%d challenges:%d", attempts, challenges)
	}
}

func TestSetupAcceptsPrivacyBrowserNullOriginWithSameOriginMetadata(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"token": {setupToken}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupOwnerPath || responseCookie(recorder, "gofer_pre_auth", true) == nil {
		t.Fatalf("null-origin same-origin setup = %d location:%q body:%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
}

func TestSetupRejectsOversizedFormWithoutAdvancingAttempts(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader("token="+strings.Repeat("x", setupFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), setupTokenFailureMessage) || strings.Contains(recorder.Body.String(), setupToken) {
		t.Fatalf("oversized setup form = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("oversized setup mutation = attempts:%d challenges:%d", attempts, challenges)
	}
}

func TestSetupTokenIsIgnoredInQueryString(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodPost, setupPath+"?token="+url.QueryEscape(setupToken), strings.NewReader(""))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupPath || strings.Contains(recorder.Body.String(), setupToken) || responseCookie(recorder, "gofer_pre_auth", true) != nil || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("query-string setup token = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("query-string setup handling = attempts:%d challenges:%d", attempts, challenges)
	}

	request = httptest.NewRequest(http.MethodGet, setupPath+"?token="+url.QueryEscape(setupToken), nil)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupPath || strings.Contains(recorder.Body.String(), setupToken) || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("query-string setup GET = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestPersonalSetupUsesTokenAndSingleProfileWithoutAdminRoutes(t *testing.T) {
	manager, _, stack, token := setupEntryStack(t)
	manager.Config().Mode = auth.ModePersonal
	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)
		return rec
	}
	if r := get("/login", nil); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/setup" {
		t.Fatalf("uninitialized login: %d", r.Code)
	}
	start := postSetup(stack, token)
	var setupCookie *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.Value != "" {
			setupCookie = cookie
		}
	}
	if setupCookie == nil {
		t.Fatalf("setup start: %d %s", start.Code, start.Body.String())
	}
	if page := get("/setup/owner", setupCookie); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Protect your personal Gofer") || strings.Contains(page.Body.String(), "Create Management") {
		t.Fatalf("personal setup: %d %s", page.Code, page.Body.String())
	}
	form := url.Values{"name": {"Personal User"}, "username": {"person"}, "password": {"correct horse battery staple 874!"}, "password_confirmation": {"correct horse battery staple 874!"}}
	completed := postSetupOwner(stack, setupCookie, form)
	var sessionCookie *http.Cookie
	for _, cookie := range completed.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.Value != "" {
			sessionCookie = cookie
		}
	}
	if completed.Code != http.StatusSeeOther || sessionCookie == nil || completed.Header().Get("Location") != "/settings/security" {
		t.Fatalf("completion: %d %s", completed.Code, completed.Body.String())
	}
	if page := get("/settings/security", sessionCookie); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Add passkey") || !strings.Contains(page.Body.String(), "TOTP") {
		t.Fatalf("personal security: %d %s", page.Code, page.Body.String())
	}
	for _, path := range []string{"/admin", "/admin/users", "/admin/account/security", "/api/admin/users", "/account/enroll", "/account/enroll/google", "/account/recover", "/setup/owner", "/setup/mfa"} {
		if rec := get(path, sessionCookie); rec.Code != http.StatusNotFound {
			t.Fatalf("unavailable route %s = %d", path, rec.Code)
		}
	}
	if page := get("/login", nil); page.Code != http.StatusOK || strings.Contains(page.Body.String(), "Have an invitation?") || strings.Contains(page.Body.String(), `href="/account/recover"`) {
		t.Fatalf("personal login: %d %s", page.Code, page.Body.String())
	}
	login, err := manager.AuthenticatePassword(t.Context(), auth.PasswordLoginOptions{Identifier: "person", Password: form.Get("password"), RequiredUserType: auth.UserTypeWebmail, Source: "127.0.0.1"})
	if err != nil || login.Session == nil || login.Session.UserID != "default" || login.PreAuthChallenge != nil {
		t.Fatalf("personal password login %#v %v", login, err)
	}
}
