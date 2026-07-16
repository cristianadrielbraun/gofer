package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSafeReturnTo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "mailto intent", raw: "/?mailto=mailto%3Ahello%40example.com%3Fsubject%3DHello", want: "/?mailto=mailto%3Ahello%40example.com%3Fsubject%3DHello"},
		{name: "application path", raw: "/settings/advanced?from=test", want: "/settings/advanced?from=test"},
		{name: "absolute URL", raw: "https://attacker.example/", want: ""},
		{name: "scheme relative URL", raw: "//attacker.example/", want: ""},
		{name: "backslash host", raw: `/\attacker.example/`, want: ""},
		{name: "encoded slash host", raw: "/%2fattacker.example/", want: ""},
		{name: "login loop", raw: "/login", want: ""},
		{name: "auth loop", raw: "/auth/google", want: ""},
		{name: "relative path", raw: "settings", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeReturnTo(tt.raw); got != tt.want {
				t.Fatalf("SafeReturnTo(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestMiddlewareDoesNotPreserveNonGETRequests(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, true)
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler should not be reached without a session")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/send", nil)

	handler.ServeHTTP(recorder, request)

	if got := GetReturnTo(httptest.NewRequest(http.MethodGet, "/login", nil)); got != "" {
		t.Fatalf("GetReturnTo() without a cookie = %q", got)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == returnToCookieName {
			t.Fatal("non-GET request should not create a return-to cookie")
		}
	}
}

func TestReturnToCookieRoundTrip(t *testing.T) {
	recorder := httptest.NewRecorder()
	SetReturnToCookie(recorder, "/?mailto=mailto%3Ahello%40example.com")
	result := recorder.Result()
	if len(result.Cookies()) != 1 {
		t.Fatalf("cookies = %d, want 1", len(result.Cookies()))
	}

	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	request.AddCookie(result.Cookies()[0])
	if got := GetReturnTo(request); got != "/?mailto=mailto%3Ahello%40example.com" {
		t.Fatalf("GetReturnTo() = %q", got)
	}
}

func TestMiddlewarePreservesMailtoIntent(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, true)
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler should not be reached without a session")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?mailto=mailto%3Ahello%40example.com", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("redirect = %d %q, want 303 /login", recorder.Code, recorder.Header().Get("Location"))
	}
	result := recorder.Result()
	request = httptest.NewRequest(http.MethodGet, "/login", nil)
	for _, cookie := range result.Cookies() {
		request.AddCookie(cookie)
	}
	if got := GetReturnTo(request); got != "/?mailto=mailto%3Ahello%40example.com" {
		t.Fatalf("preserved return path = %q", got)
	}
}
