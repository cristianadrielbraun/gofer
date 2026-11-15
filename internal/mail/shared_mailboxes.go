package mail

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

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
		o.checkSharedSenders(accountID, cfg, password, identities.Unchecked)
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

// checkSharedSenders asks the account's SMTP server, in the background,
// whether it accepts each new shared mailbox identity's address as a sender
// (smtp.CheckSender: nothing is sent), and records the answer. The identity
// is kept whatever the answer.
func (o *SyncOrchestrator) checkSharedSenders(accountID string, cfg *models.AccountConfig, password string, identities []storage.SharedMailboxIdentity) {
	if cfg == nil || strings.TrimSpace(cfg.SMTPHost) == "" {
		return
	}
	smtpCfg := *cfg
	smtpPassword := password
	if cfg.SmtpUsername != "" && o.imapScope == nil && o.accountStore != nil {
		if decrypted, err := o.accountStore.DecryptSmtpPassword(context.Background(), accountID); err == nil && decrypted != "" {
			smtpPassword = decrypted
		}
	}
	recipient := strings.TrimSpace(cfg.Username)
	if !strings.Contains(recipient, "@") {
		recipient = ""
	}
	go func() {
		for _, identity := range identities {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			result, err := smtp.CheckSender(ctx, &smtpCfg, smtpPassword, identity.MailboxEmail, recipient)
			state, message := storage.SenderCheckAccepted, ""
			switch {
			case err != nil:
				state, message = storage.SenderCheckUnknown, err.Error()
			case !result.Accepted:
				state, message = storage.SenderCheckRejected, strings.TrimSpace(fmt.Sprintf("%d %s", result.Code, result.Message))
			}
			if err := o.imapRepository().SetSharedIdentitySenderCheck(ctx, identity.IdentityID, state, message, time.Now()); err != nil {
				log.Printf("shared mailboxes %s: record sender check for %s: %v", accountID, identity.MailboxEmail, err)
			}
			cancel()
			log.Printf("shared mailboxes %s: sender check %s: %s", accountID, identity.MailboxEmail, state)
		}
	}()
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
