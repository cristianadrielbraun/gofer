package mailauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// ValidateUserStorageMigrationCredentials authenticates the original grants
// without sealing plaintext, upgrading old formats or starting provider work.
// The offline coordinator holds the runtime lock and supplies the existing key.
func ValidateUserStorageMigrationCredentials(ctx context.Context, source *storage.DB, key []byte) error {
	if ctx == nil || source == nil || len(key) != 32 {
		return errors.New("mailbox credential verification requires source, context and application key")
	}
	codec := New(nil, nil, key)
	rows, err := source.Read().QueryContext(ctx, `SELECT id,account_id,provider,provider_account_id,access_token,refresh_token,access_token_ciphertext,refresh_token_ciphertext,key_version FROM oauth_accounts ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var credential legacyOAuthCredential
		if err := rows.Scan(&credential.Context.ID, &credential.Context.AccountID, &credential.Context.Provider, &credential.Context.ProviderAccountID,
			&credential.AccessToken, &credential.RefreshToken, &credential.AccessCiphertext, &credential.RefreshCiphertext, &credential.KeyVersion); err != nil {
			return err
		}
		if _, _, err := migrationCredentialTokens(codec, credential); err != nil {
			return errors.New("application key or original binding cannot authenticate a retained mailbox grant")
		}
	}
	return rows.Err()
}

// ImportUserStorageMigrationCredentials is an offline migration stage. The
// coordinator must hold runtime locks, attach the inspected shared source
// read-only as migration_source, and use a private new central destination.
// The central users and account directory must already match the source. No
// user stores or provider endpoints are opened, and existing grants are never
// replaced. The caller retains its transaction through final verification.
func ImportUserStorageMigrationCredentials(ctx context.Context, tx *sql.Tx, key []byte) (err error) {
	if tx == nil || ctx == nil {
		return errors.New("credential migration requires a transaction")
	}
	codec := New(nil, nil, key)
	if _, err := codec.mailboxCredentialAEAD(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT gofer_migration_credentials`); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err != nil {
			_, rollbackErr := tx.ExecContext(cleanup, `ROLLBACK TO gofer_migration_credentials`)
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := tx.ExecContext(cleanup, `RELEASE gofer_migration_credentials`)
		err = errors.Join(err, releaseErr)
	}()
	if err := requireFreshCredentialMigration(ctx, tx); err != nil {
		return err
	}
	// Install ordinary lifecycle guards after the trusted private copy, inside
	// this transaction. Disabled/deleting owners never become temporarily active.
	if _, err := tx.ExecContext(ctx, userMailboxCredentialSchema); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.account_id,c.provider,c.provider_account_id,
 c.access_token,c.refresh_token,c.access_token_ciphertext,c.refresh_token_ciphertext,c.key_version,a.user_id
 FROM migration_source.oauth_accounts c JOIN migration_source.accounts a ON a.id=c.account_id ORDER BY c.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var credential legacyOAuthCredential
		var owner string
		if err := rows.Scan(&credential.Context.ID, &credential.Context.AccountID, &credential.Context.Provider, &credential.Context.ProviderAccountID,
			&credential.AccessToken, &credential.RefreshToken, &credential.AccessCiphertext, &credential.RefreshCiphertext, &credential.KeyVersion, &owner); err != nil {
			return err
		}
		access, refresh, err := migrationCredentialCiphertext(codec, credential)
		if err != nil {
			return err
		}
		// Metadata stays in SQLite so DATETIME storage classes/representations,
		// NULL expiry, token type and recorded scopes are preserved exactly.
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.gofer_mailbox_credentials(
 id,account_id,user_id,provider,provider_account_id,access_token_ciphertext,refresh_token_ciphertext,key_version,
 token_type,expires_at,scopes,granted_scopes,revision,created_at,updated_at)
 SELECT id,account_id,?,provider,provider_account_id,?,?,2,token_type,expires_at,scopes,scopes,1,created_at,updated_at
 FROM migration_source.oauth_accounts WHERE id=?`, owner, access, refresh, credential.Context.ID); err != nil {
			return fmt.Errorf("import mailbox credential: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := verifyCredentialMigration(ctx, tx, codec); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, userMailboxCredentialGuards)
	return err
}

func requireFreshCredentialMigration(ctx context.Context, tx *sql.Tx) error {
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM main.sqlite_schema WHERE name IN ('gofer_mailbox_credentials','gofer_user_store'))
 OR EXISTS(SELECT 1 FROM main.accounts) OR EXISTS(SELECT 1 FROM main.oauth_accounts)`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("mailbox credential migration requires a new central destination")
	}
	// Require every original grant and directory entry to have an exact retained
	// ordinary owner; actor/lifecycle state is central and must not be enabled to
	// work around normal credential guards. Preserve stale provider bindings.
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM migration_source.oauth_accounts c LEFT JOIN migration_source.accounts a ON a.id=c.account_id
 LEFT JOIN migration_source.users original ON original.id=a.user_id
 LEFT JOIN main.users u ON u.id=a.user_id LEFT JOIN main.gofer_account_directory d ON d.account_id=a.id
 WHERE a.id IS NULL OR original.id IS NULL OR u.id IS NULL OR d.account_id IS NULL
 OR original.user_type<>'webmail' OR original.is_admin<>0 OR u.user_type<>'webmail' OR u.is_admin<>0
 OR u.status IS NOT original.status OR u.deletion_pending IS NOT original.deletion_pending
 OR d.user_id IS NOT a.user_id OR d.state IS NOT CASE WHEN coalesce(a.is_deleting,0)<>0 THEN 'deleting' ELSE 'active' END
 OR c.id='' OR c.provider NOT IN ('google','microsoft') OR c.provider_account_id='')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("mailbox credential migration ownership or lifecycle does not match source")
	}
	return nil
}

func migrationCredentialCiphertext(codec *Service, row legacyOAuthCredential) ([]byte, []byte, error) {
	accessToken, refreshToken, err := migrationCredentialTokens(codec, row)
	if err != nil {
		return nil, nil, err
	}
	if row.KeyVersion.Valid && row.KeyVersion.Int64 == mailboxCredentialKeyVersion {
		return row.AccessCiphertext, row.RefreshCiphertext, nil
	}
	access, err := codec.encryptOAuthToken(row.Context, "access", accessToken)
	if err != nil {
		return nil, nil, err
	}
	refresh, err := codec.encryptOAuthToken(row.Context, "refresh", refreshToken)
	return access, refresh, err
}

func migrationCredentialTokens(codec *Service, row legacyOAuthCredential) (string, string, error) {
	if row.KeyVersion.Valid {
		if row.AccessToken != "" || row.RefreshToken != "" {
			return "", "", errors.New("mailbox credential mixes plaintext and ciphertext")
		}
		if row.KeyVersion.Int64 != mailboxCredentialLegacyKeyVersion && row.KeyVersion.Int64 != mailboxCredentialKeyVersion {
			return "", "", errors.New("unsupported mailbox credential key version")
		}
		var err error
		row.AccessToken, err = codec.decryptOAuthToken(row.Context, "access", row.AccessCiphertext, int(row.KeyVersion.Int64))
		if err != nil {
			return "", "", err
		}
		row.RefreshToken, err = codec.decryptOAuthToken(row.Context, "refresh", row.RefreshCiphertext, int(row.KeyVersion.Int64))
		if err != nil {
			return "", "", err
		}
	} else if row.AccessCiphertext != nil || row.RefreshCiphertext != nil {
		return "", "", errors.New("plaintext mailbox credential contains ciphertext")
	}
	return row.AccessToken, row.RefreshToken, nil
}

func verifyCredentialMigration(ctx context.Context, tx *sql.Tx, codec *Service) error {
	var sourceCount, destinationCount int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM migration_source.oauth_accounts),(SELECT count(*) FROM main.gofer_mailbox_credentials)`).Scan(&sourceCount, &destinationCount); err != nil {
		return err
	}
	if sourceCount != destinationCount {
		return errors.New("mailbox credential migration row parity failed")
	}
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.gofer_mailbox_credentials c JOIN migration_source.oauth_accounts original ON original.id=c.id
 WHERE original.key_version=2 AND (c.access_token_ciphertext IS NOT original.access_token_ciphertext OR c.refresh_token_ciphertext IS NOT original.refresh_token_ciphertext))`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("current mailbox ciphertext parity failed")
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.gofer_mailbox_credentials c JOIN migration_source.accounts a ON a.id=c.account_id
 WHERE c.user_id IS NOT a.user_id OR c.key_version<>2 OR c.revision<>1
 OR c.granted_scopes IS NOT c.scopes)`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("mailbox credential migration binding or grant parity failed")
	}
	columns := []string{"id", "account_id", "provider", "provider_account_id", "token_type", "expires_at", "scopes", "created_at", "updated_at"}
	projection := ""
	for _, column := range columns {
		if projection != "" {
			projection += ","
		}
		projection += `typeof("` + column + `"),"` + column + `" COLLATE BINARY`
	}
	source := `SELECT ` + projection + ` FROM migration_source.oauth_accounts`
	destination := `SELECT ` + projection + ` FROM main.gofer_mailbox_credentials`
	for _, pair := range [][2]string{{source, destination}, {destination, source}} {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT * FROM (`+pair[0]+` EXCEPT `+pair[1]+`))`).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return errors.New("mailbox credential migration metadata parity failed")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.account_id,c.provider,c.provider_account_id,
 c.access_token_ciphertext,c.refresh_token_ciphertext,original.access_token,original.refresh_token,
 original.access_token_ciphertext,original.refresh_token_ciphertext,original.key_version
 FROM main.gofer_mailbox_credentials c JOIN migration_source.oauth_accounts original ON original.id=c.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var original legacyOAuthCredential
		var access, refresh []byte
		if err := rows.Scan(&original.Context.ID, &original.Context.AccountID, &original.Context.Provider, &original.Context.ProviderAccountID,
			&access, &refresh, &original.AccessToken, &original.RefreshToken, &original.AccessCiphertext, &original.RefreshCiphertext, &original.KeyVersion); err != nil {
			return err
		}
		if original.KeyVersion.Valid {
			original.AccessToken, err = codec.decryptOAuthToken(original.Context, "access", original.AccessCiphertext, int(original.KeyVersion.Int64))
			if err == nil {
				original.RefreshToken, err = codec.decryptOAuthToken(original.Context, "refresh", original.RefreshCiphertext, int(original.KeyVersion.Int64))
			}
			if err != nil {
				return err
			}
		}
		for _, token := range []struct {
			kind, expected string
			ciphertext     []byte
		}{{"access", original.AccessToken, access}, {"refresh", original.RefreshToken, refresh}} {
			value, err := codec.decryptOAuthToken(original.Context, token.kind, token.ciphertext, mailboxCredentialKeyVersion)
			if err != nil {
				return err
			}
			if value != token.expected {
				return errors.New("mailbox credential migration token parity failed")
			}
		}
	}
	return rows.Err()
}
