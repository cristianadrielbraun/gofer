package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
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
	UserType           UserType
	IsAdmin            bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type UserType string

const (
	UserTypeWebmail    UserType = "webmail"
	UserTypeManagement UserType = "management"
)

func (userType UserType) Valid() bool {
	return userType == UserTypeWebmail || userType == UserTypeManagement
}

func (user *User) IsManagement() bool {
	return user != nil && user.UserType == UserTypeManagement
}

// RequiresManagementHandoff identifies the one upgrade-only state retained
// long enough to split an older mailbox-owning administrator into two users.
func (user *User) RequiresManagementHandoff() bool {
	return user != nil && user.UserType == UserTypeWebmail && user.IsAdmin
}

type AdministratorUserSummary struct {
	ID       string
	Username string
	Email    string
	Status   UserStatus
	UserType UserType
	IsAdmin  bool
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

type SecuritySessionSummary struct {
	ID                   string
	ActionReference      string
	Current              bool
	Active               bool
	AuthenticationMethod AuthenticationMethod
	AssuranceLevel       AssuranceLevel
	UserAgent            string
	AuthenticatedAt      time.Time
	LastUsedAt           time.Time
	RevokedAt            *time.Time
}

type SecuritySessionList struct {
	Sessions  []SecuritySessionSummary
	Truncated bool
}

type SecurityEventSummary struct {
	OccurredAt time.Time
	EventType  AuthEventType
	Success    bool
	Reason     AuthEventReason
	UserAgent  string
}

type SecurityEventOverview struct {
	TotalEvents int64
}

type SecurityEventPage struct {
	Events      []SecurityEventSummary
	TotalEvents int64
	Page        int64
	TotalPages  int64
	PageSize    int64
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
	Enabled              bool
	SetupToken           string
	GoogleLoginClient    *oauth2.Config
	MicrosoftLoginClient *oauth2.Config
	MicrosoftLoginTenant string
	OIDCLoginClient      *oauth2.Config
	OIDCLoginIssuer      string
	OIDCLoginName        string
	BaseURL              string
	SecureCookies        bool
}

func LoadConfig(baseURL string) *Config {
	enabled := os.Getenv("GOFER_AUTH_ENABLED") == "true"
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "http://local.localhost:8090"
	}

	cfg := &Config{
		Enabled:    enabled,
		SetupToken: os.Getenv("GOFER_SETUP_TOKEN"),
		BaseURL:    baseURL,
	}

	clientID := strings.TrimSpace(os.Getenv("GOFER_GOOGLE_LOGIN_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("GOFER_GOOGLE_LOGIN_CLIENT_SECRET"))

	if clientID != "" && clientSecret != "" {
		cfg.GoogleLoginClient = &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  baseURL + "/auth/google/callback",
			Scopes: []string{
				googleApplicationOpenIDScope,
				googleApplicationEmailScope,
				googleApplicationProfileScope,
			},
			Endpoint: google.Endpoint,
		}
	}

	microsoftClientID := strings.TrimSpace(os.Getenv("GOFER_MICROSOFT_LOGIN_CLIENT_ID"))
	microsoftClientSecret := strings.TrimSpace(os.Getenv("GOFER_MICROSOFT_LOGIN_CLIENT_SECRET"))
	microsoftTenant := normalizeMicrosoftLoginTenant(os.Getenv("GOFER_MICROSOFT_LOGIN_TENANT"))
	if microsoftClientID != "" && microsoftClientSecret != "" && microsoftTenant != "" {
		cfg.MicrosoftLoginTenant = microsoftTenant
		cfg.MicrosoftLoginClient = &oauth2.Config{
			ClientID:     microsoftClientID,
			ClientSecret: microsoftClientSecret,
			RedirectURL:  baseURL + microsoftLoginCallbackPath,
			Scopes: []string{
				microsoftApplicationOpenIDScope,
				microsoftApplicationProfileScope,
				microsoftApplicationEmailScope,
			},
			Endpoint: microsoftLoginOAuthEndpoint(microsoftTenant),
		}
	}

	oidcClientID := strings.TrimSpace(os.Getenv("GOFER_OIDC_LOGIN_CLIENT_ID"))
	oidcClientSecret := strings.TrimSpace(os.Getenv("GOFER_OIDC_LOGIN_CLIENT_SECRET"))
	oidcIssuer := normalizeOIDCLoginIssuer(os.Getenv("GOFER_OIDC_LOGIN_ISSUER"))
	if oidcClientID != "" && oidcClientSecret != "" && oidcIssuer != "" {
		cfg.OIDCLoginIssuer = oidcIssuer
		cfg.OIDCLoginName = normalizeOIDCLoginName(os.Getenv("GOFER_OIDC_LOGIN_NAME"))
		cfg.OIDCLoginClient = &oauth2.Config{
			ClientID:     oidcClientID,
			ClientSecret: oidcClientSecret,
			RedirectURL:  baseURL + oidcLoginCallbackPath,
			Scopes: []string{
				oidcApplicationOpenIDScope,
				oidcApplicationProfileScope,
				oidcApplicationEmailScope,
			},
		}
	}

	return cfg
}

type Manager struct {
	config                     *Config
	db                         *storage.DB
	clock                      Clock
	tokens                     TokenGenerator
	bucketHashKey              []byte
	googleIDTokenVerifier      GoogleIDTokenVerifier
	microsoftIDTokenVerifier   MicrosoftIDTokenVerifier
	oidcIDTokenVerifier        OIDCIDTokenVerifier
	oidcEndpointMu             sync.Mutex
	oidcEndpoint               *oauth2.Endpoint
	passkeyRegistrationFactory passkeyRegistrationFactory
	passkeyAssertionFactory    passkeyAssertionFactory
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
		if dependencies[0].BucketHashKey != nil {
			deps.BucketHashKey = append([]byte(nil), dependencies[0].BucketHashKey...)
		}
		if dependencies[0].GoogleIDTokenVerifier != nil {
			deps.GoogleIDTokenVerifier = dependencies[0].GoogleIDTokenVerifier
		}
		if dependencies[0].MicrosoftIDTokenVerifier != nil {
			deps.MicrosoftIDTokenVerifier = dependencies[0].MicrosoftIDTokenVerifier
		}
		if dependencies[0].OIDCIDTokenVerifier != nil {
			deps.OIDCIDTokenVerifier = dependencies[0].OIDCIDTokenVerifier
		}
	}
	if deps.GoogleIDTokenVerifier == nil && config != nil && config.GoogleLoginClient != nil {
		deps.GoogleIDTokenVerifier = newDiscoveredGoogleIDTokenVerifier(config.GoogleLoginClient.ClientID)
	}
	if deps.MicrosoftIDTokenVerifier == nil && config != nil && config.MicrosoftLoginClient != nil {
		deps.MicrosoftIDTokenVerifier = newDiscoveredMicrosoftIDTokenVerifier(
			config.MicrosoftLoginClient.ClientID, config.MicrosoftLoginTenant,
		)
	}
	if deps.OIDCIDTokenVerifier == nil && config != nil && config.OIDCLoginClient != nil {
		deps.OIDCIDTokenVerifier = newDiscoveredOIDCIDTokenVerifier(
			config.OIDCLoginIssuer, config.OIDCLoginClient.ClientID,
		)
	}
	return &Manager{
		config:                     config,
		db:                         db,
		clock:                      deps.Clock,
		tokens:                     deps.Tokens,
		bucketHashKey:              deps.BucketHashKey,
		googleIDTokenVerifier:      deps.GoogleIDTokenVerifier,
		microsoftIDTokenVerifier:   deps.MicrosoftIDTokenVerifier,
		oidcIDTokenVerifier:        deps.OIDCIDTokenVerifier,
		passkeyRegistrationFactory: newPasskeyRegistrationCeremony,
		passkeyAssertionFactory:    newPasskeyAssertionCeremony,
	}
}

func (m *Manager) Config() *Config {
	return m.config
}

func (m *Manager) IsEnabled() bool {
	return m.config.Enabled
}

func (m *Manager) HasGoogleLogin() bool {
	return m.config.GoogleLoginClient != nil
}

func (m *Manager) HasMicrosoftLogin() bool {
	return m.config.MicrosoftLoginClient != nil
}

func (m *Manager) HasOIDCLogin() bool {
	return m.config.OIDCLoginClient != nil
}

func (m *Manager) OIDCLoginName() string {
	if !m.HasOIDCLogin() {
		return ""
	}
	return normalizeOIDCLoginName(m.config.OIDCLoginName)
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
		`INSERT INTO users (id, email, email_normalized, name, status, user_type, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"default", "local@gofer.local", "local@gofer.local", "Local User", UserStatusActive, UserTypeWebmail, 0, now, now,
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
