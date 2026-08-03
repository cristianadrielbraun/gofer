package mailauth

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

func (m *Service) GoogleAccountOAuthURL(state string) string {
	return m.accountOAuthConfig().AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (m *Service) MicrosoftAccountOAuthURL(state string) string {
	return m.microsoftAccountOAuthConfig().AuthCodeURL(state, oauth2.SetAuthURLParam("prompt", "consent"))
}

func (m *Service) ExchangeAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.accountOAuthConfig().Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func (m *Service) ExchangeMicrosoftAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.microsoftAccountOAuthConfig().Exchange(ctx, code, oauth2.SetAuthURLParam("scope", strings.Join(microsoftAccountTokenExchangeScopes(), " ")))
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func microsoftAccountTokenScopes() []string { return microsoftAccountTokenExchangeScopes() }

func microsoftAccountTokenExchangeScopes() []string {
	return []string{"openid", "email", "profile", "offline_access", microsoftGraphContactsScope, microsoftGraphMailScope, microsoftGraphMailSendScope, microsoftGraphMailboxSettingsScope}
}

func (m *Service) accountOAuthConfig() *oauth2.Config {
	cfg := m.config.GoogleClient
	return &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: m.config.BaseURL + "/auth/google/account/callback", Scopes: cfg.Scopes, Endpoint: cfg.Endpoint}
}

func (m *Service) microsoftAccountOAuthConfig() *oauth2.Config {
	cfg := m.config.MicrosoftClient
	return &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: m.config.BaseURL + "/auth/microsoft/account/callback", Scopes: cfg.Scopes, Endpoint: cfg.Endpoint}
}
