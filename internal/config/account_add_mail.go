package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// ErrAccountHasMail is adding mail to an account that already has it.
var ErrAccountHasMail = errors.New("this account already has email")

// ErrAccountHasNoMail is turning on the mail of an account that has none.
var ErrAccountHasNoMail = errors.New("this account has no email")

// AddMail makes an account with no mail, only a CalDAV or CardDAV server, a
// mail account like any other, with the mailbox req describes. Its calendars
// and contacts keep signing in with the login they used.
func (s *UserAccountStore) AddMail(ctx context.Context, owner, id string, req *models.CreateAccountRequest) error {
	if req == nil {
		return errors.New("account request is required")
	}
	copy := *req
	if err := s.WithUser(ctx, owner, func(local *AccountStore, _ *storage.DB) error {
		return local.prepareAddMail(ctx, owner, &copy)
	}); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, owner, id, func(local *AccountStore, _ *storage.DB) error {
		return local.addMail(ctx, owner, id, &copy)
	})
}

// AddMail is UserAccountStore.AddMail for the shared store of open and
// personal mode.
func (s *AccountStore) AddMail(ctx context.Context, owner, id string, req *models.CreateAccountRequest) error {
	if req == nil {
		return errors.New("account request is required")
	}
	copy := *req
	if err := s.prepareAddMail(ctx, owner, &copy); err != nil {
		return err
	}
	return s.addMail(ctx, owner, id, &copy)
}

func (s *AccountStore) prepareAddMail(ctx context.Context, owner string, req *models.CreateAccountRequest) error {
	if err := s.prepareAccountCreation(ctx, req); err != nil {
		return err
	}
	if err := s.checkMailAllowlist(ctx, owner, requestMailbox(req)); err != nil {
		return err
	}
	if accountProviderHasNoMail(req.Provider) || accountProviderUsesGraphMail(req.Provider) || req.AuthMethod != "plain" {
		return errors.New("add mail with IMAP or JMAP")
	}
	return nil
}

func (s *AccountStore) addMail(ctx context.Context, owner, id string, req *models.CreateAccountRequest) error {
	tx, err := s.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var provider, davUsername, name string
	var davPassword []byte
	err = tx.QueryRowContext(ctx, `SELECT provider, username, encrypted_password, display_name FROM accounts
		WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, id, owner).Scan(&provider, &davUsername, &davPassword, &name)
	if err != nil {
		return err
	}
	if !accountProviderHasNoMail(provider) {
		return ErrAccountHasMail
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE user_id = ? AND id != ? AND email_address = ? COLLATE NOCASE)`,
		owner, id, req.EmailAddress).Scan(&taken); err != nil {
		return err
	}
	if taken == 1 {
		return ErrMailboxExists
	}
	if err := s.keepDAVLogin(ctx, tx, owner, id, davUsername, davPassword); err != nil {
		return err
	}
	password, err := s.encryptPassword(owner, id, passwordKindIMAP, req.Password)
	if err != nil {
		return fmt.Errorf("encrypt password: %w", err)
	}
	var smtpPassword []byte
	if req.SmtpPassword != "" {
		if smtpPassword, err = s.encryptPassword(owner, id, passwordKindSMTP, req.SmtpPassword); err != nil {
			return fmt.Errorf("encrypt smtp password: %w", err)
		}
	}
	if req.DisplayName == "" {
		req.DisplayName = name
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET provider = ?, email_address = ?, display_name = ?, initials = ?,
		  imap_host = ?, imap_port = ?, imap_tls_mode = ?, smtp_host = ?, smtp_port = ?, smtp_tls_mode = ?,
		  username = ?, encrypted_password = ?, auth_method = ?, smtp_username = ?, encrypted_smtp_password = ?,
		  jmap_session_url = ?, email_sync_enabled = 1, email_sync_error = '', email_sync_error_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ?`,
		req.Provider, req.EmailAddress, req.DisplayName, extractInitials(req.DisplayName),
		req.IMAPHost, req.IMAPPort, req.IMAPTLSMode, req.SMTPHost, req.SMTPPort, req.SMTPTLSMode,
		req.Username, password, req.AuthMethod, req.SmtpUsername, smtpPassword,
		req.JMAPSessionURL, id, owner); err != nil {
		return err
	}
	return tx.Commit()
}

// keepDAVLogin gives the calendars and contacts that signed in with the
// account's login a copy of it, before the account's login becomes the
// mailbox's.
func (s *AccountStore) keepDAVLogin(ctx context.Context, tx *sql.Tx, owner, id, username string, encrypted []byte) error {
	password := ""
	if len(encrypted) > 0 {
		var err error
		if password, err = s.decryptPassword(owner, id, passwordKindIMAP, encrypted); err != nil {
			return err
		}
	}
	calDAV, err := s.encryptPassword(owner, id, passwordKindCalDAV, password)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_caldav_configs
		SET username = ?, encrypted_password = ?, use_account_credentials = 0, updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ? AND user_id = ? AND use_account_credentials = 1`, username, calDAV, id, owner); err != nil {
		return err
	}
	cardDAV, err := s.encryptPassword(owner, id, passwordKindCardDAV, password)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE account_contact_sync_configs
		SET username = CASE WHEN username = '' THEN ? ELSE username END,
		    encrypted_password = COALESCE(encrypted_password, ?), updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ? AND user_id = ? AND provider = ?`, username, cardDAV, id, owner, providers.ProviderCardDAV)
	return err
}
