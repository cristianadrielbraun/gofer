package httpguard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #54: a page served through HTTPS loads with an HTTP base URL, but
// Review import is rejected before the uploaded profile can be processed.
func TestProfileImportBaseURLMismatch(t *testing.T) {
	for _, baseURL := range []string{"http://mail.example.test", "https://mail.example.test"} {
		for _, htmx := range []bool{false, true} {
			t.Run(baseURL+"/htmx="+fmt.Sprint(htmx), func(t *testing.T) {
				cfg, err := newConfig("0.0.0.0:8090", baseURL, true)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				handler := cfg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.WriteHeader(http.StatusNoContent)
				}))
				for _, path := range []string{"/api/profile/import/preview", "/api/profile/import"} {
					request := httptest.NewRequest(http.MethodPost, "https://mail.example.test"+path, strings.NewReader("profile data"))
					request.Header.Set("Origin", "https://mail.example.test")
					request.Header.Set("Sec-Fetch-Site", "same-origin")
					if htmx {
						request.Header.Set("HX-Request", "true")
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if baseURL == "https://mail.example.test" {
						if response.Code != http.StatusNoContent || response.Header().Get("HX-Trigger") != "" {
							t.Fatalf("correct base URL: status=%d headers=%v", response.Code, response.Header())
						}
						continue
					}
					if response.Code != http.StatusForbidden || calls != 0 {
						t.Fatalf("mismatch: status=%d handler calls=%d", response.Code, calls)
					}
					message := strings.TrimSpace(response.Body.String())
					for _, explanation := range []string{"GOFER_BASE_URL", "https://", "reverse proxy", "restart Gofer"} {
						if !strings.Contains(message, explanation) {
							t.Errorf("missing %q in %q", explanation, message)
						}
					}
					if htmx {
						var trigger map[string]string
						if err := json.Unmarshal([]byte(response.Header().Get("HX-Trigger")), &trigger); err != nil {
							t.Fatal(err)
						}
						if trigger["gofer-request-blocked"] != message {
							t.Fatalf("HTMX explanation = %v, want %q", trigger, message)
						}
					} else if response.Header().Get("HX-Trigger") != "" {
						t.Fatal("non-HTMX response has a trigger")
					}
				}
				if baseURL == "https://mail.example.test" && calls != 2 {
					t.Fatalf("correct base URL: handler calls=%d", calls)
				}
			})
		}
	}
}

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
			name:       "privacy browser null origin with same-origin fetch metadata",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "localhost:8090",
			headers:    map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "null origin with same-site fetch metadata",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "localhost:8090",
			headers:    map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null origin with cross-site fetch metadata",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "localhost:8090",
			headers:    map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null origin with user-initiated fetch metadata",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "localhost:8090",
			headers:    map[string]string{"Origin": "null", "Sec-Fetch-Site": "none"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null origin without fetch metadata",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "localhost:8090",
			headers:    map[string]string{"Origin": "null"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null origin cannot bypass untrusted host",
			method:     http.MethodPost,
			path:       "/account/redeem",
			host:       "attacker.example",
			headers:    map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusMisdirectedRequest,
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
			path:       "/auth/google/login/callback",
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
		{
			name:       "same-origin admin SSE",
			method:     http.MethodGet,
			path:       "/api/admin/events",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Referer": "http://local.localhost:8090/admin/avatars/"},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "foreign-origin admin SSE",
			method:     http.MethodGet,
			path:       "/api/admin/events",
			host:       "local.localhost:8090",
			headers:    map[string]string{"Origin": "https://attacker.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "headerless admin SSE",
			method:     http.MethodGet,
			path:       "/api/admin/events",
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
			if got := recorder.Header().Get("Content-Security-Policy"); got != PagePolicy {
				t.Fatalf("Content-Security-Policy = %q", got)
			}
		})
	}
}

func TestMiddlewareRejectsMultipleOriginHeadersWithSameOriginFetchMetadata(t *testing.T) {
	cfg, err := newConfig(DefaultListenAddr, DefaultBaseURL, false)
	if err != nil {
		t.Fatalf("newConfig() error = %v", err)
	}
	called := false
	request := httptest.NewRequest(http.MethodPost, "/account/redeem", nil)
	request.Host = "localhost:8090"
	request.Header.Add("Origin", "null")
	request.Header.Add("Origin", "http://localhost:8090")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	cfg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("multiple Origin headers = status:%d called:%t", recorder.Code, called)
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

func TestMiddlewareKeepsLoopbackHostsWithLANBaseURL(t *testing.T) {
	cfg, err := newConfig("0.0.0.0:8090", "http://192.168.0.10:8090", false)
	if err != nil {
		t.Fatalf("newConfig() error = %v", err)
	}
	for _, tc := range []struct {
		host       string
		wantStatus int
	}{
		{"192.168.0.10:8090", http.StatusNoContent},
		{"localhost:8090", http.StatusNoContent},
		{"127.0.0.1:8090", http.StatusNoContent},
		{"[::1]:8090", http.StatusNoContent},
		{"localhost:9999", http.StatusMisdirectedRequest},
		{"192.168.0.11:8090", http.StatusMisdirectedRequest},
	} {
		t.Run(tc.host, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Host = tc.host

			cfg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
		})
	}
}
