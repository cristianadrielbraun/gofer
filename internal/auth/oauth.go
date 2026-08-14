package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
)

type GoogleUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
}

func (m *Manager) GoogleLoginOAuthURL(state string) string {
	return m.config.GoogleLoginClient.AuthCodeURL(state)
}

func (m *Manager) ExchangeGoogleLoginCode(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := m.config.GoogleLoginClient.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return token, nil
}

func (m *Manager) GetGoogleLoginUserInfo(ctx context.Context, token *oauth2.Token) (*GoogleUserInfo, error) {
	client := m.config.GoogleLoginClient.Client(ctx, token)
	resp, err := client.Get("https://openidconnect.googleapis.com/v1/userinfo")
	if err != nil {
		return nil, fmt.Errorf("fetch userinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, string(body))
	}

	var info GoogleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}

	return &info, nil
}

func (m *Manager) HandleGoogleCallback(ctx context.Context, code string, userAgent string) (*User, *PrimaryAuthenticationResult, error) {
	token, err := m.ExchangeGoogleLoginCode(ctx, code)
	if err != nil {
		return nil, nil, err
	}

	info, err := m.GetGoogleLoginUserInfo(ctx, token)
	if err != nil {
		return nil, nil, err
	}

	if !info.EmailVerified {
		return nil, nil, fmt.Errorf("email not verified")
	}

	user, err := m.CreateOrUpdateUser(ctx, info.Email, info.Name, info.Picture)
	if err != nil {
		return nil, nil, err
	}

	result, err := m.completeFederatedPrimaryAuthentication(
		ctx, user.ID, userAgent, AuthenticationMethodFederatedGoogle,
	)
	if err != nil {
		return nil, nil, err
	}

	return user, result, nil
}
