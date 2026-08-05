package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareUsesDistinctUnauthenticatedResponseModes(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, true)
	called := 0
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name         string
		method       string
		target       string
		headers      map[string]string
		status       int
		location     string
		hxRedirect   string
		contentType  string
		body         string
		returnTarget string
	}{
		{
			name: "browser navigation", method: http.MethodGet, target: "/settings/advanced?from=mail",
			status: http.StatusSeeOther, location: "/login", returnTarget: "/settings/advanced?from=mail",
		},
		{
			name: "htmx navigation", method: http.MethodGet, target: "/folder/inbox?email=42",
			headers: map[string]string{"HX-Request": "true"}, status: http.StatusUnauthorized,
			hxRedirect: "/login", contentType: "text/plain; charset=utf-8", body: "authentication required\n",
			returnTarget: "/folder/inbox?email=42",
		},
		{
			name: "htmx API action", method: http.MethodPost, target: "/api/mail/sync",
			headers: map[string]string{"HX-Request": "TRUE"}, status: http.StatusUnauthorized,
			hxRedirect: "/login", contentType: "text/plain; charset=utf-8", body: "authentication required\n",
		},
		{
			name: "JSON API", method: http.MethodGet, target: "/api/folders/unread",
			headers: map[string]string{"Accept": "application/json"}, status: http.StatusUnauthorized,
			contentType: "application/json; charset=utf-8", body: "{\"error\":\"authentication_required\"}\n",
		},
		{
			name: "SSE", method: http.MethodGet, target: "/api/events",
			headers: map[string]string{"Accept": "text/event-stream"}, status: http.StatusUnauthorized,
		},
		{
			name: "browser form", method: http.MethodPost, target: "/admin/security/private-target",
			status: http.StatusSeeOther, location: "/login",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, nil)
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			if recorder.Code != test.status || response.Header.Get("Location") != test.location || response.Header.Get("HX-Redirect") != test.hxRedirect {
				t.Fatalf("response = status:%d location:%q HX-Redirect:%q", recorder.Code, response.Header.Get("Location"), response.Header.Get("HX-Redirect"))
			}
			if got := response.Header.Get("Content-Type"); test.contentType != "" && got != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
			}
			if test.name == "SSE" && strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("unauthenticated SSE response retained stream content type %q", response.Header.Get("Content-Type"))
			}
			if test.name == "SSE" && recorder.Body.Len() != 0 {
				t.Fatalf("unauthenticated SSE body = %q, want empty", recorder.Body.String())
			}
			if test.body != "" && recorder.Body.String() != test.body {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.body)
			}
			if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Vary") != "HX-Request" {
				t.Fatalf("cache headers = Cache-Control:%q Vary:%q", response.Header.Get("Cache-Control"), response.Header.Get("Vary"))
			}
			loginRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
			for _, cookie := range response.Cookies() {
				loginRequest.AddCookie(cookie)
			}
			if got := GetReturnTo(loginRequest); got != test.returnTarget {
				t.Fatalf("return target = %q, want %q", got, test.returnTarget)
			}
		})
	}
	if called != 0 {
		t.Fatalf("protected handler called %d times", called)
	}
}

func TestMiddlewareClearsInvalidSessionForEveryResponseMode(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, true)
	called := false
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	tests := []struct {
		name    string
		target  string
		headers map[string]string
		status  int
	}{
		{name: "browser", target: "/", status: http.StatusSeeOther},
		{name: "HTMX", target: "/contacts", headers: map[string]string{"HX-Request": "true"}, status: http.StatusUnauthorized},
		{name: "API", target: "/api/folders/unread", status: http.StatusUnauthorized},
		{name: "SSE", target: "/api/events", status: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "invalid-session-token"})
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			cleared := false
			for _, cookie := range recorder.Result().Cookies() {
				if cookie.Name == sessionCookieName && cookie.MaxAge == -1 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatalf("invalid session cookie was not cleared: %#v", recorder.Result().Cookies())
			}
		})
	}
	if called {
		t.Fatal("protected handler was called for an invalid session")
	}
}

func TestMiddlewareKeepsPublicRoutesUnauthenticated(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, true)
	called := make(map[string]int)
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called[r.URL.Path]++
		if GetCurrentUser(r.Context()) != nil || GetCurrentSession(r.Context()) != nil {
			t.Fatalf("public request context = path:%q user:%#v session:%#v", r.URL.Path, GetCurrentUser(r.Context()), GetCurrentSession(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	publicPaths := []string{"/login", "/login/mfa", "/auth/google", "/auth/google/callback", "/assets/app.js", "/sw.js"}
	for _, path := range publicPaths {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if called[path] != 1 || recorder.Code != http.StatusNoContent {
				t.Fatalf("public request %q calls=%d status=%d", path, called[path], recorder.Code)
			}
		})
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/login", nil))
	if called["/login"] != 2 || recorder.Code != http.StatusNoContent {
		t.Fatalf("public POST /login calls=%d status=%d", called["/login"], recorder.Code)
	}
}
