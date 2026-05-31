package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"golang.org/x/oauth2"
)

type GoogleUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
}

type MicrosoftUserInfo struct {
	Sub               string `json:"sub"`
	ObjectID          string `json:"oid"`
	TenantID          string `json:"tid"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	Name              string `json:"name"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
}

func (i MicrosoftUserInfo) EmailAddress() string {
	for _, value := range []string{i.Email, i.PreferredUsername, i.UPN} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func (i MicrosoftUserInfo) ProviderAccountID() string {
	if strings.TrimSpace(i.Sub) != "" {
		return strings.TrimSpace(i.Sub)
	}
	if strings.TrimSpace(i.TenantID) != "" && strings.TrimSpace(i.ObjectID) != "" {
		return strings.TrimSpace(i.TenantID) + ":" + strings.TrimSpace(i.ObjectID)
	}
	return strings.TrimSpace(i.ObjectID)
}

func (m *Manager) GoogleOAuthURL(state string) string {
	return m.config.GoogleClient.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (m *Manager) GoogleAccountOAuthURL(state string) string {
	return m.accountOAuthConfig().AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (m *Manager) MicrosoftAccountOAuthURL(state string) string {
	return m.microsoftAccountOAuthConfig().AuthCodeURL(state, oauth2.SetAuthURLParam("prompt", "select_account"))
}

func (m *Manager) ExchangeAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.accountOAuthConfig().Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func (m *Manager) ExchangeMicrosoftAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.microsoftAccountOAuthConfig().Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func (m *Manager) accountOAuthConfig() *oauth2.Config {
	cfg := m.config.GoogleClient
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  m.config.BaseURL + "/auth/google/account/callback",
		Scopes:       cfg.Scopes,
		Endpoint:     cfg.Endpoint,
	}
}

func (m *Manager) microsoftAccountOAuthConfig() *oauth2.Config {
	cfg := m.config.MicrosoftClient
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  m.config.BaseURL + "/auth/microsoft/account/callback",
		Scopes:       cfg.Scopes,
		Endpoint:     cfg.Endpoint,
	}
}

func (m *Manager) GenerateState() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) ExchangeCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.config.GoogleClient.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func (m *Manager) GetGoogleUserInfo(ctx context.Context, token *oauth2.Token) (*GoogleUserInfo, error) {
	client := m.config.GoogleClient.Client(ctx, token)
	resp, err := client.Get("https://openidconnect.googleapis.com/v1/userinfo")
	if err != nil {
		return nil, fmt.Errorf("fetch userinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, string(body))
	}

	var info GoogleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}

	return &info, nil
}

func (m *Manager) GetMicrosoftUserInfo(ctx context.Context, token *oauth2.Token) (*MicrosoftUserInfo, error) {
	_ = ctx
	idToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(idToken) == "" {
		return nil, fmt.Errorf("microsoft id token not returned")
	}
	return microsoftUserInfoFromIDToken(idToken, m.config.MicrosoftClient.ClientID, time.Now())
}

func microsoftUserInfoFromIDToken(idToken, clientID string, now time.Time) (*MicrosoftUserInfo, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid microsoft id token")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode microsoft id token: %w", err)
	}

	var claims struct {
		MicrosoftUserInfo
		Audience any   `json:"aud"`
		Expires  int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse microsoft id token: %w", err)
	}
	if clientID != "" && !microsoftAudienceMatches(claims.Audience, clientID) {
		return nil, fmt.Errorf("microsoft id token audience mismatch")
	}
	if claims.Expires > 0 && !now.IsZero() && time.Unix(claims.Expires, 0).Before(now.Add(-1*time.Minute)) {
		return nil, fmt.Errorf("microsoft id token expired")
	}

	info := claims.MicrosoftUserInfo
	if info.ProviderAccountID() == "" {
		return nil, fmt.Errorf("microsoft id token missing subject")
	}
	if info.EmailAddress() == "" {
		return nil, fmt.Errorf("microsoft id token missing email")
	}
	return &info, nil
}

func microsoftAudienceMatches(audience any, clientID string) bool {
	switch v := audience.(type) {
	case string:
		return v == clientID
	case []any:
		for _, item := range v {
			if value, ok := item.(string); ok && value == clientID {
				return true
			}
		}
	}
	return false
}

func (m *Manager) HandleGoogleCallback(ctx context.Context, code string, userAgent string) (*User, *Session, error) {
	token, err := m.ExchangeCode(ctx, code)
	if err != nil {
		return nil, nil, err
	}

	info, err := m.GetGoogleUserInfo(ctx, token)
	if err != nil {
		return nil, nil, err
	}

	if !info.EmailVerified {
		return nil, nil, fmt.Errorf("email not verified")
	}

	user, err := m.CreateOrUpdateUser(ctx, info.Email, info.Name, info.Picture)
	if err != nil {
		return nil, nil, err
	}

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		t := token.Expiry
		expiresAt = &t
	}

	err = m.UpsertOAuthAccount(ctx, user.ID, "google", info.Sub, token.AccessToken, token.RefreshToken, token.TokenType, expiresAt, "")
	if err != nil {
		return nil, nil, fmt.Errorf("store oauth account: %w", err)
	}

	if err := m.autoSetupGmail(ctx, user.ID, info.Email, info.Name, info.Sub); err != nil {
		return nil, nil, fmt.Errorf("gmail auto-setup: %w", err)
	}

	session, err := m.CreateSession(ctx, user.ID, userAgent)
	if err != nil {
		return nil, nil, err
	}

	return user, session, nil
}

func (m *Manager) autoSetupGmail(ctx context.Context, userID, email, displayName, providerAccountID string) error {
	var existing string
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts
		 WHERE user_id = ? AND (
		   (provider = ? AND provider_account_id = ? AND provider_account_id != '')
		   OR (email_address = ? AND imap_host = 'imap.gmail.com' AND auth_method = 'oauth2')
		 ) LIMIT 1`, userID, providers.ProviderGmail, providerAccountID, email,
	).Scan(&existing)
	if err == nil && existing != "" {
		_, _ = m.db.Write().ExecContext(ctx,
			`UPDATE accounts SET provider = ?, provider_account_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			providers.ProviderGmail, providerAccountID, existing,
		)
		return nil
	}

	id := "gmail_" + email
	initials := extractInitials(displayName)
	color := generateColor(id)

	_, err = m.db.Write().ExecContext(ctx,
		`INSERT OR IGNORE INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, color, initials,
		  imap_host, imap_port, imap_tls_mode,
		  smtp_host, smtp_port, smtp_tls_mode,
		  username, auth_method)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, providers.ProviderGmail, providerAccountID, email, displayName, color, initials,
		"imap.gmail.com", 993, "tls",
		"smtp.gmail.com", 465, "tls",
		email, "oauth2")
	return err
}

func extractInitials(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(firstRune(parts[0]) + firstRune(parts[1]))
	}
	runes := []rune(name)
	if len(runes) >= 2 {
		return strings.ToUpper(string(runes[:2]))
	}
	return strings.ToUpper(name)
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

func generateColor(id string) string {
	colors := []string{"#3b82f6", "#8b5cf6", "#ec4899", "#f97316", "#14b8a6", "#6366f1"}
	h := 0
	for _, c := range id {
		h = h*31 + int(c)
	}
	return colors[abs(h)%len(colors)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
