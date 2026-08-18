package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const oauthAccountsV86Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	provider_account_id TEXT NOT NULL CHECK (provider_account_id <> ''),
	access_token TEXT NOT NULL DEFAULT '',
	refresh_token TEXT NOT NULL DEFAULT '',
	access_token_ciphertext BLOB CHECK (access_token_ciphertext IS NULL OR length(access_token_ciphertext) > 0),
	refresh_token_ciphertext BLOB CHECK (refresh_token_ciphertext IS NULL OR length(refresh_token_ciphertext) > 0),
	key_version INTEGER CHECK (key_version IS NULL OR key_version > 0),
	token_type TEXT NOT NULL DEFAULT 'Bearer',
	expires_at DATETIME,
	scopes TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	CHECK (
		(key_version IS NULL AND access_token_ciphertext IS NULL AND refresh_token_ciphertext IS NULL)
		OR
		(key_version IS NOT NULL AND access_token = '' AND refresh_token = '')
	)
)`

type legacyMailboxOAuthCredential struct {
	ID                string
	UserID            string
	Provider          string
	ProviderAccountID string
	AccessToken       string
	RefreshToken      string
	TokenType         string
	ExpiresAt         sql.NullTime
	Scopes            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	AccountID         string
}

type mailboxOAuthAccountCandidate struct {
	ID                string
	UserID            string
	Provider          string
	ProviderAccountID string
}

func migrateV85ToV86(tx *sql.Tx) error {
	exists, err := tableExistsTx(tx, "oauth_accounts")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(fmt.Sprintf(oauthAccountsV86Table, "oauth_accounts")); err != nil {
			return fmt.Errorf("create exact mailbox OAuth credential table: %w", err)
		}
		if err := createMailboxOAuthCredentialIndexes(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 86)
	}

	alreadyExact, err := columnExistsTx(tx, "oauth_accounts", "account_id")
	if err != nil {
		return err
	}
	if alreadyExact {
		for _, column := range []string{"access_token_ciphertext", "refresh_token_ciphertext", "key_version"} {
			exists, err := columnExistsTx(tx, "oauth_accounts", column)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("oauth_accounts has account_id but is missing %s", column)
			}
		}
		if err := createMailboxOAuthCredentialIndexes(tx); err != nil {
			return err
		}
		if err := foreignKeyCheckTx(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 86)
	}

	credentials, err := loadLegacyMailboxOAuthCredentials(tx)
	if err != nil {
		return err
	}
	if len(credentials) > 0 {
		accountsExists, err := tableExistsTx(tx, "accounts")
		if err != nil {
			return err
		}
		if !accountsExists {
			return fmt.Errorf("map mailbox OAuth credentials: accounts table is missing")
		}
		accounts, err := loadMailboxOAuthAccountCandidates(tx)
		if err != nil {
			return err
		}
		if err := mapLegacyMailboxOAuthCredentials(credentials, accounts); err != nil {
			return err
		}
		for _, credential := range credentials {
			for index := range accounts {
				if accounts[index].ID != credential.AccountID || accounts[index].ProviderAccountID != "" {
					continue
				}
				if _, err := tx.Exec(`UPDATE accounts SET provider_account_id = ? WHERE id = ? AND provider_account_id = ''`,
					credential.ProviderAccountID, credential.AccountID,
				); err != nil {
					return fmt.Errorf("backfill mailbox account provider identity: %w", err)
				}
				accounts[index].ProviderAccountID = credential.ProviderAccountID
			}
		}
	}

	if _, err := tx.Exec(fmt.Sprintf(oauthAccountsV86Table, "oauth_accounts_v86")); err != nil {
		return fmt.Errorf("create migrated mailbox OAuth credential table: %w", err)
	}
	for _, credential := range credentials {
		if _, err := tx.Exec(`
			INSERT INTO oauth_accounts_v86 (
				id, account_id, provider, provider_account_id,
				access_token, refresh_token, token_type, expires_at, scopes, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			credential.ID, credential.AccountID, credential.Provider, credential.ProviderAccountID,
			credential.AccessToken, credential.RefreshToken, credential.TokenType,
			credential.ExpiresAt, credential.Scopes, credential.CreatedAt, credential.UpdatedAt,
		); err != nil {
			return fmt.Errorf("copy mailbox OAuth credential %q: %w", credential.ID, err)
		}
	}
	if _, err := tx.Exec(`DROP TABLE oauth_accounts`); err != nil {
		return fmt.Errorf("drop legacy mailbox OAuth credential table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE oauth_accounts_v86 RENAME TO oauth_accounts`); err != nil {
		return fmt.Errorf("activate exact mailbox OAuth credential table: %w", err)
	}
	if err := createMailboxOAuthCredentialIndexes(tx); err != nil {
		return err
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 86)
}

func loadLegacyMailboxOAuthCredentials(tx *sql.Tx) ([]*legacyMailboxOAuthCredential, error) {
	rows, err := tx.Query(`
		SELECT id, user_id, provider, provider_account_id, access_token, refresh_token,
		       token_type, expires_at, scopes, created_at, updated_at
		FROM oauth_accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load legacy mailbox OAuth credentials: %w", err)
	}
	defer rows.Close()
	credentials := []*legacyMailboxOAuthCredential{}
	for rows.Next() {
		credential := &legacyMailboxOAuthCredential{}
		if err := rows.Scan(
			&credential.ID, &credential.UserID, &credential.Provider, &credential.ProviderAccountID,
			&credential.AccessToken, &credential.RefreshToken, &credential.TokenType,
			&credential.ExpiresAt, &credential.Scopes, &credential.CreatedAt, &credential.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan legacy mailbox OAuth credential: %w", err)
		}
		credential.Provider = strings.TrimSpace(credential.Provider)
		credential.ProviderAccountID = strings.TrimSpace(credential.ProviderAccountID)
		if credential.ProviderAccountID == "" {
			return nil, fmt.Errorf("map mailbox OAuth credential %q: provider account identity is empty", credential.ID)
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load legacy mailbox OAuth credentials: %w", err)
	}
	return credentials, nil
}

func loadMailboxOAuthAccountCandidates(tx *sql.Tx) ([]mailboxOAuthAccountCandidate, error) {
	rows, err := tx.Query(`
		SELECT id, COALESCE(user_id, ''), provider, provider_account_id
		FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load mailbox accounts for OAuth credential mapping: %w", err)
	}
	defer rows.Close()
	accounts := []mailboxOAuthAccountCandidate{}
	for rows.Next() {
		var account mailboxOAuthAccountCandidate
		if err := rows.Scan(&account.ID, &account.UserID, &account.Provider, &account.ProviderAccountID); err != nil {
			return nil, fmt.Errorf("scan mailbox account for OAuth credential mapping: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load mailbox accounts for OAuth credential mapping: %w", err)
	}
	return accounts, nil
}

func mapLegacyMailboxOAuthCredentials(credentials []*legacyMailboxOAuthCredential, accounts []mailboxOAuthAccountCandidate) error {
	usedAccounts := map[string]bool{}
	for _, credential := range credentials {
		mailboxProvider, ok := mailboxProviderForOAuthProvider(credential.Provider)
		if !ok {
			return fmt.Errorf("map mailbox OAuth credential %q: unsupported provider %q", credential.ID, credential.Provider)
		}
		matches := []string{}
		for _, account := range accounts {
			if account.UserID == credential.UserID && account.Provider == mailboxProvider &&
				account.ProviderAccountID != "" && account.ProviderAccountID == credential.ProviderAccountID {
				matches = append(matches, account.ID)
			}
		}
		if len(matches) > 1 {
			return fmt.Errorf("map mailbox OAuth credential %q: %d exact mailbox matches", credential.ID, len(matches))
		}
		if len(matches) == 1 {
			if usedAccounts[matches[0]] {
				return fmt.Errorf("map mailbox OAuth credential %q: mailbox %q is already assigned", credential.ID, matches[0])
			}
			credential.AccountID = matches[0]
			usedAccounts[matches[0]] = true
		}
	}

	for _, credential := range credentials {
		if credential.AccountID != "" {
			continue
		}
		mailboxProvider, _ := mailboxProviderForOAuthProvider(credential.Provider)
		remainingCredentials := 0
		for _, candidate := range credentials {
			candidateProvider, _ := mailboxProviderForOAuthProvider(candidate.Provider)
			if candidate.AccountID == "" && candidate.UserID == credential.UserID && candidateProvider == mailboxProvider {
				remainingCredentials++
			}
		}
		blankAccounts := []string{}
		for _, account := range accounts {
			if !usedAccounts[account.ID] && account.UserID == credential.UserID &&
				account.Provider == mailboxProvider && account.ProviderAccountID == "" {
				blankAccounts = append(blankAccounts, account.ID)
			}
		}
		if remainingCredentials != 1 || len(blankAccounts) != 1 {
			return fmt.Errorf(
				"map mailbox OAuth credential %q: ownership is ambiguous (%d credentials, %d unbound mailboxes)",
				credential.ID, remainingCredentials, len(blankAccounts),
			)
		}
		credential.AccountID = blankAccounts[0]
		usedAccounts[blankAccounts[0]] = true
	}
	return nil
}

func mailboxProviderForOAuthProvider(provider string) (string, bool) {
	switch strings.TrimSpace(provider) {
	case "google":
		return "gmail", true
	case "microsoft":
		return "outlook", true
	default:
		return "", false
	}
}

func createMailboxOAuthCredentialIndexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_provider_account ON oauth_accounts(provider, provider_account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_oauth_accounts_account ON oauth_accounts(account_id)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create mailbox OAuth credential index: %w", err)
		}
	}
	return nil
}
