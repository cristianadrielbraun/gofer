package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
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
