package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

func (m *Manager) GoogleOAuthURL(state string) string {
	return m.config.GoogleClient.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (m *Manager) GoogleAccountOAuthURL(state string) string {
	return m.accountOAuthConfig().AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (m *Manager) ExchangeAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.accountOAuthConfig().Exchange(ctx, code)
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

	if err := m.autoSetupGmail(ctx, user.ID, info.Email, info.Name); err != nil {
		return nil, nil, fmt.Errorf("gmail auto-setup: %w", err)
	}

	session, err := m.CreateSession(ctx, user.ID, userAgent)
	if err != nil {
		return nil, nil, err
	}

	return user, session, nil
}

func (m *Manager) autoSetupGmail(ctx context.Context, userID, email, displayName string) error {
	var existing string
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE user_id = ? AND imap_host = 'imap.gmail.com' LIMIT 1`, userID,
	).Scan(&existing)
	if err == nil && existing != "" {
		return nil
	}

	id := "gmail_" + email
	initials := extractInitials(displayName)
	color := generateColor(id)

	_, err = m.db.Write().ExecContext(ctx,
		`INSERT OR IGNORE INTO accounts (id, user_id, email_address, display_name, color, initials,
		  imap_host, imap_port, imap_tls_mode,
		  smtp_host, smtp_port, smtp_tls_mode,
		  username, auth_method)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, email, displayName, color, initials,
		"imap.gmail.com", 993, "tls",
		"smtp.gmail.com", 465, "tls",
		email, "oauth2")
	return err
}

func extractInitials(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(string(parts[0][0]) + string(parts[1][0]))
	}
	if len(name) >= 2 {
		return strings.ToUpper(name[:2])
	}
	return strings.ToUpper(name)
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
