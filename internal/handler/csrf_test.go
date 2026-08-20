package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func csrfProofForSession(t *testing.T, manager *auth.Manager, sessionToken, action string) string {
	t.Helper()
	probe := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.CSRFToken(r.Context(), http.MethodPost, action))
	}))
	probePath := "/settings/security"
	if session, err := manager.GetSessionByToken(t.Context(), sessionToken); err == nil && session != nil {
		if user, err := manager.GetUserByID(t.Context(), session.UserID); err == nil && user != nil && user.IsManagement() {
			probePath = "/admin/account/security"
		}
	}
	request := httptest.NewRequest(http.MethodGet, probePath, nil)
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: sessionToken})
	recorder := httptest.NewRecorder()
	probe.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(recorder.Body.String()) != 64 {
		t.Fatalf("CSRF proof probe = status:%d token:%q", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

func csrfProofFromForm(t *testing.T, body, action string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)<form[^>]*action="` + regexp.QuoteMeta(action) + `"[^>]*>.*?<input type="hidden" name="_csrf" value="([0-9a-f]{64})">`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("rendered page omitted CSRF proof for %q", action)
	}
	return match[1]
}

func TestAdminSecurityFormsRequireRenderedSessionCSRFProof(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			user_type, is_admin, created_at, updated_at
		) VALUES ('management-admin', 'management-admin', 'management-admin',
			'Management Admin', 'active', 1, 'management', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert management administrator: %v", err)
	}
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	handler.auth = manager
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "management-admin", "test-agent",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), session.UserID, session.ID, auth.AuthenticationMethodTOTP,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	if err := db.AddHTTPDiscoveryException(t.Context(), "mail.example.test", "management-admin"); err != nil {
		t.Fatalf("seed HTTP discovery exception: %v", err)
	}
	exceptions, err := db.ListMailSecurityExceptions(t.Context())
	if err != nil || len(exceptions) != 1 {
		t.Fatalf("seeded security exceptions = %#v, %v", exceptions, err)
	}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	request := httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin security page status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), session.Token) {
		t.Fatal("admin security page exposed the raw session bearer")
	}
	const action = "/admin/security/private-target"
	actions := []string{
		"/admin/security/http-discovery",
		"/admin/security/plaintext",
		action,
		"/admin/security/exceptions/" + exceptions[0].ID + "/delete",
	}
	proofs := map[string]string{}
	seenProofs := map[string]bool{}
	for _, formAction := range actions {
		proofs[formAction] = csrfProofFromForm(t, recorder.Body.String(), formAction)
		if seenProofs[proofs[formAction]] {
			t.Fatalf("rendered CSRF proof was reused across actions: %q", formAction)
		}
		seenProofs[proofs[formAction]] = true
	}
	proof := proofs[action]

	form := url.Values{
		"protocol":    {"http"},
		"host":        {"127.0.0.1"},
		"port":        {"8080"},
		"acknowledge": {"yes"},
	}
	request = httptest.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing-proof mutation status = %d, want 403", recorder.Code)
	}
	if allowed, err := db.IsPrivateTargetAllowed(t.Context(), "http", "127.0.0.1", 8080); err != nil || allowed {
		t.Fatalf("private target after rejected mutation = %t, %v", allowed, err)
	}

	form.Set(auth.CSRFFormFieldName, proof)
	request = httptest.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("valid-proof mutation status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if allowed, err := db.IsPrivateTargetAllowed(t.Context(), "http", "127.0.0.1", 8080); err != nil || !allowed {
		t.Fatalf("private target after valid mutation = %t, %v", allowed, err)
	}
}

func TestAdminMailSecurityMutationsRequireRecentStrongStepUp(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			user_type, is_admin, created_at, updated_at
		) VALUES ('management-admin', 'management-admin', 'management-admin',
			'Management Admin', 'active', 1, 'management', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	handler.auth = manager
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "management-admin", "admin browser",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddHTTPDiscoveryException(t.Context(), "existing.example.test", "management-admin"); err != nil {
		t.Fatal(err)
	}
	exceptions, err := db.ListMailSecurityExceptions(t.Context())
	if err != nil || len(exceptions) != 1 {
		t.Fatalf("seeded exceptions = %#v, %v", exceptions, err)
	}
	deletePath := "/admin/security/exceptions/" + exceptions[0].ID + "/delete"
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	pageRequest.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Recent administrator verification required") ||
		!strings.Contains(page.Body.String(), `href="/admin/account/security"`) || !strings.Contains(page.Body.String(), " disabled") {
		t.Fatalf("stale admin security page = %d %q", page.Code, page.Body.String())
	}

	tests := []struct {
		path string
		form url.Values
	}{
		{path: "/admin/security/http-discovery", form: url.Values{"domain": {"new.example.test"}, "acknowledge": {"yes"}}},
		{path: "/admin/security/plaintext", form: url.Values{"protocol": {"imap"}, "host": {"mail.lab.test"}, "port": {"1143"}, "acknowledge": {"yes"}}},
		{path: "/admin/security/private-target", form: url.Values{"protocol": {"http"}, "host": {"127.0.0.1"}, "port": {"8080"}, "acknowledge": {"yes"}}},
		{path: deletePath, form: url.Values{}},
	}
	post := func(test struct {
		path string
		form url.Values
	}) *httptest.ResponseRecorder {
		t.Helper()
		test.form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), test.path))
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, request)
		return recorder
	}
	for _, test := range tests {
		recorder := post(test)
		if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/admin/security?error=") {
			t.Fatalf("stale mutation %q = %d %q", test.path, recorder.Code, recorder.Header().Get("Location"))
		}
	}
	assertOnlySeeded := func(t *testing.T) {
		t.Helper()
		stored, err := db.ListMailSecurityExceptions(t.Context())
		if err != nil || len(stored) != 1 || stored[0].ID != exceptions[0].ID {
			t.Fatalf("exceptions after rejected mutation = %#v, %v", stored, err)
		}
	}
	assertOnlySeeded(t)

	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), session.UserID, session.ID, auth.AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(password) = %t, %v", steppedUp, err)
	}
	if recorder := post(tests[2]); recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/admin/security?error=") {
		t.Fatalf("weak admin step-up mutation = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	assertOnlySeeded(t)

	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), session.UserID, session.ID, auth.AuthenticationMethodTOTP,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(TOTP) = %t, %v", steppedUp, err)
	}
	recorder := post(tests[2])
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/admin/security?notice=HTTP+private+target+is+now+allowed+for+127.0.0.1%3A8080." {
		t.Fatalf("strong admin step-up mutation = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if allowed, err := db.IsPrivateTargetAllowed(t.Context(), "http", "127.0.0.1", 8080); err != nil || !allowed {
		t.Fatalf("private target after strong step-up = %t, %v", allowed, err)
	}
}

func TestLogoutRequiresSessionCSRFBeforeRevocation(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("new auth session test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, created_at, updated_at)
		VALUES ('user-id', 'user', 'user', 'User', 'active', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	session, err := manager.CreateSession(t.Context(), "user-id", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	handler := &Handler{auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing-proof logout status = %d, want 403", recorder.Code)
	}
	if current, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || current == nil {
		t.Fatalf("session after rejected logout = %#v, %v", current, err)
	}

	proof := csrfProofForSession(t, manager, session.Token, "/auth/logout")
	form := url.Values{auth.CSRFFormFieldName: {proof}}
	request = httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("valid-proof logout = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if current, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || current != nil {
		t.Fatalf("session after valid logout = %#v, %v", current, err)
	}
}
