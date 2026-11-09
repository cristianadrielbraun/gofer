package mailauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
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

// ExchangeAccountCode redeems a Google sign-in for an account. It goes
// through the proxy ctx carries, the one of the account being connected, and
// binds the token to it, so the requests that follow take the same route.
func (m *Service) ExchangeAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	proxy := netguard.ProxyFrom(ctx)
	token, err := m.accountOAuthConfig().Exchange(withOAuthProxy(ctx, proxy), code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	netguard.BindBearerProxy(token.AccessToken, proxy)
	return token, nil
}

// ExchangeMicrosoftAccountCode is ExchangeAccountCode for Microsoft.
func (m *Service) ExchangeMicrosoftAccountCode(ctx context.Context, code string) (*oauth2.Token, error) {
	proxy := netguard.ProxyFrom(ctx)
	token, err := m.microsoftAccountOAuthConfig().Exchange(withOAuthProxy(ctx, proxy), code, oauth2.SetAuthURLParam("scope", strings.Join(microsoftAccountTokenExchangeScopes(), " ")))
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	netguard.BindBearerProxy(token.AccessToken, proxy)
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
