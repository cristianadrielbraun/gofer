package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionCookiesRespectSecureConfiguration(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "loopback HTTP", true: "remote HTTPS"}[secure], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			SetSessionCookie(recorder, "session-token", secure)

			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %d, want 1", len(cookies))
			}
			cookie := cookies[0]
			if cookie.Secure != secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("session cookie flags = Secure:%t HttpOnly:%t SameSite:%d", cookie.Secure, cookie.HttpOnly, cookie.SameSite)
			}

			recorder = httptest.NewRecorder()
			ClearSessionCookie(recorder, secure)
			cookies = recorder.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != secure || cookies[0].MaxAge != -1 {
				t.Fatalf("cleared session cookie = %#v", cookies)
			}
		})
	}
}
