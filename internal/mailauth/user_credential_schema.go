package mailauth

// Absent access tokens are valid for existing refresh-only authorizations.
// New grants still require an access token at the public Upsert boundary.
const userMailboxCredentialSchema = `
		CREATE TABLE IF NOT EXISTS gofer_mailbox_credentials (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL UNIQUE REFERENCES gofer_account_directory(account_id),
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL CHECK(provider IN ('google','microsoft')),
			provider_account_id TEXT NOT NULL CHECK(provider_account_id<>''),
			access_token_ciphertext BLOB CHECK(access_token_ciphertext IS NULL OR length(access_token_ciphertext)>0),
			refresh_token_ciphertext BLOB CHECK(refresh_token_ciphertext IS NULL OR length(refresh_token_ciphertext)>0),
			key_version INTEGER NOT NULL CHECK(key_version=2),
			token_type TEXT NOT NULL DEFAULT 'Bearer', expires_at DATETIME,
			scopes TEXT NOT NULL DEFAULT '', granted_scopes TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id,provider,provider_account_id)
		);`

const userMailboxCredentialGuards = `
		CREATE TRIGGER IF NOT EXISTS gofer_mailbox_credential_owner_insert BEFORE INSERT ON gofer_mailbox_credentials
		WHEN NOT EXISTS(SELECT 1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
		 WHERE d.account_id=NEW.account_id AND d.user_id=NEW.user_id AND d.state='active'
		 AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)
		BEGIN SELECT RAISE(ABORT,'mailbox credential owner is unavailable'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_mailbox_credential_identity BEFORE UPDATE OF id,account_id,user_id ON gofer_mailbox_credentials
		WHEN NEW.id IS NOT OLD.id OR NEW.account_id IS NOT OLD.account_id OR NEW.user_id IS NOT OLD.user_id
		BEGIN SELECT RAISE(ABORT,'mailbox credential ownership is immutable'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_mailbox_credential_owner_update BEFORE UPDATE ON gofer_mailbox_credentials
		WHEN NOT EXISTS(SELECT 1 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
		 WHERE d.account_id=NEW.account_id AND d.user_id=NEW.user_id AND d.state='active'
		 AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)
		BEGIN SELECT RAISE(ABORT,'mailbox credential owner is unavailable'); END;
	`
