package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestMicrosoftUserInfoFromIDToken(t *testing.T) {
	idToken := testMicrosoftIDToken(t, map[string]any{
		"aud":                "client-id",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"sub":                "subject-id",
		"preferred_username": "person@outlook.com",
		"name":               "Person Outlook",
	})

	info, err := microsoftUserInfoFromIDToken(idToken, "client-id", time.Now())
	if err != nil {
		t.Fatalf("parse microsoft id token: %v", err)
	}
	if got := info.ProviderAccountID(); got != "subject-id" {
		t.Fatalf("provider account id = %q, want subject-id", got)
	}
	if got := info.EmailAddress(); got != "person@outlook.com" {
		t.Fatalf("email address = %q, want person@outlook.com", got)
	}
	if info.Name != "Person Outlook" {
		t.Fatalf("name = %q, want Person Outlook", info.Name)
	}
}

func TestMicrosoftUserInfoFromIDTokenRejectsAudienceMismatch(t *testing.T) {
	idToken := testMicrosoftIDToken(t, map[string]any{
		"aud":                "other-client",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"sub":                "subject-id",
		"preferred_username": "person@outlook.com",
	})

	_, err := microsoftUserInfoFromIDToken(idToken, "client-id", time.Now())
	if err == nil {
		t.Fatal("expected audience mismatch")
	}
	if !strings.Contains(err.Error(), "audience mismatch") {
		t.Fatalf("error = %q, want audience mismatch", err)
	}
}

func TestMicrosoftAccountOAuthURLForcesConsentForContacts(t *testing.T) {
	manager := NewManager(&Config{
		BaseURL: "https://gofer.example",
		MicrosoftClient: &oauth2.Config{
			ClientID:    "client-id",
			RedirectURL: "https://gofer.example/auth/microsoft/account/callback",
			Scopes: []string{
				"openid",
				"email",
				"profile",
				"offline_access",
				microsoftGraphContactsScope,
				microsoftGraphMailScope,
				microsoftGraphMailSendScope,
				microsoftGraphMailboxSettingsScope,
			},
			Endpoint: oauth2.Endpoint{AuthURL: "https://login.example/authorize"},
		},
	}, nil)

	rawURL := manager.MicrosoftAccountOAuthURL("state-value")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	values := parsed.Query()
	if got := values.Get("prompt"); got != "consent" {
		t.Fatalf("prompt = %q, want consent", got)
	}
	if !strings.Contains(values.Get("scope"), microsoftGraphContactsScope) {
		t.Fatalf("scope = %q, want Graph contacts scope", values.Get("scope"))
	}
	if !strings.Contains(values.Get("scope"), microsoftGraphMailScope) {
		t.Fatalf("scope = %q, want Graph mail scope", values.Get("scope"))
	}
	if !strings.Contains(values.Get("scope"), microsoftGraphMailSendScope) {
		t.Fatalf("scope = %q, want Graph mail send scope", values.Get("scope"))
	}
	if !strings.Contains(values.Get("scope"), microsoftGraphMailboxSettingsScope) {
		t.Fatalf("scope = %q, want Graph mailbox settings scope", values.Get("scope"))
	}
	if strings.Contains(values.Get("scope"), "outlook.office.com/IMAP") || strings.Contains(values.Get("scope"), "outlook.office.com/SMTP") {
		t.Fatalf("scope = %q, must not request Outlook IMAP/SMTP scopes", values.Get("scope"))
	}
}

func TestExchangeMicrosoftAccountCodeRequestsGraphMailScopes(t *testing.T) {
	ctx := context.Background()
	var gotScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotScope = r.FormValue("scope")
		if got := r.FormValue("grant_type"); got != "authorization_code" {
			t.Fatalf("grant_type = %q, want authorization_code", got)
		}
		if got := r.FormValue("code"); got != "auth-code" {
			t.Fatalf("code = %q, want auth-code", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600,"scope":"https://graph.microsoft.com/Contacts.ReadWrite https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/MailboxSettings.ReadWrite"}`))
	}))
	defer server.Close()

	manager := NewManager(&Config{
		BaseURL: "https://gofer.example",
		MicrosoftClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://gofer.example/auth/microsoft/account/callback",
			Endpoint:     oauth2.Endpoint{TokenURL: server.URL},
		},
	}, nil)

	token, err := manager.ExchangeMicrosoftAccountCode(ctx, "auth-code")
	if err != nil {
		t.Fatalf("ExchangeMicrosoftAccountCode() error = %v", err)
	}
	if token.AccessToken != "graph-token" {
		t.Fatalf("access token = %q, want graph-token", token.AccessToken)
	}
	if gotScope != strings.Join(microsoftAccountTokenExchangeScopes(), " ") {
		t.Fatalf("scope = %q, want Microsoft token exchange scopes", gotScope)
	}
	for _, graphScope := range []string{microsoftGraphContactsScope, microsoftGraphMailScope, microsoftGraphMailSendScope, microsoftGraphMailboxSettingsScope} {
		if !strings.Contains(gotScope, graphScope) {
			t.Fatalf("scope = %q, want Graph scope %q during code exchange", gotScope, graphScope)
		}
	}
	if strings.Contains(gotScope, "outlook.office.com/IMAP") || strings.Contains(gotScope, "outlook.office.com/SMTP") {
		t.Fatalf("scope = %q, must not request Outlook IMAP/SMTP scopes during code exchange", gotScope)
	}
}

func testMicrosoftIDToken(t *testing.T, payload map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body) + "."
}
