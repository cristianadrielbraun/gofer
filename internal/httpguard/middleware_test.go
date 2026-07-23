package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareRequestBoundary(t *testing.T) {
	cfg, err := newConfig(DefaultListenAddr, DefaultBaseURL, false)
	if err != nil {
		t.Fatalf("newConfig() error = %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		host       string
		headers    map[string]string
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "trusted safe request",
			method:     http.MethodGet,
			path:       "/",
			host:       "local.localhost:8090",
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "trusted loopback alias",
			method:     http.MethodGet,
			path:       "/",
			host:       "localhost:8090",
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "untrusted host",
			method:     http.MethodGet,
			path:       "/",
			host:       "attacker.example",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			name:       "host prefix attack",
			method:     http.MethodGet,
			path:       "/",
			host:       "local.localhost:8090.attacker.example",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			name:       "same-origin compose",
			method:     http.MethodPost,
			path:       "/compose",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Origin": "http://local.localhost:8090"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "same-origin referer",
			method:     http.MethodDelete,
			path:       "/api/messages/42",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Referer": "http://local.localhost:8090/folder/inbox"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "same-origin fetch metadata",
			method:     http.MethodPatch,
			path:       "/api/settings/ui",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "explicit automation request",
			method:     http.MethodPost,
			path:       "/api/mail/sync",
			host:       "local.localhost:8090",
			headers:    map[string]string{automationHeader: "1"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "foreign-origin compose is blocked before handler",
			method:     http.MethodPost,
			path:       "/compose",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Origin": "https://attacker.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "origin prefix attack",
			method:     http.MethodPost,
			path:       "/compose",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Origin": "http://local.localhost:8090.attacker.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "headerless unsafe request",
			method:     http.MethodPost,
			path:       "/compose",
			host:       "local.localhost:8090",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-site fetch metadata overrides custom header",
			method:     http.MethodPost,
			path:       "/compose",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site", automationHeader: "1"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "OAuth callback remains a safe navigation",
			method:     http.MethodGet,
			path:       "/auth/google/callback",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "same-origin SSE",
			method:     http.MethodGet,
			path:       "/api/events",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Referer": "http://local.localhost:8090/"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "foreign-origin SSE",
			method:     http.MethodGet,
			path:       "/api/events",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Origin": "https://attacker.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "headerless SSE",
			method:     http.MethodGet,
			path:       "/api/events",
			host:       "local.localhost:8090",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, nil)
			request.Host = tt.host
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}

			cfg.Middleware(next).ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Fatalf("handler called = %t, want %t", called, tt.wantCalled)
			}
			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
			}
			if got := recorder.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'self'" {
				t.Fatalf("Content-Security-Policy = %q", got)
			}
		})
	}
}

func TestMiddlewareAcceptsExactLoopbackOriginAliases(t *testing.T) {
	cfg, err := newConfig(DefaultListenAddr, DefaultBaseURL, false)
	if err != nil {
		t.Fatalf("newConfig() error = %v", err)
	}
	for _, origin := range []string{
		"http://local.localhost:8090",
		"http://localhost:8090",
		"http://127.0.0.1:8090",
		"http://[::1]:8090",
	} {
		t.Run(origin, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/mail/sync", nil)
			request.Host = "local.localhost:8090"
			request.Header.Set("Origin", origin)

			cfg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
		})
	}
}
