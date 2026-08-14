package mailauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Config struct {
	Enabled         bool
	BaseURL         string
	GoogleClient    *oauth2.Config
	MicrosoftClient *oauth2.Config
}

func LoadConfig(baseURL string, authenticationEnabled bool) *Config {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://local.localhost:8090"
	}
	cfg := &Config{Enabled: authenticationEnabled, BaseURL: baseURL}

	googleClientID := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_ID"))
	googleClientSecret := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"))
	if googleClientID != "" && googleClientSecret != "" {
		cfg.GoogleClient = &oauth2.Config{
			ClientID:     googleClientID,
			ClientSecret: googleClientSecret,
			RedirectURL:  baseURL + "/auth/google/account/callback",
			Scopes:       googleAccountScopes(),
			Endpoint:     google.Endpoint,
		}
	}

	microsoftClientID := strings.TrimSpace(os.Getenv("MICROSOFT_OAUTH_CLIENT_ID"))
	microsoftClientSecret := strings.TrimSpace(os.Getenv("MICROSOFT_OAUTH_CLIENT_SECRET"))
	if microsoftClientID != "" && microsoftClientSecret != "" {
		cfg.MicrosoftClient = &oauth2.Config{
			ClientID:     microsoftClientID,
			ClientSecret: microsoftClientSecret,
			RedirectURL:  baseURL + "/auth/microsoft/account/callback",
			Scopes:       microsoftAccountTokenScopes(),
			Endpoint:     microsoftEndpoint(os.Getenv("MICROSOFT_OAUTH_TENANT")),
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
	return oauth2.Endpoint{AuthURL: base + "/authorize", TokenURL: base + "/token"}
}

type Service struct {
	config *Config
	db     *storage.DB
}

// Manager remains an alias while callers migrate to the service terminology.
type Manager = Service

func New(config *Config, db *storage.DB) *Service {
	if config == nil {
		config = &Config{}
	}
	return &Service{config: config, db: db}
}

func NewManager(config *Config, db *storage.DB) *Service {
	return New(config, db)
}

func (m *Service) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.CleanupExpiredAccountOAuthFlows(ctx); err != nil {
					log.Printf("account OAuth flow cleanup error: %v", err)
				}
			}
		}
	}()
}

func (m *Service) HasGoogleOAuth() bool { return m != nil && m.config.GoogleClient != nil }

func (m *Service) HasMicrosoftOAuth() bool { return m != nil && m.config.MicrosoftClient != nil }

func (m *Service) GenerateState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Service) UpsertOAuthAccount(ctx context.Context, userID, provider, providerAccountID, accessToken, refreshToken, tokenType string, expiresAt *time.Time, scopes string) error {
	now := time.Now()
	var existingID string
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`, provider, providerAccountID,
	).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup oauth account: %w", err)
	}
	if existingID != "" {
		_, err = m.db.Write().ExecContext(ctx,
			`UPDATE oauth_accounts SET user_id = ?, access_token = ?, refresh_token = COALESCE(NULLIF(?, ''), refresh_token), token_type = ?, expires_at = ?, scopes = ?, updated_at = ? WHERE id = ?`,
			userID, accessToken, refreshToken, tokenType, expiresAt, scopes, now, existingID)
		return err
	}
	_, err = m.db.Write().ExecContext(ctx,
		`INSERT INTO oauth_accounts (id, user_id, provider, provider_account_id, access_token, refresh_token, token_type, expires_at, scopes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), userID, provider, providerAccountID, accessToken, refreshToken, tokenType, expiresAt, scopes, now, now)
	return err
}
