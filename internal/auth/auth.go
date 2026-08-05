package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type User struct {
	ID                 string
	Email              string
	EmailNormalized    string
	Username           string
	UsernameNormalized string
	Name               string
	AvatarURL          string
	Status             UserStatus
	AuthVersion        int64
	MFARequired        bool
	LastLoginAt        *time.Time
	DisabledAt         *time.Time
	DisabledBy         string
	IsAdmin            bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type UserStatus string

const (
	UserStatusPending  UserStatus = "pending"
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

func (status UserStatus) AllowsAuthentication() bool {
	return status == UserStatusActive
}

type Session struct {
	ID                   string
	UserID               string
	Token                string
	AuthVersion          int64
	AuthenticationMethod AuthenticationMethod
	AssuranceLevel       AssuranceLevel
	UserAgent            string
	AuthenticatedAt      time.Time
	LastUsedAt           time.Time
	IdleExpiresAt        time.Time
	AbsoluteExpiresAt    time.Time
	StepUpAt             *time.Time
	StepUpMethod         AuthenticationMethod
	RevokedAt            *time.Time
	RevokedBy            string
	RevocationReason     SessionRevocationReason
	CreatedAt            time.Time
}

type PreAuthChallenge struct {
	ID                string
	Token             string
	Nonce             string
	UserID            string
	SessionID         string
	Purpose           ChallengePurpose
	Origin            string
	Attempts          int
	MaxAttempts       int
	PayloadCiphertext []byte
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ConsumedAt        *time.Time
}

type PreAuthChallengeOptions struct {
	UserID      string
	SessionID   string
	Purpose     ChallengePurpose
	Origin      string
	Lifetime    time.Duration
	MaxAttempts int
	IssueNonce  bool
}

type OAuthAccount struct {
	ID                string
	UserID            string
	Provider          string
	ProviderAccountID string
	AccessToken       string
	RefreshToken      string
	TokenType         string
	ExpiresAt         *time.Time
	Scopes            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type contextKey string

const (
	userContextKey    contextKey = "user"
	sessionContextKey contextKey = "session"
)

func UserFromContext(ctx context.Context) *User {
	u, ok := ctx.Value(userContextKey).(*User)
	if !ok {
		return nil
	}
	return u
}

func ContextWithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userContextKey, u)
}

func SessionFromContext(ctx context.Context) *Session {
	session, ok := ctx.Value(sessionContextKey).(*Session)
	if !ok {
		return nil
	}
	return session
}

func ContextWithSession(ctx context.Context, session *Session) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

type Config struct {
	Enabled         bool
	GoogleClient    *oauth2.Config
	MicrosoftClient *oauth2.Config
	BaseURL         string
	SecureCookies   bool
}

func LoadConfig(baseURL string) *Config {
	enabled := os.Getenv("GOFER_AUTH_ENABLED") == "true"
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "http://local.localhost:8090"
	}

	cfg := &Config{
		Enabled: enabled,
		BaseURL: baseURL,
	}

	clientID := os.Getenv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")

	if clientID != "" && clientSecret != "" {
		cfg.GoogleClient = &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  baseURL + "/auth/google/callback",
			Scopes: []string{
				"openid",
				"email",
				"profile",
				"https://mail.google.com/",
				"https://www.googleapis.com/auth/contacts",
			},
			Endpoint: google.Endpoint,
		}
	} else if enabled {
		fmt.Println("WARNING: GOFER_AUTH_ENABLED but GOOGLE_OAUTH_CLIENT_ID or GOOGLE_OAUTH_CLIENT_SECRET not set")
		cfg.Enabled = false
	}

	microsoftClientID := os.Getenv("MICROSOFT_OAUTH_CLIENT_ID")
	microsoftClientSecret := os.Getenv("MICROSOFT_OAUTH_CLIENT_SECRET")
	if microsoftClientID != "" && microsoftClientSecret != "" {
		tenant := strings.TrimSpace(os.Getenv("MICROSOFT_OAUTH_TENANT"))
		if tenant == "" {
			tenant = "common"
		}
		cfg.MicrosoftClient = &oauth2.Config{
			ClientID:     microsoftClientID,
			ClientSecret: microsoftClientSecret,
			RedirectURL:  baseURL + "/auth/microsoft/account/callback",
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
			Endpoint: microsoftEndpoint(tenant),
		}
	}

	return cfg
}

func microsoftEndpoint(tenant string) oauth2.Endpoint {
	tenant = strings.Trim(strings.TrimSpace(tenant), "/")
	if tenant == "" {
		tenant = "common"
	}
	base := "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0"
	return oauth2.Endpoint{
		AuthURL:  base + "/authorize",
		TokenURL: base + "/token",
	}
}

type Manager struct {
	config *Config
	db     *storage.DB
	clock  Clock
	tokens TokenGenerator
}

func NewManager(config *Config, db *storage.DB, dependencies ...Dependencies) *Manager {
	deps := Dependencies{Clock: systemClock{}, Tokens: secureTokenGenerator{}}
	if len(dependencies) > 0 {
		if dependencies[0].Clock != nil {
			deps.Clock = dependencies[0].Clock
		}
		if dependencies[0].Tokens != nil {
			deps.Tokens = dependencies[0].Tokens
		}
	}
	return &Manager{config: config, db: db, clock: deps.Clock, tokens: deps.Tokens}
}

func (m *Manager) Config() *Config {
	return m.config
}

func (m *Manager) IsEnabled() bool {
	return m.config.Enabled
}

func (m *Manager) HasGoogleOAuth() bool {
	return m.config.GoogleClient != nil
}

func (m *Manager) HasMicrosoftOAuth() bool {
	return m.config.MicrosoftClient != nil
}

func (m *Manager) DB() *storage.DB {
	return m.db
}

func (m *Manager) EnsureDefaultUser() error {
	if m.config.Enabled {
		return nil
	}

	var count int
	err := m.db.Read().QueryRow("SELECT COUNT(*) FROM users WHERE id = 'default'").Scan(&count)
	if err != nil {
		return fmt.Errorf("check default user: %w", err)
	}
	if count > 0 {
		return nil
	}

	now := m.clock.Now()
	_, err = m.db.Write().Exec(
		`INSERT INTO users (id, email, email_normalized, name, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"default", "local@gofer.local", "local@gofer.local", "Local User", UserStatusActive, 1, now, now,
	)
	if err != nil {
		return fmt.Errorf("create default user: %w", err)
	}

	_, err = m.db.Write().Exec(`UPDATE accounts SET user_id = 'default' WHERE user_id IS NULL`)
	if err != nil {
		return fmt.Errorf("assign accounts to default user: %w", err)
	}

	_, err = m.db.Write().Exec(`UPDATE app_settings SET user_id = 'default' WHERE user_id IS NULL`)
	if err != nil {
		return fmt.Errorf("assign settings to default user: %w", err)
	}

	return nil
}

func (m *Manager) GetDefaultUser() *User {
	if m.config.Enabled {
		return nil
	}
	return &User{
		ID:              "default",
		Email:           "local@gofer.local",
		EmailNormalized: "local@gofer.local",
		Name:            "Local User",
		Status:          UserStatusActive,
		AuthVersion:     1,
		IsAdmin:         true,
	}
}

func (m *Manager) GetUserByID(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(m.db.Read().QueryRowContext(ctx, userSelect+` WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}
