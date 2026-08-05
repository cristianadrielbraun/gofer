package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const csrfTestAction = "/admin/security/private-target"

func csrfTokenThroughMiddleware(t *testing.T, manager *Manager, sessionToken, method, path string) string {
	t.Helper()
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, CSRFToken(r.Context(), method, path))
	}))
	request := httptest.NewRequest(http.MethodGet, "/csrf-token-probe", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("CSRF token probe status = %d", recorder.Code)
	}
	token := recorder.Body.String()
	if !canonicalPresentedCSRFToken(token) {
		t.Fatalf("CSRF token probe returned %q", token)
	}
	return token
}

func TestSessionCSRFRejectsMissingInvalidCrossSessionStaleAndActionReplay(t *testing.T) {
	now := time.Date(2026, time.August, 5, 8, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"session-one-id", "session-two-id", "rotated-id"},
		tokens: []string{"session-one-token", "session-two-token", "rotated-token"},
	})
	insertActiveUser(t, manager, "user-one", false, now)
	insertActiveUser(t, manager, "user-two", false, now)
	sessionOne, err := manager.CreateSession(t.Context(), "user-one", "agent-one")
	if err != nil {
		t.Fatalf("CreateSession(user one) error = %v", err)
	}
	sessionTwo, err := manager.CreateSession(t.Context(), "user-two", "agent-two")
	if err != nil {
		t.Fatalf("CreateSession(user two) error = %v", err)
	}

	tokenOne := csrfTokenThroughMiddleware(t, manager, sessionOne.Token, http.MethodPost, csrfTestAction)
	tokenTwo := csrfTokenThroughMiddleware(t, manager, sessionTwo.Token, http.MethodPost, csrfTestAction)
	otherActionToken := csrfTokenThroughMiddleware(t, manager, sessionOne.Token, http.MethodPost, "/admin/security/plaintext")
	if tokenOne == tokenTwo || tokenOne == otherActionToken || strings.Contains(tokenOne, sessionOne.Token) {
		t.Fatalf("CSRF proofs are not isolated: one=%q two=%q other=%q", tokenOne, tokenTwo, otherActionToken)
	}

	called := 0
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name         string
		sessionToken string
		formTokens   []string
		headers      []string
	}{
		{name: "missing", sessionToken: sessionOne.Token},
		{name: "malformed", sessionToken: sessionOne.Token, formTokens: []string{"not-a-token"}},
		{name: "cross session", sessionToken: sessionTwo.Token, formTokens: []string{tokenOne}},
		{name: "other action replay", sessionToken: sessionOne.Token, formTokens: []string{otherActionToken}},
		{name: "duplicate form values", sessionToken: sessionOne.Token, formTokens: []string{tokenOne, tokenOne}},
		{name: "duplicate headers", sessionToken: sessionOne.Token, headers: []string{tokenOne, tokenOne}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			form := url.Values{}
			for _, token := range test.formTokens {
				form.Add(CSRFFormFieldName, token)
			}
			request := httptest.NewRequest(http.MethodPost, csrfTestAction, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for _, token := range test.headers {
				request.Header.Add(CSRFHeaderName, token)
			}
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: test.sessionToken})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden || called != 0 {
				t.Fatalf("request status = %d called=%d, want 403/0", recorder.Code, called)
			}
		})
	}

	form := url.Values{CSRFFormFieldName: {tokenOne}}
	request := httptest.NewRequest(http.MethodPost, csrfTestAction, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionOne.Token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("valid form request status = %d called=%d, want 204/1", recorder.Code, called)
	}

	request = httptest.NewRequest(http.MethodPost, csrfTestAction, nil)
	request.Header.Set(CSRFHeaderName, tokenOne)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionOne.Token})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || called != 2 {
		t.Fatalf("valid header request status = %d called=%d, want 204/2", recorder.Code, called)
	}

	rotated, err := manager.RotateSession(t.Context(), sessionOne.Token, "rotated-agent")
	if err != nil {
		t.Fatalf("RotateSession() error = %v", err)
	}
	form = url.Values{CSRFFormFieldName: {tokenOne}}
	request = httptest.NewRequest(http.MethodPost, csrfTestAction, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rotated.Token})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || called != 2 {
		t.Fatalf("stale replay status = %d called=%d, want 403/2", recorder.Code, called)
	}
}

func TestSessionCSRFScopePreservesOtherPostsAndLegacyLocalMode(t *testing.T) {
	now := time.Date(2026, time.August, 5, 8, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"session-id"}, tokens: []string{"session-token"},
	})
	insertActiveUser(t, manager, "user-id", false, now)
	session, err := manager.CreateSession(t.Context(), "user-id", "agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	called := 0
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/mail/sync", nil),
		httptest.NewRequest(http.MethodGet, csrfTestAction, nil),
	} {
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.Token})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("unscoped request %s %s status = %d", request.Method, request.URL.Path, recorder.Code)
		}
	}

	insertActiveUser(t, manager, "default", true, now)
	manager.config.Enabled = false
	request := httptest.NewRequest(http.MethodPost, csrfTestAction, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || called != 3 {
		t.Fatalf("legacy local request status = %d called=%d, want 204/3", recorder.Code, called)
	}
}
