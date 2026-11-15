package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMCPTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	now := time.Now().UTC()
	token, secret, err := db.CreateMCPToken(ctx, MCPTokenInput{UserID: "default", Name: " Claude ", Scopes: []string{"mail.read", "mail.send"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, MCPTokenPrefix) || token.Name != "Claude" {
		t.Fatalf("token %+v secret %q", token, secret)
	}
	var stored string
	_ = db.Read().QueryRow(`SELECT token_hash FROM mcp_tokens WHERE id = ?`, token.ID).Scan(&stored)
	if stored == secret || stored != HashMCPToken(secret) {
		t.Fatal("the token itself is stored")
	}

	got, err := db.VerifyMCPToken(ctx, secret, now)
	if err != nil || got.ID != token.ID || got.UserID != "default" || strings.Join(got.Scopes, " ") != "mail.read mail.send" {
		t.Fatalf("verify = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "gofer_mcp_wrong", strings.TrimPrefix(secret, MCPTokenPrefix)} {
		if _, err := db.VerifyMCPToken(ctx, bad, now); err != ErrMCPTokenInvalid {
			t.Errorf("verify %q = %v", bad, err)
		}
	}

	past := now.Add(-time.Hour)
	_, expired, _ := db.CreateMCPToken(ctx, MCPTokenInput{UserID: "default", Name: "old", ExpiresAt: &past})
	if _, err := db.VerifyMCPToken(ctx, expired, now); err != ErrMCPTokenInvalid {
		t.Errorf("expired token = %v", err)
	}

	if err := db.RevokeMCPToken(ctx, "someone-else", token.ID); err == nil {
		t.Fatal("another user revoked the token")
	}
	if err := db.RevokeMCPToken(ctx, "default", token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.VerifyMCPToken(ctx, secret, now); err != ErrMCPTokenInvalid {
		t.Fatalf("revoked token = %v", err)
	}
	tokens, err := db.ListMCPTokens(ctx, "default")
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
}

func TestMCPTokenAndGrantMailboxes(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	now := time.Now().UTC()
	mailboxes := []string{"account:a1", "shared:a1:support@example.com"}

	_, secret, err := db.CreateMCPToken(ctx, MCPTokenInput{UserID: "default", Scopes: []string{"mail.read"}, Mailboxes: mailboxes})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.VerifyMCPToken(ctx, secret, now)
	if err != nil || strings.Join(got.Mailboxes, " ") != strings.Join(mailboxes, " ") {
		t.Fatalf("token mailboxes = %v, %v", got.Mailboxes, err)
	}
	_, all, _ := db.CreateMCPToken(ctx, MCPTokenInput{UserID: "default", Scopes: []string{"mail.read"}})
	if got, _ := db.VerifyMCPToken(ctx, all, now); len(got.Mailboxes) != 0 {
		t.Fatalf("unlimited token has mailboxes %v", got.Mailboxes)
	}

	client, err := db.RegisterOAuthClient(ctx, "Claude", []string{"https://claude.example/callback"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := db.CreateOAuthGrant(ctx, client, "default", []string{"mail.read"}, mailboxes, now)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := db.RefreshOAuthGrant(ctx, tokens.RefreshToken, client.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// A refreshed access token keeps the grant's mailboxes.
	got, err = db.VerifyMCPToken(ctx, refreshed.AccessToken, now.Add(time.Minute))
	if err != nil || strings.Join(got.Mailboxes, " ") != strings.Join(mailboxes, " ") {
		t.Fatalf("refreshed mailboxes = %v, %v", got.Mailboxes, err)
	}
}
