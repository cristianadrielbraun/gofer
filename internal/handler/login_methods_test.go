package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func postLoginMethods(t *testing.T, handler *Handler, identifier string) string {
	t.Helper()
	form := url.Values{"identifier": {identifier}}
	request := httptest.NewRequest(http.MethodPost, "/login/methods", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "198.51.100.72:43120"
	recorder := httptest.NewRecorder()
	handler.handleLoginMethods(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login methods status = %d", recorder.Code)
	}
	return strings.TrimSpace(recorder.Body.String())
}

func insertLoginMethodsPasskey(t *testing.T, handler *Handler) {
	t.Helper()
	if _, err := handler.db.Write().Exec(`
		INSERT INTO webauthn_users (user_id, rp_id, user_handle) VALUES ('person', 'gofer.example', x'00112233445566778899aabbccddeeff');
		INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name, credential_ciphertext, key_version, rp_id)
		VALUES ('person-passkey', 'person', x'01', x'02', 'Laptop', x'03', 1, 'gofer.example')`); err != nil {
		t.Fatal(err)
	}
}

// Only a username with a passkey is offered one; an unknown username looks
// like one without.
func TestLoginMethodsOffersPasskeyOnlyToUsernamesWithOne(t *testing.T) {
	handler, _, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	for _, identifier := range []string{"person", "nobody", ""} {
		if got := postLoginMethods(t, handler, identifier); got != `{"passkey":false}` {
			t.Fatalf("%q before a passkey = %s", identifier, got)
		}
	}
	insertLoginMethodsPasskey(t, handler)
	if got := postLoginMethods(t, handler, " Person "); got != `{"passkey":true}` {
		t.Fatalf("person with a passkey = %s", got)
	}
	if got := postLoginMethods(t, handler, "nobody"); got != `{"passkey":false}` {
		t.Fatalf("unknown username = %s", got)
	}
}

// Without JavaScript, Continue posts only the username and gets back the
// page opened at its sign-in options, without trying a sign-in.
func TestLoginContinueWithoutJavaScriptShowsSignInOptions(t *testing.T) {
	handler, _, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	insertLoginMethodsPasskey(t, handler)
	form := url.Values{"identifier": {"person"}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "198.51.100.73:43120"
	recorder := httptest.NewRecorder()
	handler.handleLoginSubmit(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || responseCookie(recorder, "gofer_session", true) != nil {
		t.Fatalf("continue = %d", recorder.Code)
	}
	for _, want := range []string{
		`data-login-reveal="password" data-open="true"`, `data-login-reveal="passkey" data-open="true"`,
		`value="person"`, `role="alert"`,
	} {
		if !strings.Contains(body, want) != (want == `role="alert"`) {
			t.Fatalf("continue page has %q = %v", want, strings.Contains(body, want))
		}
	}
	passwordInput := body[strings.Index(body, `id="login-password"`):]
	passwordInput = passwordInput[:strings.Index(passwordInput, ">")]
	if strings.Contains(passwordInput, "disabled") {
		t.Fatalf("the password is still disabled: %s", passwordInput)
	}
}
