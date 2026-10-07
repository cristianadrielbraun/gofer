package mailauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/retry"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type userCredentialPurpose int

const (
	userCredentialMailbox userCredentialPurpose = iota
	userCredentialContacts
	userCredentialCalendarRead
	userCredentialCalendarWrite
)

// Known grants and cached access-token scopes are different for a scoped Graph
// refresh. Only recorded/returned scopes are retained; requests do not imply a
// grant. Explicit reconnect replaces the known authorization snapshot.
// Microsoft Graph uses both resource-qualified and bare permission names.
// Only known bare Graph permissions are aliases; another resource URI stays
// distinct. See Microsoft's scopes-oidc reference for the Graph default resource.
func ownedScopeKey(scope string) string {
	key := strings.ToLower(scope)
	switch key {
	case "contacts.readwrite", "calendars.read", "calendars.readwrite", "mail.readwrite", "mail.send", "mailboxsettings.readwrite":
		return "https://graph.microsoft.com/" + key
	default:
		return key
	}
}
func ownedGraphHasScopes(record string, expected ...string) bool {
	seen := make(map[string]bool)
	for _, scope := range strings.Fields(record) {
		seen[ownedScopeKey(scope)] = true
	}
	for _, scope := range expected {
		key := ownedScopeKey(scope)
		if key == ownedScopeKey(microsoftGraphCalendarScope) && seen[ownedScopeKey(microsoftGraphCalendarWriteScope)] {
			continue
		}
		if !seen[key] {
			return false
		}
	}
	return true
}

func mergeKnownScopes(values ...string) string {
	seen := make(map[string]bool)
	var scopes []string
	for _, value := range values {
		for _, scope := range strings.Fields(value) {
			key := ownedScopeKey(scope)
			if !seen[key] {
				seen[key] = true
				scopes = append(scopes, scope)
			}
		}
	}
	return strings.Join(scopes, " ")
}
func googleCalendarReadAllowed(scopes string) bool {
	return recordHasScopes(scopes, GoogleCalendarReadOnlyScope) || recordHasScopes(scopes, "https://www.googleapis.com/auth/calendar")
}
func googleCalendarWriteAllowed(scopes string) bool {
	return recordHasScopes(scopes, GoogleCalendarEventsScope) || recordHasScopes(scopes, "https://www.googleapis.com/auth/calendar")
}
func calendarWriteAllowed(record userCredentialRecord, provider string) bool {
	switch provider {
	case providers.ProviderGmail:
		return record.Provider == providers.OAuthGoogle && googleCalendarWriteAllowed(record.grantedScopes)
	case providers.ProviderOutlook:
		return record.Provider == providers.OAuthMicrosoft && ownedGraphHasScopes(record.grantedScopes, microsoftGraphCalendarWriteScope)
	default:
		return false
	}
}

// CalendarWriteAuthorizedForUser is a local grant check. It neither refreshes
// credentials nor waits behind a token request for this account.
func (s *UserCredentials) CalendarWriteAuthorizedForUser(ctx context.Context, owner, id, provider string) bool {
	allowed := false
	err := s.operation(ctx, owner, id, false, func(ctx context.Context) error {
		record, err := s.load(ctx, owner, id)
		if err == nil {
			allowed = calendarWriteAllowed(record, provider)
		}
		return err
	})
	return err == nil && allowed
}
func (s *UserCredentials) HasGoogleOAuth() bool    { return s != nil && s.codec.HasGoogleOAuth() }
func (s *UserCredentials) HasMicrosoftOAuth() bool { return s != nil && s.codec.HasMicrosoftOAuth() }

func (s *UserCredentials) GetMicrosoftGraphContactsTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.serviceToken(ctx, owner, id, userCredentialContacts, providers.OAuthMicrosoft, false)
}
func (s *UserCredentials) GetMicrosoftGraphCalendarTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.serviceToken(ctx, owner, id, userCredentialCalendarRead, providers.OAuthMicrosoft, false)
}
func (s *UserCredentials) GetMicrosoftGraphCalendarWriteTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.serviceToken(ctx, owner, id, userCredentialCalendarWrite, providers.OAuthMicrosoft, false)
}
func (s *UserCredentials) GetGoogleCalendarTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.serviceToken(ctx, owner, id, userCredentialCalendarRead, providers.OAuthGoogle, false)
}
func (s *UserCredentials) GetGoogleCalendarWriteTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.serviceToken(ctx, owner, id, userCredentialCalendarWrite, providers.OAuthGoogle, false)
}

// Service-specific Graph refreshes preserve the existing cached mailbox token,
// matching the legacy contacts/calendar helpers. Rotation and known grants are
// published with the same identity/reconnect fence as a mailbox refresh.
func (s *UserCredentials) publishServiceRefresh(ctx context.Context, record userCredentialRecord, token *oauth2.Token) error {
	return s.withIdentity(ctx, record.owner, record.AccountID, true, func(identity userCredentialIdentity) error {
		if identity.provider != record.Provider || identity.subject != record.ProviderAccountID {
			return ErrMailboxAuthorizationChanged
		}
		var refresh []byte
		var err error
		if token.RefreshToken != "" {
			refresh, err = s.codec.encryptOAuthToken(record.oauthCredentialContext, "refresh", token.RefreshToken)
			if err != nil {
				return err
			}
		}
		observed, _ := token.Extra("scope").(string)
		granted := mergeKnownScopes(record.grantedScopes, observed)
		result, err := s.routing.System().Write().ExecContext(ctx, `UPDATE gofer_mailbox_credentials
   SET refresh_token_ciphertext=CASE WHEN ? IS NULL THEN refresh_token_ciphertext ELSE ? END,
   granted_scopes=?,revision=revision+1,updated_at=CURRENT_TIMESTAMP
   WHERE id=? AND account_id=? AND user_id=? AND provider=? AND provider_account_id=? AND revision=?`, refresh, refresh, granted, record.ID, record.AccountID, record.owner, record.Provider, record.ProviderAccountID, record.revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrMailboxAuthorizationChanged
		}
		return nil
	})
}

func (s *UserCredentials) serviceToken(ctx context.Context, owner, id string, purpose userCredentialPurpose, expectedProvider string, force bool) (string, error) {
	authorization, err := s.serviceAuthorization(ctx, owner, id, purpose, expectedProvider, force)
	if err != nil {
		return "", err
	}
	return authorization.Token(), nil
}

func (s *UserCredentials) serviceAuthorization(ctx context.Context, owner, id string, purpose userCredentialPurpose, expectedProvider string, force bool) (authorization *UserServiceAuthorization, err error) {
	return s.serviceAuthorizationFrom(ctx, owner, id, purpose, expectedProvider, force, nil)
}

func (s *UserCredentials) serviceAuthorizationFrom(ctx context.Context, owner, id string, purpose userCredentialPurpose, expectedProvider string, force bool, expected *UserServiceAuthorization) (authorization *UserServiceAuthorization, err error) {
	err = s.operation(ctx, owner, id, true, func(ctx context.Context) error {
		record, err := s.load(ctx, owner, id)
		if err != nil {
			return err
		}
		// This check runs after acquiring the credential gate. A reconnect in
		// the wait between validation and refresh cannot authorize a new grant.
		if expected != nil && (expected.repository != s || expected.owner != owner || expected.account != id || expected.purpose != purpose ||
			expected.id != record.ID || expected.provider != record.Provider || expected.subject != record.ProviderAccountID || expected.revision != record.revision) {
			return ErrMailboxAuthorizationChanged
		}
		if expectedProvider != "" && record.Provider != expectedProvider {
			return errors.New("mailbox provider does not match the requested service")
		}
		if purpose == userCredentialCalendarWrite {
			provider := providers.ProviderGmail
			if record.Provider == providers.OAuthMicrosoft {
				provider = providers.ProviderOutlook
			}
			if !calendarWriteAllowed(record, provider) {
				return errors.New("calendar event writes are not authorized; reconnect this account to grant write access")
			}
		}
		if purpose == userCredentialCalendarRead && record.Provider == providers.OAuthGoogle && !googleCalendarReadAllowed(record.grantedScopes) {
			return errors.New("Google Calendar reads are not authorized; reconnect this account to grant Calendar access")
		}
		var scopes []string
		if record.Provider == providers.OAuthMicrosoft {
			switch purpose {
			case userCredentialContacts:
				scopes = []string{microsoftGraphContactsScope}
			case userCredentialCalendarRead:
				scopes = []string{microsoftGraphCalendarScope}
			case userCredentialCalendarWrite:
				scopes = []string{microsoftGraphCalendarWriteScope}
			default:
				return errors.New("unknown credential service")
			}
		}
		cached := ownedGraphHasScopes(record.Scopes, scopes...)
		if record.Provider == providers.OAuthGoogle {
			if purpose == userCredentialCalendarRead {
				cached = googleCalendarReadAllowed(record.Scopes)
			}
			if purpose == userCredentialCalendarWrite {
				cached = googleCalendarWriteAllowed(record.Scopes)
			}
		}
		if !force && cached && record.AccessToken != "" && record.ExpiresAt.Valid && record.ExpiresAt.Time.After(time.Now().Add(5*time.Minute)) {
			authorization = s.authorizationFor(record, purpose, record.AccessToken)
			return nil
		}
		if record.RefreshToken == "" {
			return errors.New("mailbox authorization has no refresh token")
		}
		cfg, err := s.codec.oauthConfigForProvider(record.Provider)
		if err != nil {
			return err
		}
		var token *oauth2.Token
		if record.Provider == providers.OAuthMicrosoft {
			token, err = refreshTokenForScopes(ctx, cfg, record.RefreshToken, scopes)
		} else {
			token, err = cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: record.RefreshToken, TokenType: "Bearer"}).Token()
			var response *oauth2.RetrieveError
			if errors.As(err, &response) && response.Response != nil {
				err = oauthRefreshError(response)
			}
		}
		if err != nil {
			return fmt.Errorf("refresh service authorization: %w", err)
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return errors.New("provider returned an empty service access token")
		}
		if record.Provider == providers.OAuthMicrosoft {
			err = s.publishServiceRefresh(ctx, record, token)
		} else {
			err = s.publish(ctx, record, token)
		}
		if err != nil {
			return err
		}
		observed, _ := token.Extra("scope").(string)
		if record.Provider == providers.OAuthMicrosoft && strings.TrimSpace(observed) != "" && !ownedGraphHasScopes(observed, scopes...) {
			return errors.New("refreshed Graph authorization does not contain the requested service scope")
		}
		if record.Provider == providers.OAuthGoogle && strings.TrimSpace(observed) != "" {
			if purpose == userCredentialCalendarRead && !googleCalendarReadAllowed(observed) {
				return errors.New("refreshed Google authorization does not allow Calendar reads")
			}
			if purpose == userCredentialCalendarWrite && !googleCalendarWriteAllowed(observed) {
				return errors.New("refreshed Google authorization does not allow Calendar writes")
			}
		}
		record.revision++ // Both credential publications increment the matched revision once.
		authorization = s.authorizationFor(record, purpose, token.AccessToken)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return authorization, nil
}

func (s *UserCredentials) ContactsAccount(owner, id string) *UserAccountCredentials {
	return &UserAccountCredentials{credentials: s, owner: owner, id: id, purpose: userCredentialContacts}
}
func (s *UserCredentials) CalendarAccount(owner, id string, write bool) *UserAccountCredentials {
	purpose := userCredentialCalendarRead
	if write {
		purpose = userCredentialCalendarWrite
	}
	return &UserAccountCredentials{credentials: s, owner: owner, id: id, purpose: purpose}
}
func (s *UserAccountCredentials) service(ctx context.Context, id string, purpose userCredentialPurpose, provider string) (string, error) {
	if id != s.id {
		return "", storage.ErrAccountRoute
	}
	return s.credentials.serviceToken(ctx, s.owner, id, purpose, provider, false)
}
func (s *UserAccountCredentials) GetMicrosoftGraphContactsTokenForAccount(ctx context.Context, id string) (string, error) {
	return s.service(ctx, id, userCredentialContacts, providers.OAuthMicrosoft)
}
func (s *UserAccountCredentials) GetMicrosoftGraphCalendarTokenForAccount(ctx context.Context, id string) (string, error) {
	return s.service(ctx, id, userCredentialCalendarRead, providers.OAuthMicrosoft)
}
func (s *UserAccountCredentials) GetMicrosoftGraphCalendarWriteTokenForAccount(ctx context.Context, id string) (string, error) {
	return s.service(ctx, id, userCredentialCalendarWrite, providers.OAuthMicrosoft)
}
func (s *UserAccountCredentials) GetGoogleCalendarTokenForAccount(ctx context.Context, id string) (string, error) {
	return s.service(ctx, id, userCredentialCalendarRead, providers.OAuthGoogle)
}
func (s *UserAccountCredentials) GetGoogleCalendarWriteTokenForAccount(ctx context.Context, id string) (string, error) {
	return s.service(ctx, id, userCredentialCalendarWrite, providers.OAuthGoogle)
}
func (s *UserAccountCredentials) CalendarWriteAuthorized(ctx context.Context, id, provider string) bool {
	return id == s.id && s.credentials.CalendarWriteAuthorizedForUser(ctx, s.owner, id, provider)
}
func (s *UserAccountCredentials) HasGoogleOAuth() bool    { return s.credentials.HasGoogleOAuth() }
func (s *UserAccountCredentials) HasMicrosoftOAuth() bool { return s.credentials.HasMicrosoftOAuth() }

func oauthRefreshError(response *oauth2.RetrieveError) error {
	at, _ := retry.ParseRetryAfter(response.Response.Header.Get("Retry-After"), time.Now().UTC())
	return &OAuthTokenError{Status: response.Response.StatusCode, Code: response.ErrorCode, RetryAt: at}
}
