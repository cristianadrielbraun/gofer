package mail

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// applySharedMailboxDiscovery records the shared mailboxes an IMAP folder
// listing showed and brings the account's sender identities in line with
// them. It runs with every folder sync, which already lists the folders, so
// it asks the server nothing more. A failure is logged and does not stop the
// sync of the folders themselves.
func (o *SyncOrchestrator) applySharedMailboxDiscovery(ctx context.Context, accountID string, cfg *models.AccountConfig, password string, folders []imap.FolderInfo, complete bool) {
	identities, ok := o.recordSharedMailboxes(ctx, accountID, sharedMailboxesListed(folders), complete)
	if ok && len(identities.Unchecked) > 0 {
		o.checkSharedSenders(ctx, accountID, cfg, password, identities.Unchecked)
	}
}

// recordSharedMailboxes records the shared mailboxes a sync found, whatever
// the protocol, brings the account's sender identities in line with them,
// and announces any change. ok is false when they could not be recorded.
func (o *SyncOrchestrator) recordSharedMailboxes(ctx context.Context, accountID string, seen []string, complete bool) (storage.SharedIdentityChanges, bool) {
	mailboxes, identities, err := o.imapRepository().ApplySharedMailboxDiscovery(ctx, accountID, seen, complete, time.Now().UTC())
	if err != nil {
		log.Printf("shared mailboxes %s: %v", accountID, err)
		return identities, false
	}
	if mailboxes.Changed() || identities.Changed() {
		log.Printf("shared mailboxes %s: added=%d restored=%d removed=%d identities created=%d reactivated=%d deactivated=%d renamed=%d",
			accountID, len(mailboxes.Added), len(mailboxes.Restored), len(mailboxes.Removed),
			len(identities.Created), len(identities.Reactivated), len(identities.Deactivated), len(identities.Renamed))
		// The sidebar and the compose From list are redrawn as for any
		// change to the account's folders.
		o.publishEvent(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: map[string]any{
			"status":                 "folders_changed",
			"folders_changed":        true,
			"shared_mailboxes_added": mailboxes.Added, "shared_mailboxes_restored": mailboxes.Restored, "shared_mailboxes_removed": mailboxes.Removed,
		}})
	}
	return identities, true
}

// sharedMailboxesListed returns the shared mailboxes a listing has folders
// of, each once.
func sharedMailboxesListed(folders []imap.FolderInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range folders {
		if f.SharedMailbox == "" || seen[f.SharedMailbox] {
			continue
		}
		seen[f.SharedMailbox] = true
		out = append(out, f.SharedMailbox)
	}
	return out
}

// sharedSenderCheckTimeout bounds asking the server about one address.
const sharedSenderCheckTimeout = 30 * time.Second

// checkSharedSenders asks the account's SMTP server, signed in with the
// account's SMTP credentials, whether it takes each new shared mailbox
// identity's address as a sender, and records the answer. It runs within the
// sync, under its context, so it ends with the sync or the account. Only a
// permanent refusal (5xx) counts as refused; a temporary one (4xx), or a
// server that can't be asked, leaves the answer unknown.
func (o *SyncOrchestrator) checkSharedSenders(ctx context.Context, accountID string, cfg *models.AccountConfig, password string, identities []storage.SharedMailboxIdentity) {
	if cfg == nil || strings.TrimSpace(cfg.SMTPHost) == "" {
		return
	}
	smtpCfg := *cfg
	smtpPassword, err := o.sharedSenderSMTPPassword(ctx, accountID, cfg, password)
	if err != nil {
		log.Printf("shared mailboxes %s: read the SMTP password: %v", accountID, err)
		return
	}
	recipient := strings.TrimSpace(cfg.Username)
	if !strings.Contains(recipient, "@") {
		recipient = ""
	}
	for _, identity := range identities {
		if ctx.Err() != nil {
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, sharedSenderCheckTimeout)
		result, err := smtp.CheckSender(checkCtx, &smtpCfg, smtpPassword, identity.MailboxEmail, recipient)
		cancel()
		state, message := sharedSenderCheckState(result, err)
		if err := o.imapRepository().SetSharedIdentitySenderCheck(ctx, identity.IdentityID, state, message, time.Now()); err != nil {
			log.Printf("shared mailboxes %s: record sender check for %s: %v", accountID, identity.MailboxEmail, err)
		}
		log.Printf("shared mailboxes %s: sender check %s: %s", accountID, identity.MailboxEmail, state)
	}
}

// sharedSenderCheckState is what a sender check's answer is recorded as.
func sharedSenderCheckState(result smtp.SenderCheck, err error) (string, string) {
	switch {
	case err != nil:
		return storage.SenderCheckUnknown, err.Error()
	case result.Accepted:
		return storage.SenderCheckAccepted, ""
	}
	message := strings.TrimSpace(fmt.Sprintf("%d %s", result.Code, result.Message))
	if result.Code >= 500 {
		return storage.SenderCheckRejected, message
	}
	return storage.SenderCheckUnknown, message
}

// sharedSenderSMTPPassword is the password the account sends mail with: its
// own SMTP password when it has a separate SMTP sign-in, else its mail one.
func (o *SyncOrchestrator) sharedSenderSMTPPassword(ctx context.Context, accountID string, cfg *models.AccountConfig, password string) (string, error) {
	if cfg.SmtpUsername == "" {
		return password, nil
	}
	var smtpPassword string
	var err error
	switch {
	case o.imapScope != nil:
		err = o.imapScope.accounts.WithAccountForUser(ctx, o.imapScope.owner, o.imapScope.id, func(local *config.AccountStore, _ *storage.DB) error {
			var err error
			smtpPassword, err = local.DecryptSmtpPassword(ctx, accountID)
			return err
		})
	case o.accountStore != nil:
		smtpPassword, err = o.accountStore.DecryptSmtpPassword(ctx, accountID)
	}
	if err != nil {
		return "", err
	}
	if smtpPassword == "" {
		return password, nil
	}
	return smtpPassword, nil
}

// disabledSharedMailboxes returns the account's shared mailboxes the user
// turned off. Failing to read them syncs every mailbox.
func (o *SyncOrchestrator) disabledSharedMailboxes(ctx context.Context, accountID string) map[string]bool {
	var off map[string]bool
	err := o.withSyncStore(ctx, func(db *storage.DB) error {
		var err error
		off, err = db.DisabledSharedMailboxes(ctx, accountID)
		return err
	})
	if err != nil {
		log.Printf("shared mailboxes %s: read which are turned off: %v", accountID, err)
	}
	return off
}
