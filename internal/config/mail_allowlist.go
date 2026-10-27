package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// mailAllowlist returns the administrator's allowed mail lists as they apply
// to a person. Accounts from before people had their own sign-in have no
// owner, and are held to the lists in full.
func (s *AccountStore) mailAllowlist(ctx context.Context, userID string) (storage.MailAllowlist, error) {
	list, err := s.policyDB.MailAllowlistForUser(ctx, userID)
	if err != nil {
		return storage.MailAllowlist{}, fmt.Errorf("check allowed mail lists: %w", err)
	}
	return list, nil
}

// checkMailAllowlist refuses a mailbox the allowed mail lists don't let its
// owner use.
func (s *AccountStore) checkMailAllowlist(ctx context.Context, userID string, mailbox storage.MailAllowlistMailbox) error {
	list, err := s.mailAllowlist(ctx, userID)
	if err != nil {
		return err
	}
	return list.CheckMailbox(mailbox)
}

// requestMailbox is what the allowed mail lists check about a new or changed
// account.
func requestMailbox(req *models.CreateAccountRequest) storage.MailAllowlistMailbox {
	return storage.MailAllowlistMailbox{Provider: req.Provider, Email: req.EmailAddress,
		IMAPHost: req.IMAPHost, SMTPHost: req.SMTPHost, JMAPSessionURL: req.JMAPSessionURL}
}

// CheckAccountServer refuses a server one of an account's identities would
// send through, when the allowed mail lists don't let the account's owner
// connect to it.
func (s *AccountStore) CheckAccountServer(ctx context.Context, accountID, host string) error {
	owner, err := s.accountOwner(ctx, accountID, "")
	if err != nil {
		return err
	}
	list, err := s.mailAllowlist(ctx, owner)
	if err != nil {
		return err
	}
	return list.CheckServer(host)
}

// checkUpdatedMailbox checks an account as an update would leave it: the
// fields the update sets, and the saved ones it leaves alone.
func (s *AccountStore) checkUpdatedMailbox(ctx context.Context, accountID, userID string, req *models.CreateAccountRequest) error {
	var mailbox storage.MailAllowlistMailbox
	err := s.db.Read().QueryRowContext(ctx, `
		SELECT provider, email_address, COALESCE(imap_host, ''), COALESCE(smtp_host, ''), COALESCE(jmap_session_url, '')
		FROM accounts WHERE id = ?`, accountID).Scan(
		&mailbox.Provider, &mailbox.Email, &mailbox.IMAPHost, &mailbox.SMTPHost, &mailbox.JMAPSessionURL)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("account %s not found", accountID)
	}
	if err != nil {
		return err
	}
	if req.Provider != "" {
		mailbox.Provider = req.Provider
	}
	if req.EmailAddress != "" {
		mailbox.Email = req.EmailAddress
	}
	switch {
	case accountProviderUsesGraphMail(mailbox.Provider), accountProviderUsesJMAP(mailbox.Provider):
		mailbox.IMAPHost, mailbox.SMTPHost = "", ""
		if req.JMAPSessionURL != "" {
			mailbox.JMAPSessionURL = req.JMAPSessionURL
		}
	default:
		mailbox.JMAPSessionURL = ""
		if req.IMAPHost != "" {
			mailbox.IMAPHost = req.IMAPHost
		}
		if req.SMTPHost != "" {
			mailbox.SMTPHost = req.SMTPHost
		}
	}
	return s.checkMailAllowlist(ctx, userID, mailbox)
}

// checkMailAllowlist refuses to connect a copied account the allowed mail
// lists no longer let its owner use.
func (s *AccountServiceSnapshot) checkMailAllowlist(ctx context.Context) error {
	list, err := s.repository.routing.System().MailAllowlistForUser(ctx, s.owner)
	if err != nil {
		return fmt.Errorf("check allowed mail lists: %w", err)
	}
	d := s.data
	return list.CheckMailbox(storage.MailAllowlistMailbox{Provider: d.Provider, Email: d.EmailAddress,
		IMAPHost: d.IMAPHost, SMTPHost: d.SMTPHost, JMAPSessionURL: d.JMAPSessionURL})
}

// checkServiceURLs refuses contacts or calendar servers the allowed mail
// lists don't let the account's owner connect to.
func (s *UserAccountStore) checkServiceURLs(ctx context.Context, owner string, addresses ...string) error {
	list, err := s.routing.System().MailAllowlistForUser(ctx, owner)
	if err != nil {
		return fmt.Errorf("check allowed mail lists: %w", err)
	}
	for _, address := range addresses {
		if err := list.CheckServerURL(address); err != nil {
			return err
		}
	}
	return nil
}
