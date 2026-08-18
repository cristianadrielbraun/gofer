package mailauth

import (
	"bytes"
	"context"
	"testing"
)

var testMailboxCredentialKey = []byte("0123456789abcdef0123456789abcdef")

func storedOAuthTokenRecord(t *testing.T, manager *Service, ctx context.Context, accountID, provider string, secrets ...string) oauthTokenRecord {
	t.Helper()
	record, err := manager.oauthTokenForAccount(ctx, accountID, provider)
	if err != nil {
		t.Fatalf("oauthTokenForAccount() error = %v", err)
	}
	var plaintextAccess, plaintextRefresh string
	var accessCiphertext, refreshCiphertext []byte
	if err := manager.db.Read().QueryRowContext(ctx, `
		SELECT access_token, refresh_token, access_token_ciphertext, refresh_token_ciphertext
		FROM oauth_accounts WHERE account_id = ?`, accountID,
	).Scan(&plaintextAccess, &plaintextRefresh, &accessCiphertext, &refreshCiphertext); err != nil {
		t.Fatalf("query encrypted mailbox credential: %v", err)
	}
	if plaintextAccess != "" || plaintextRefresh != "" {
		t.Fatalf("plaintext mailbox credential remains: access=%q refresh=%q", plaintextAccess, plaintextRefresh)
	}
	for _, secret := range secrets {
		if secret != "" && (bytes.Contains(accessCiphertext, []byte(secret)) || bytes.Contains(refreshCiphertext, []byte(secret))) {
			t.Fatalf("mailbox ciphertext contains plaintext secret %q", secret)
		}
	}
	return record
}
