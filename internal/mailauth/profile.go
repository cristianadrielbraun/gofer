package mailauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type GoogleAccountInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

type MicrosoftAccountInfo struct {
	Sub               string `json:"sub"`
	ObjectID          string `json:"oid"`
	TenantID          string `json:"tid"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	Name              string `json:"name"`
}

func (i MicrosoftAccountInfo) EmailAddress() string {
	for _, value := range []string{i.Email, i.PreferredUsername, i.UPN} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (i MicrosoftAccountInfo) ProviderAccountID() string {
	if subject := strings.TrimSpace(i.Sub); subject != "" {
		return subject
	}
	if tenant, object := strings.TrimSpace(i.TenantID), strings.TrimSpace(i.ObjectID); tenant != "" && object != "" {
		return tenant + ":" + object
	}
	return strings.TrimSpace(i.ObjectID)
}

func (m *Service) GetGoogleAccountInfo(ctx context.Context, token *oauth2.Token) (*GoogleAccountInfo, error) {
	client := m.config.GoogleClient.Client(ctx, token)
	resp, err := client.Get("https://openidconnect.googleapis.com/v1/userinfo")
	if err != nil {
		return nil, fmt.Errorf("fetch userinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, string(body))
	}

	var info GoogleAccountInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	return &info, nil
}

func (m *Service) GetMicrosoftAccountInfo(ctx context.Context, token *oauth2.Token) (*MicrosoftAccountInfo, error) {
	_ = ctx
	idToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(idToken) == "" {
		return nil, fmt.Errorf("microsoft id token not returned")
	}
	return microsoftAccountInfoFromIDToken(idToken, m.config.MicrosoftClient.ClientID, time.Now())
}

func microsoftAccountInfoFromIDToken(idToken, clientID string, now time.Time) (*MicrosoftAccountInfo, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid microsoft id token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode microsoft id token: %w", err)
	}

	var claims struct {
		MicrosoftAccountInfo
		Audience any   `json:"aud"`
		Expires  int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse microsoft id token: %w", err)
	}
	if clientID != "" && !microsoftAudienceMatches(claims.Audience, clientID) {
		return nil, fmt.Errorf("microsoft id token audience mismatch")
	}
	if claims.Expires > 0 && !now.IsZero() && time.Unix(claims.Expires, 0).Before(now.Add(-time.Minute)) {
		return nil, fmt.Errorf("microsoft id token expired")
	}

	info := claims.MicrosoftAccountInfo
	if info.ProviderAccountID() == "" {
		return nil, fmt.Errorf("microsoft id token missing subject")
	}
	if info.EmailAddress() == "" {
		return nil, fmt.Errorf("microsoft id token missing email")
	}
	return &info, nil
}

func microsoftAudienceMatches(audience any, clientID string) bool {
	switch value := audience.(type) {
	case string:
		return value == clientID
	case []any:
		for _, item := range value {
			if candidate, ok := item.(string); ok && candidate == clientID {
				return true
			}
		}
	}
	return false
}
