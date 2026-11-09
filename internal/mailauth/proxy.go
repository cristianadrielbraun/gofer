package mailauth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"golang.org/x/oauth2"
)

// AccountProxyResolver returns the proxy an account connects through, or nil
// for direct connections.
type AccountProxyResolver func(ctx context.Context, accountID string) (*netguard.Proxy, error)

// UserAccountProxyResolver is AccountProxyResolver for an account in its
// owner's own database.
type UserAccountProxyResolver func(ctx context.Context, owner, accountID string) (*netguard.Proxy, error)

// SetAccountProxyResolver makes token refreshes go through each account's
// proxy, and binds the tokens handed out to it, so the account's API requests
// take the same route. Without a resolver every account connects directly.
func (m *Service) SetAccountProxyResolver(resolve AccountProxyResolver) {
	m.proxyFor = resolve
}

// SetAccountProxyResolver is Service.SetAccountProxyResolver for accounts in
// their owners' own databases.
func (s *UserCredentials) SetAccountProxyResolver(resolve UserAccountProxyResolver) {
	s.proxyFor = resolve
}

// routeAccountToken runs get, which returns one of the account's access
// tokens, with the token endpoint reached through the account's proxy, and
// binds the token to that proxy. An account whose proxy can't be read gets no
// token, rather than one its requests would carry out directly.
func (m *Service) routeAccountToken(ctx context.Context, accountID string, get func(context.Context) (string, error)) (string, error) {
	var proxy *netguard.Proxy
	if m.proxyFor != nil {
		var err error
		if proxy, err = m.proxyFor(ctx, accountID); err != nil {
			return "", fmt.Errorf("account %s proxy: %w", accountID, err)
		}
	}
	token, err := get(withOAuthProxy(ctx, proxy))
	if err == nil {
		netguard.BindBearerProxy(token, proxy)
	}
	return token, err
}

// accountProxy is the proxy of the owner's account, or nil without a
// resolver.
func (s *UserCredentials) accountProxy(ctx context.Context, owner, id string) (*netguard.Proxy, error) {
	if s.proxyFor == nil {
		return nil, nil
	}
	proxy, err := s.proxyFor(ctx, owner, id)
	if err != nil {
		return nil, fmt.Errorf("account %s proxy: %w", id, err)
	}
	return proxy, nil
}

// withOAuthProxy makes the oauth2 package, and refreshTokenForScopes, reach
// token endpoints through proxy, or directly when it is nil, never through a
// proxy from the environment. A client a caller already gave the oauth2
// package, as tests do, is kept.
func withOAuthProxy(ctx context.Context, proxy *netguard.Proxy) context.Context {
	ctx = netguard.WithProxy(ctx, proxy)
	if client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && client != nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, netguard.APIClient(0))
}

// oauthHTTPClient is the client the oauth2 package would use with ctx.
func oauthHTTPClient(ctx context.Context) *http.Client {
	if client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && client != nil {
		return client
	}
	return netguard.APIClient(0)
}
