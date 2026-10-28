package mail

import (
	"context"
	"log"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// SetMutationWake sets what tells the queued-changes worker to run now, so
// the changes mail rules make reach the server without waiting for its next
// round. It is set once, before the orchestrator starts.
func (o *SyncOrchestrator) SetMutationWake(wake func()) {
	o.mutationWake = wake
}

// runMailRules applies the account's rules to the mail that arrived since
// they last ran, and tells open pages about the folders they changed. A
// failure is logged; the mail it left waits for the next sync.
func (o *SyncOrchestrator) runMailRules(ctx context.Context, accountID string) storage.MailRuleOutcome {
	var outcome storage.MailRuleOutcome
	apply := func(db *storage.DB) error {
		var err error
		outcome, err = db.ApplyPendingMailRules(ctx, accountID)
		return err
	}
	var err error
	if o.imapScope != nil {
		err = o.imapScope.call(ctx, apply)
	} else if o.db != nil {
		err = apply(o.db)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("mail rules account=%s: %v", accountID, err)
	}
	if !outcome.Changed() {
		return outcome
	}
	for folderID := range outcome.Folders {
		o.publishEvent(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folderID, Payload: map[string]any{"status": "complete", "reason": "mail-rules"}})
	}
	// Per-user stores replay their queue at the start of the account's next
	// pass, which a due change brings forward.
	if o.imapScope == nil && o.mutationWake != nil {
		o.mutationWake()
	}
	return outcome
}
