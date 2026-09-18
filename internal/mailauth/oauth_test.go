package mailauth

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

func TestMicrosoftAccountInfoFromIDToken(t *testing.T) {
	idToken := testMicrosoftIDToken(t, map[string]any{
		"aud":                "client-id",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"sub":                "subject-id",
		"preferred_username": "person@outlook.com",
		"name":               "Person Outlook",
	})

	info, err := microsoftAccountInfoFromIDToken(idToken, "client-id", time.Now())
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

func TestMicrosoftAccountInfoFromIDTokenRejectsAudienceMismatch(t *testing.T) {
	idToken := testMicrosoftIDToken(t, map[string]any{
		"aud":                "other-client",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"sub":                "subject-id",
		"preferred_username": "person@outlook.com",
	})

	_, err := microsoftAccountInfoFromIDToken(idToken, "client-id", time.Now())
	if err == nil || !strings.Contains(err.Error(), "audience mismatch") {
		t.Fatalf("error = %v, want audience mismatch", err)
	}
}

func TestMicrosoftAccountOAuthURLForcesConsentForContacts(t *testing.T) {
	manager := New(&Config{
		BaseURL: "https://gofer.example",
		MicrosoftClient: &oauth2.Config{
			ClientID:    "client-id",
			RedirectURL: "https://gofer.example/auth/microsoft/mailbox/callback",
			Scopes:      microsoftAccountTokenScopes(),
			Endpoint:    oauth2.Endpoint{AuthURL: "https://login.example/authorize"},
		},
	}, nil, testMailboxCredentialKey)

	rawURL := manager.MicrosoftAccountOAuthURL("state-value")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	values := parsed.Query()
	if got := values.Get("prompt"); got != "consent" {
		t.Fatalf("prompt = %q, want consent", got)
	}
	for _, scope := range []string{microsoftGraphContactsScope, microsoftGraphCalendarScope, microsoftGraphMailScope, microsoftGraphMailSendScope, microsoftGraphMailboxSettingsScope} {
		if !strings.Contains(values.Get("scope"), scope) {
			t.Fatalf("scope = %q, want Graph scope %q", values.Get("scope"), scope)
		}
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

	manager := New(&Config{
		BaseURL: "https://gofer.example",
		MicrosoftClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://gofer.example/auth/microsoft/mailbox/callback",
			Endpoint:     oauth2.Endpoint{TokenURL: server.URL},
		},
	}, nil, testMailboxCredentialKey)

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
