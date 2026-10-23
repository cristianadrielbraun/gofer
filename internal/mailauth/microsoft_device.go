package mailauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Device code sign-in connects Outlook without a redirect back to Gofer:
// Microsoft shows the user a short code to enter on its own page while Gofer
// waits for the grant. It needs neither a client secret nor a public address.

const (
	microsoftDeviceCodeGrant       = "urn:ietf:params:oauth:grant-type:device_code"
	microsoftDeviceDefaultInterval = 5 * time.Second
	microsoftDeviceSlowDownStep    = 5 * time.Second
)

var (
	ErrMicrosoftDevicePending  = errors.New("microsoft device sign-in is still pending")
	ErrMicrosoftDeviceDeclined = errors.New("microsoft device sign-in was declined")
	ErrMicrosoftDeviceExpired  = errors.New("microsoft device sign-in expired")
	errMicrosoftDeviceSlowDown = errors.New("microsoft device sign-in asked to slow down")
)

// MicrosoftDeviceSignIn is what the user needs to finish signing in at
// Microsoft. The device code itself never leaves the server.
type MicrosoftDeviceSignIn struct {
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	Interval        time.Duration
}

type microsoftDeviceFlow struct {
	MicrosoftDeviceSignIn
	deviceCode  string
	userID      string
	sessionHash string
	polling     bool
	nextPoll    time.Time
}

func (m *Service) UsesMicrosoftDeviceCode() bool {
	return m.HasMicrosoftOAuth() && m.config.MicrosoftDeviceCode
}

// StartMicrosoftDeviceSignIn asks Microsoft for a user code and keeps the
// device code under the account OAuth flow state that will consume it.
func (m *Service) StartMicrosoftDeviceSignIn(ctx context.Context, state, userID, sessionToken string) (*MicrosoftDeviceSignIn, error) {
	if !m.UsesMicrosoftDeviceCode() {
		return nil, errors.New("microsoft device sign-in is not configured")
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("microsoft device sign-in requires a flow state and user")
	}
	cfg := m.config.MicrosoftClient
	response, err := (&oauth2.Config{ClientID: cfg.ClientID, Scopes: cfg.Scopes, Endpoint: cfg.Endpoint}).DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("start microsoft device sign-in: %w", err)
	}
	if response.DeviceCode == "" || response.UserCode == "" || response.VerificationURI == "" {
		return nil, errors.New("microsoft device sign-in response is incomplete")
	}
	now := time.Now()
	// The account OAuth flow that finishes the connection expires first.
	expiresAt := now.Add(accountOAuthFlowLifetime)
	if !response.Expiry.IsZero() && response.Expiry.Before(expiresAt) {
		expiresAt = response.Expiry
	}
	interval := time.Duration(response.Interval) * time.Second
	if interval <= 0 {
		interval = microsoftDeviceDefaultInterval
	}
	flow := &microsoftDeviceFlow{
		MicrosoftDeviceSignIn: MicrosoftDeviceSignIn{
			UserCode: response.UserCode, VerificationURI: response.VerificationURI,
			ExpiresAt: expiresAt, Interval: interval,
		},
		deviceCode:  response.DeviceCode,
		userID:      userID,
		sessionHash: accountOAuthFlowSecretHash(sessionToken),
	}
	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	if m.deviceFlows == nil {
		m.deviceFlows = map[string]*microsoftDeviceFlow{}
	}
	for key, pending := range m.deviceFlows {
		if !now.Before(pending.ExpiresAt) {
			delete(m.deviceFlows, key)
		}
	}
	m.deviceFlows[accountOAuthFlowSecretHash(state)] = flow
	signIn := flow.MicrosoftDeviceSignIn
	return &signIn, nil
}

// MicrosoftDeviceSignIn returns the pending sign-in started by this user and
// browser session.
func (m *Service) MicrosoftDeviceSignIn(_ context.Context, state, userID, sessionToken string) (*MicrosoftDeviceSignIn, error) {
	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	_, flow, err := m.pendingMicrosoftDeviceFlow(state, userID, sessionToken)
	if err != nil {
		return nil, err
	}
	signIn := flow.MicrosoftDeviceSignIn
	return &signIn, nil
}

// PollMicrosoftDeviceSignIn asks Microsoft once whether the user finished.
// Early or overlapping polls report pending without contacting Microsoft, so
// the browser's polling pace cannot exceed the interval Microsoft set.
func (m *Service) PollMicrosoftDeviceSignIn(ctx context.Context, state, userID, sessionToken string) (*oauth2.Token, error) {
	m.deviceMu.Lock()
	key, flow, err := m.pendingMicrosoftDeviceFlow(state, userID, sessionToken)
	if err != nil {
		m.deviceMu.Unlock()
		return nil, err
	}
	if flow.polling || time.Now().Before(flow.nextPoll) {
		m.deviceMu.Unlock()
		return nil, ErrMicrosoftDevicePending
	}
	flow.polling = true
	m.deviceMu.Unlock()

	token, err := m.redeemMicrosoftDeviceCode(ctx, flow.deviceCode)

	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	flow.polling = false
	switch {
	case errors.Is(err, errMicrosoftDeviceSlowDown):
		flow.Interval += microsoftDeviceSlowDownStep
		err = ErrMicrosoftDevicePending
	case errors.Is(err, ErrMicrosoftDevicePending):
	default:
		// Success and refusals both end the sign-in; a grant redeems once.
		delete(m.deviceFlows, key)
		return token, err
	}
	flow.nextPoll = time.Now().Add(flow.Interval)
	return nil, err
}

// pendingMicrosoftDeviceFlow requires deviceMu.
func (m *Service) pendingMicrosoftDeviceFlow(state, userID, sessionToken string) (string, *microsoftDeviceFlow, error) {
	key := accountOAuthFlowSecretHash(state)
	flow := m.deviceFlows[key]
	if strings.TrimSpace(state) == "" || flow == nil {
		return "", nil, ErrAccountOAuthFlowNotFound
	}
	if !time.Now().Before(flow.ExpiresAt) {
		delete(m.deviceFlows, key)
		return "", nil, ErrMicrosoftDeviceExpired
	}
	if subtle.ConstantTimeCompare([]byte(flow.userID), []byte(strings.TrimSpace(userID))) != 1 {
		return "", nil, ErrAccountOAuthFlowUserMismatch
	}
	if subtle.ConstantTimeCompare([]byte(flow.sessionHash), []byte(accountOAuthFlowSecretHash(sessionToken))) != 1 {
		return "", nil, ErrAccountOAuthFlowSessionMismatch
	}
	return key, flow, nil
}

func (m *Service) redeemMicrosoftDeviceCode(ctx context.Context, deviceCode string) (*oauth2.Token, error) {
	cfg := m.config.MicrosoftClient
	values := url.Values{}
	values.Set("grant_type", microsoftDeviceCodeGrant)
	values.Set("client_id", cfg.ClientID)
	values.Set("device_code", deviceCode)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := http.DefaultClient
	if contextClient, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && contextClient != nil {
		client = contextClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll microsoft device sign-in: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read microsoft device sign-in: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &failure)
		switch strings.TrimSpace(failure.Code) {
		case "authorization_pending":
			return nil, ErrMicrosoftDevicePending
		case "slow_down":
			return nil, errMicrosoftDeviceSlowDown
		case "authorization_declined", "access_denied":
			return nil, ErrMicrosoftDeviceDeclined
		case "expired_token", "code_expired":
			return nil, ErrMicrosoftDeviceExpired
		}
		return nil, &OAuthTokenError{Status: resp.StatusCode, Code: strings.TrimSpace(failure.Code), Description: strings.TrimSpace(failure.Description)}
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("parse microsoft device sign-in: %w", err)
	}
	var grant struct {
		AccessToken  string      `json:"access_token"`
		TokenType    string      `json:"token_type"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &grant); err != nil {
		return nil, fmt.Errorf("parse microsoft device sign-in: %w", err)
	}
	if strings.TrimSpace(grant.AccessToken) == "" {
		return nil, errors.New("microsoft device sign-in returned no access token")
	}
	token := &oauth2.Token{AccessToken: grant.AccessToken, TokenType: grant.TokenType, RefreshToken: grant.RefreshToken}
	if seconds, err := grant.ExpiresIn.Int64(); err == nil && seconds > 0 {
		token.Expiry = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	return token.WithExtra(fields), nil
}
