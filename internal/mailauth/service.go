package mailauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
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
			RedirectURL:  baseURL + "/auth/google/mailbox/callback",
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
			RedirectURL:  baseURL + "/auth/microsoft/mailbox/callback",
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
	config        *Config
	db            *storage.DB
	credentialKey []byte
}

// Manager remains an alias while callers migrate to the service terminology.
type Manager = Service

func New(config *Config, db *storage.DB, credentialKey []byte) *Service {
	if config == nil {
		config = &Config{}
	}
	return &Service{config: config, db: db, credentialKey: append([]byte(nil), credentialKey...)}
}

func NewManager(config *Config, db *storage.DB, credentialKey []byte) *Service {
	return New(config, db, credentialKey)
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

func (m *Service) UpsertOAuthAccount(ctx context.Context, accountID, provider, providerAccountID, accessToken, refreshToken, tokenType string, expiresAt *time.Time, scopes string) error {
	now := time.Now()
	provider = strings.TrimSpace(provider)
	providerAccountID = strings.TrimSpace(providerAccountID)
	if strings.TrimSpace(accountID) == "" || providerAccountID == "" {
		return fmt.Errorf("mailbox account and provider identity are required")
	}
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mailbox OAuth credential upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var accountProvider, accountProviderID string
	if err := tx.QueryRowContext(ctx, `SELECT provider, provider_account_id FROM accounts WHERE id = ?`, accountID).Scan(
		&accountProvider, &accountProviderID,
	); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("mailbox account is unavailable")
	} else if err != nil {
		return fmt.Errorf("load mailbox account identity: %w", err)
	}
	wantOAuthProvider, err := oauthProviderForAccountProvider(accountProvider)
	if err != nil || wantOAuthProvider != provider || strings.TrimSpace(accountProviderID) != providerAccountID {
		return fmt.Errorf("mailbox OAuth credential does not match its account")
	}

	var accountCredentialID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM oauth_accounts WHERE account_id = ?`, accountID).Scan(&accountCredentialID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup mailbox OAuth credential by account: %w", err)
	}
	var providerCredentialID, providerCredentialAccountID string
	err = tx.QueryRowContext(ctx, `
		SELECT id, account_id FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`,
		provider, providerAccountID,
	).Scan(&providerCredentialID, &providerCredentialAccountID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup mailbox OAuth credential by provider identity: %w", err)
	}
	if providerCredentialID != "" && providerCredentialAccountID != accountID {
		return fmt.Errorf("mailbox provider identity is already assigned to another account")
	}
	if accountCredentialID != "" && providerCredentialID != "" && accountCredentialID != providerCredentialID {
		return fmt.Errorf("mailbox account has a conflicting OAuth credential")
	}
	credentialID := accountCredentialID
	if credentialID == "" {
		credentialID = providerCredentialID
	}
	existingCredential := credentialID != ""
	if !existingCredential {
		credentialID = uuid.NewString()
	}
	credential := oauthCredentialContext{
		ID: credentialID, AccountID: accountID, Provider: provider, ProviderAccountID: providerAccountID,
	}
	accessCiphertext, err := m.encryptOAuthToken(credential, "access", accessToken)
	if err != nil {
		return err
	}
	var refreshCiphertext []byte
	if refreshToken != "" {
		refreshCiphertext, err = m.encryptOAuthToken(credential, "refresh", refreshToken)
		if err != nil {
			return err
		}
	}
	if existingCredential {
		result, err := tx.ExecContext(ctx, `
			UPDATE oauth_accounts
			SET account_id = ?, provider = ?, provider_account_id = ?,
			    access_token = '', refresh_token = '', access_token_ciphertext = ?,
			    refresh_token_ciphertext = CASE WHEN ? IS NULL THEN refresh_token_ciphertext ELSE ? END,
			    key_version = ?, token_type = ?, expires_at = ?, scopes = ?, updated_at = ?
			WHERE id = ?`,
			accountID, provider, providerAccountID, accessCiphertext,
			refreshCiphertext, refreshCiphertext, mailboxCredentialKeyVersion,
			tokenType, expiresAt, scopes, now, credentialID,
		)
		if err != nil {
			return fmt.Errorf("update mailbox OAuth credential: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check mailbox OAuth credential update: %w", err)
		}
		if changed == 1 {
			return tx.Commit()
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO oauth_accounts (
			id, account_id, provider, provider_account_id,
			access_token, refresh_token, access_token_ciphertext, refresh_token_ciphertext,
			key_version, token_type, expires_at, scopes, created_at, updated_at
		) VALUES (?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		credentialID, accountID, provider, providerAccountID,
		accessCiphertext, refreshCiphertext, mailboxCredentialKeyVersion,
		tokenType, expiresAt, scopes, now, now,
	)
	if err != nil {
		return fmt.Errorf("insert mailbox OAuth credential: %w", err)
	}
	return tx.Commit()
}
