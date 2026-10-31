package mailauth

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

func googleAccountScopes() []string {
	return []string{
		"openid",
		"email",
		"profile",
		"https://mail.google.com/",
		"https://www.googleapis.com/auth/contacts",
		GoogleCalendarReadOnlyScope,
		GoogleCalendarEventsScope,
		GoogleCalendarAppCreatedScope,
	}
}

// GoogleAccountOAuthURL is where a Google account signs in. fullCalendar also
// asks to manage all the person's calendars. Google keeps what was granted
// before, so an ordinary reconnect doesn't take that away again.
func (m *Service) GoogleAccountOAuthURL(state string, fullCalendar bool) string {
	cfg := m.accountOAuthConfig()
	if fullCalendar {
		cfg.Scopes = append(append([]string(nil), cfg.Scopes...), GoogleCalendarFullScope)
	}
	return cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.SetAuthURLParam("include_granted_scopes", "true"))
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
	return []string{"openid", "email", "profile", "offline_access", microsoftGraphContactsScope, microsoftGraphCalendarWriteScope, microsoftGraphMailScope, microsoftGraphMailSendScope, microsoftGraphMailboxSettingsScope}
}

func (m *Service) accountOAuthConfig() *oauth2.Config {
	cfg := m.config.GoogleClient
	return &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: m.config.BaseURL + "/auth/google/mailbox/callback", Scopes: cfg.Scopes, Endpoint: cfg.Endpoint}
}

func (m *Service) microsoftAccountOAuthConfig() *oauth2.Config {
	cfg := m.config.MicrosoftClient
	return &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: m.config.BaseURL + "/auth/microsoft/mailbox/callback", Scopes: cfg.Scopes, Endpoint: cfg.Endpoint}
}
