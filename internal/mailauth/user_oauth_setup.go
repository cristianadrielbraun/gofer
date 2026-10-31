package mailauth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"golang.org/x/oauth2"
)

// WithMailboxSetup serializes a user's callbacks without leasing a mailbox DB.
// Two grants for the same identity must converge on one local account, including
// when an earlier callback saved the account but failed to save its grant.
func (s *UserCredentials) WithMailboxSetup(ctx context.Context, owner string, fn func(context.Context) error) error {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return err
	}
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	key := "setup:" + owner
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return errors.New("mailbox credentials are shutting down")
	}
	s.operations.Add(1)
	gate := s.gates[key]
	if gate == nil {
		gate = &userCredentialGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		s.gates[key] = gate
	}
	gate.refs++
	s.mu.Unlock()
	defer s.operations.Done()
	defer func() {
		s.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.gates, key)
		}
		s.mu.Unlock()
	}()
	select {
	case <-work.Done():
		return work.Err()
	case <-gate.token:
	}
	defer func() { gate.token <- struct{}{} }()
	if err := s.routing.ValidateUser(work, owner); err != nil {
		return err
	}
	return fn(work)
}

// AccountAuthorizationURL is where an account of the provider signs in.
// fullCalendar asks a Google account to let Gofer manage all its calendars.
func (s *UserCredentials) AccountAuthorizationURL(provider, state string, fullCalendar bool) (string, error) {
	switch provider {
	case providers.ProviderGmail:
		if s.codec.HasGoogleOAuth() {
			return s.codec.GoogleAccountOAuthURL(state, fullCalendar), nil
		}
	case providers.ProviderOutlook:
		if s.codec.HasMicrosoftOAuth() {
			return s.codec.MicrosoftAccountOAuthURL(state), nil
		}
	}
	return "", errors.New("mailbox OAuth provider is not configured")
}

func (s *UserCredentials) CreateAccountOAuthFlow(ctx context.Context, owner, session, provider string, form map[string]string) (string, error) {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return "", err
	}
	// Routed mode always requires a browser session, even if the OAuth service
	// was initialized without its legacy Enabled flag.
	if strings.TrimSpace(session) == "" {
		return "", errors.New("authenticated session required")
	}
	return s.codec.CreateAccountOAuthFlow(ctx, owner, session, provider, form)
}

func (s *UserCredentials) ConsumeAccountOAuthFlow(ctx context.Context, state, owner, session, provider string) (*AccountOAuthFlow, error) {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	if strings.TrimSpace(session) == "" {
		return nil, ErrAccountOAuthFlowSessionMismatch
	}
	return s.codec.ConsumeAccountOAuthFlow(ctx, state, owner, session, provider)
}

// ExchangeMailboxCode performs provider HTTP outside local database leases.
// The caller must bind the result to the consumed session/owner and save the
// grant with UpsertForUser before starting the mailbox worker.
func (s *UserCredentials) ExchangeMailboxCode(ctx context.Context, provider, code string) (*models.CreateAccountRequest, *oauth2.Token, error) {
	if _, err := s.AccountAuthorizationURL(provider, "", false); err != nil {
		return nil, nil, err
	}
	var token *oauth2.Token
	var err error
	switch provider {
	case providers.ProviderGmail:
		token, err = s.codec.ExchangeAccountCode(ctx, code)
	case providers.ProviderOutlook:
		token, err = s.codec.ExchangeMicrosoftAccountCode(ctx, code)
	}
	if err != nil {
		return nil, nil, err
	}
	return s.MailboxGrant(ctx, provider, token)
}

// MailboxGrant identifies the mailbox a provider token belongs to, whether
// the token came from a redirect code or a device sign-in.
func (s *UserCredentials) MailboxGrant(ctx context.Context, provider string, token *oauth2.Token) (*models.CreateAccountRequest, *oauth2.Token, error) {
	var req *models.CreateAccountRequest
	var err error
	switch provider {
	case providers.ProviderGmail:
		var info *GoogleAccountInfo
		info, err = s.codec.GetGoogleAccountInfo(ctx, token)
		if err == nil {
			req = providers.GmailAccountRequest(info.Email, info.Name, info.Sub)
		}
	case providers.ProviderOutlook:
		var info *MicrosoftAccountInfo
		info, err = s.codec.GetMicrosoftAccountInfo(ctx, token)
		if err == nil {
			req = providers.OutlookAccountRequest(info.EmailAddress(), info.Name, info.ProviderAccountID())
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if req == nil || strings.TrimSpace(req.EmailAddress) == "" || strings.TrimSpace(req.ProviderAccountID) == "" || token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, nil, errors.New("mailbox authorization has incomplete identity or credentials")
	}
	if req.DisplayName == "" {
		req.DisplayName = req.EmailAddress
	}
	return req, token, nil
}

func (s *UserCredentials) UsesMicrosoftDeviceCode() bool {
	return s != nil && s.codec.UsesMicrosoftDeviceCode()
}

func (s *UserCredentials) StartMicrosoftDeviceSignIn(ctx context.Context, state, owner, session string) (*MicrosoftDeviceSignIn, error) {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	if strings.TrimSpace(session) == "" {
		return nil, errors.New("authenticated session required")
	}
	return s.codec.StartMicrosoftDeviceSignIn(ctx, state, owner, session)
}

func (s *UserCredentials) MicrosoftDeviceSignIn(ctx context.Context, state, owner, session string) (*MicrosoftDeviceSignIn, error) {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	return s.codec.MicrosoftDeviceSignIn(ctx, state, owner, session)
}

func (s *UserCredentials) PollMicrosoftDeviceSignIn(ctx context.Context, state, owner, session string) (*oauth2.Token, error) {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	return s.codec.PollMicrosoftDeviceSignIn(ctx, state, owner, session)
}
