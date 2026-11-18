package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// syncJMAPAccount syncs a JMAP account. JMAP accounts share the provider
// repository that Gmail and Outlook use.
func (o *SyncOrchestrator) syncJMAPAccount(ctx context.Context, accountID string, cfg *models.AccountConfig) error {
	password, err := o.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		return err
	}
	// Label changes that failed earlier go first, so the sync below reads
	// them back from the server.
	o.replayJMAPLabelMutationQueue(ctx, accountID, cfg, password)
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return fmt.Errorf("JMAP session: %w", err)
	}
	mailAccount := session.PrimaryAccount(jmap.CapabilityMail)
	if mailAccount == "" {
		return fmt.Errorf("the JMAP server offers no mail account for this login")
	}
	// The shared mailboxes are found first, from the session alone, so the
	// account wizard lists them while the login's own mail still syncs.
	o.discoverJMAPSharedMailboxes(ctx, accountID, client, session)
	if err := o.syncJMAPTarget(ctx, accountID, client, session, jmapTarget{account: mailAccount}); err != nil {
		return err
	}
	o.syncJMAPSharedMailboxes(ctx, accountID, client, session)
	return nil
}

// discoverJMAPSharedMailboxes records the mailboxes shared with the login,
// with the names the server gives them, and brings their identities in
// line.
func (o *SyncOrchestrator) discoverJMAPSharedMailboxes(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session) {
	targets := jmapSharedAccounts(session)
	seen := make([]string, 0, len(targets))
	for _, target := range targets {
		seen = append(seen, target.mailbox)
	}
	if names := jmapSharedMailboxNames(ctx, client, session, targets); len(names) > 0 {
		if err := o.withSyncStore(ctx, func(db *storage.DB) error {
			return db.SetSharedMailboxServerNames(ctx, accountID, names)
		}); err != nil {
			log.Printf("jmap sync %s: shared mailbox names: %v", accountID, err)
		}
	}
	identities, ok := o.recordSharedMailboxes(ctx, accountID, seen, true)
	if ok && len(identities.Unchecked) > 0 {
		o.recordJMAPSharedSenders(ctx, session, targets, identities.Unchecked)
	}
}

// syncJMAPTarget syncs one JMAP account of the login, its own or a shared
// mailbox: its mailboxes, then its emails, each from its own cursors.
func (o *SyncOrchestrator) syncJMAPTarget(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session, target jmapTarget) error {
	mailboxes, err := o.syncJMAPMailboxes(ctx, accountID, client, session, target)
	if err != nil {
		return err
	}
	repo := o.outlookRepository()
	emailCursor, err := repo.GetLabelSyncState(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapEmailScope))
	if err != nil {
		return err
	}
	if emailCursor.Cursor == "" {
		return o.importJMAPEmails(ctx, accountID, client, session, target, mailboxes)
	}
	err = o.syncJMAPEmailChanges(ctx, accountID, client, session, target, mailboxes, emailCursor.Cursor)
	if jmap.IsCannotCalculateChanges(err) {
		// The server no longer knows the saved state: import everything again.
		log.Printf("jmap sync %s: changes since %q are no longer available, importing again", accountID, emailCursor.Cursor)
		return o.importJMAPEmails(ctx, accountID, client, session, target, mailboxes)
	}
	if err != nil {
		return err
	}
	return repo.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapMailboxScope), mailboxes.state, false)
}

// syncJMAPSharedMailboxes syncs the mailboxes shared with the login, each a
// JMAP account of its own, which discoverJMAPSharedMailboxes recorded. One
// the user turned off keeps its folders, listed, and gets no
// mail. A shared mailbox that fails is logged and does not stop the others,
// nor fail the sync of the login's own mail.
func (o *SyncOrchestrator) syncJMAPSharedMailboxes(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session) {
	targets := jmapSharedAccounts(session)
	off := o.disabledSharedMailboxes(ctx, accountID)
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		var err error
		if off[target.mailbox] {
			_, err = o.syncJMAPMailboxes(ctx, accountID, client, session, target)
		} else {
			err = o.syncJMAPTarget(ctx, accountID, client, session, target)
		}
		if err != nil {
			log.Printf("jmap sync %s: shared mailbox %s: %v", accountID, target.mailbox, err)
		}
	}
	if err := o.forgetUnsharedJMAPAccounts(ctx, accountID, targets); err != nil {
		log.Printf("jmap sync %s: shared mailboxes no longer shared: %v", accountID, err)
	}
}

// recordJMAPSharedSenders records, for new shared mailbox identities, what
// the session says of sending from them: a shared account the login may
// submit from accepts its own address.
func (o *SyncOrchestrator) recordJMAPSharedSenders(ctx context.Context, session *jmap.Session, targets []jmapTarget, unchecked []storage.SharedMailboxIdentity) {
	for _, identity := range unchecked {
		result, message := storage.SenderCheckUnknown, "The JMAP server does not let this login send from the mailbox."
		for _, target := range targets {
			if target.mailbox != strings.ToLower(identity.MailboxEmail) {
				continue
			}
			if _, ok := session.Accounts[target.account].AccountCapabilities[jmap.CapabilitySubmission]; ok {
				result, message = storage.SenderCheckAccepted, ""
			}
		}
		if err := o.imapRepository().SetSharedIdentitySenderCheck(ctx, identity.IdentityID, result, message, time.Now().UTC()); err != nil {
			log.Printf("shared mailbox sender %s: %v", identity.MailboxEmail, err)
		}
	}
}

// forgetUnsharedJMAPAccounts reconciles the folders of shared accounts the
// session no longer lists, as folders the server no longer has.
func (o *SyncOrchestrator) forgetUnsharedJMAPAccounts(ctx context.Context, accountID string, targets []jmapTarget) error {
	current := map[string]bool{}
	for _, target := range targets {
		current[target.account] = true
	}
	var keep []string
	gone := false
	err := o.withSyncStore(ctx, func(db *storage.DB) error {
		listed, err := db.ListListedProviderFolders(ctx, accountID)
		if err != nil {
			return err
		}
		for _, folder := range listed {
			if account, _ := jmapSplitRef(folder.ProviderRemoteID); account == "" || current[account] {
				keep = append(keep, folder.ProviderRemoteID)
			} else {
				gone = true
			}
		}
		return nil
	})
	if err != nil || !gone {
		return err
	}
	return o.reconcileJMAPFolders(ctx, accountID, keep)
}

// replayJMAPLabelMutationQueue retries the account's queued label changes,
// recording each outcome as the IMAP replay does.
func (o *SyncOrchestrator) replayJMAPLabelMutationQueue(ctx context.Context, accountID string, cfg *models.AccountConfig, password string) {
	if o.db == nil {
		return
	}
	entries, err := o.db.ListDueLabelMutations(ctx, accountID, storage.LabelProviderKeyword, providerLabelMutationReplayLimit)
	if err != nil {
		log.Printf("jmap label mutation queue list account=%s: %v", accountID, err)
		return
	}
	for _, entry := range entries {
		if err := o.applyQueuedJMAPLabelMutation(ctx, cfg, password, entry); err != nil {
			log.Printf("jmap label mutation replay account=%s message=%d label=%q op=%s: %v", entry.AccountID, entry.MessageID, entry.LabelName, entry.Operation, err)
			if markErr := o.db.MarkLabelMutationError(ctx, entry.ID, entry.Attempts, err); markErr != nil {
				log.Printf("jmap label mutation queue mark error id=%d: %v", entry.ID, markErr)
			}
			continue
		}
		if err := o.db.MarkLabelMutationSuccess(ctx, entry.ID); err != nil {
			log.Printf("jmap label mutation queue mark success id=%d: %v", entry.ID, err)
		}
	}
}

func (o *SyncOrchestrator) applyQueuedJMAPLabelMutation(ctx context.Context, cfg *models.AccountConfig, password string, entry storage.LabelMutationQueueEntry) error {
	info, err := o.queuedMessageMutationInfo(ctx, entry)
	if err != nil || info == nil {
		return err
	}
	add := entry.Operation == storage.LabelMutationAdd
	if !add && entry.Operation != storage.LabelMutationRemove {
		return fmt.Errorf("unsupported label mutation operation %q", entry.Operation)
	}
	if keyword, spam := JMAPSpamKeyword(entry.LabelName); spam {
		// Spam training keywords are status, not labels shown on the message.
		_, err := SetJMAPKeyword(ctx, cfg, password, info.LabelCopyIDs(info.RemoteMessageID), keyword, add)
		return err
	}
	keyword, err := o.keywordForAccountLabel(ctx, entry.AccountID, entry.LabelName)
	if err != nil {
		return err
	}
	if _, err := SetJMAPKeyword(ctx, cfg, password, info.LabelCopyIDs(info.RemoteMessageID), keyword, add); err != nil {
		return err
	}
	if add {
		if _, err := o.db.AddMessageLabel(ctx, entry.MessageID, entry.AccountID, storage.LabelInput{
			AccountID: entry.AccountID, Name: entry.LabelName, ProviderID: keyword, ProviderType: storage.LabelProviderKeyword,
		}); err != nil {
			return err
		}
	} else if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderKeyword, keyword, keyword); err != nil {
		return err
	}
	return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
}

// jmapMaxChanges is how many changed emails one Email/changes call returns.
const jmapMaxChanges = 500

// syncJMAPEmailChanges applies the emails created, updated and destroyed
// since the saved Email state. Each chunk is stored before its new state is
// saved, so an interrupted sync resumes from the last chunk stored. Created
// and updated emails are fetched as summaries, without bodies, and stored
// like imported ones, which also moves them between folders; an email no
// longer in any known mailbox, or gone, is removed from its folders.
func (o *SyncOrchestrator) syncJMAPEmailChanges(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session, target jmapTarget, mailboxes jmapMailboxes, since string) error {
	mailAccount := target.account
	folders := mailboxes.folders
	batch := jmapImportPage
	if max := session.Core.MaxObjectsInGet; max > 0 && max < batch {
		batch = max
	}
	repo := o.outlookRepository()
	touched := map[string]bool{}
	// Emails the server created since the cursor are new mail, announced per
	// folder as IMAP does once they are stored.
	fresh := map[string]*newMailSummary{}
	for {
		changes, err := client.GetEmailChanges(ctx, session, mailAccount, since, jmapMaxChanges)
		if err != nil {
			return err
		}
		created := make(map[string]bool, len(changes.Created))
		for _, id := range changes.Created {
			created[id] = true
		}
		gone := append([]string(nil), changes.Destroyed...)
		changed := append(append([]string(nil), changes.Created...), changes.Updated...)
		for start := 0; start < len(changed); start += batch {
			end := min(start+batch, len(changed))
			got, err := client.GetEmails(ctx, session, mailAccount, changed[start:end], jmap.EmailSummaryProperties)
			if err != nil {
				return err
			}
			// Changed and then deleted before this request.
			gone = append(gone, got.NotFound...)
			var rows []storage.ProviderSyncMessage
			for _, email := range got.List {
				emailRows := jmapEmailToProviderSyncs(accountID, target, email, folders)
				if len(emailRows) == 0 {
					gone = append(gone, email.ID)
					continue
				}
				for _, row := range emailRows {
					row.Arrived = created[email.ID]
					rows = append(rows, row)
					touched[row.FolderID] = true
					if created[email.ID] {
						if fresh[row.FolderID] == nil {
							fresh[row.FolderID] = &newMailSummary{}
						}
						fresh[row.FolderID].AddProvider(row, jmapNewMail(email))
					}
				}
			}
			if len(rows) > 0 {
				if _, err := repo.UpsertProviderSyncMessages(ctx, rows); err != nil {
					return err
				}
			}
		}
		for _, id := range gone {
			folderIDs, err := repo.MarkProviderMessageDeleted(ctx, accountID, target.ref(id))
			if err != nil {
				return err
			}
			for _, folderID := range folderIDs {
				touched[folderID] = true
			}
		}
		if err := repo.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapEmailScope), changes.NewState, false); err != nil {
			return err
		}
		if !changes.HasMoreChanges || changes.NewState == since {
			break
		}
		since = changes.NewState
	}
	for folderID := range touched {
		o.publishEvent(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folderID, Payload: map[string]any{"status": "complete", "provider": "jmap"}})
	}
	if len(fresh) > 0 {
		outcome := o.runMailRules(ctx, accountID)
		for folderID, summary := range fresh {
			summary.ApplyRules(outcome)
			o.publishNewMail(ctx, accountID, folderID, mailboxes.names[folderID], *summary)
		}
	}
	return nil
}

// jmapNewMail is what a new-mail notification shows of an email.
func jmapNewMail(email jmap.Email) storage.SyncMessage {
	msg := storage.SyncMessage{Subject: email.Subject, DateSent: email.ReceivedAt, IsRead: email.Keywords[jmap.KeywordSeen]}
	if len(email.From) > 0 {
		msg.FromName, msg.FromEmail = email.From[0].Name, email.From[0].Email
	}
	return msg
}

const (
	jmapEmailScope   = "email"
	jmapMailboxScope = "mailbox"
	// jmapImportScope holds the progress of an import that hasn't finished.
	jmapImportScope = "email-import"
	// jmapImportPage is how many emails one import request fetches, within the
	// server's own limit.
	jmapImportPage = 100
)

// jmapPagePoint is the last email a page ended with, and when it was
// received.
type jmapPagePoint struct {
	ID         string    `json:"anchor"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// jmapImportProgress is an import that hasn't finished: the Email state read
// before its first page and the last email it stored. It is saved after every
// page, so the next sync continues the import instead of starting over.
type jmapImportProgress struct {
	State string `json:"state"`
	jmapPagePoint
}

// importJMAPEmails stores every email of the account, newest first, removes
// from each folder the messages the server no longer has in it, then saves the
// Email and Mailbox states as the account's sync cursors. The Email
// state is read before the first page, so mail that arrives during the import
// is caught by the changes since it. An interrupted import continues from the
// last page it stored, with the state it read first; storing an email twice
// only updates it.
func (o *SyncOrchestrator) importJMAPEmails(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session, target jmapTarget, mailboxes jmapMailboxes) error {
	mailAccount := target.account
	repo := o.outlookRepository()
	saved, err := repo.GetLabelSyncState(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapImportScope))
	if err != nil {
		return err
	}
	var progress jmapImportProgress
	if saved.Cursor != "" {
		if err := json.Unmarshal([]byte(saved.Cursor), &progress); err != nil || progress.State == "" || progress.ID == "" {
			log.Printf("jmap sync %s: ignoring unreadable import progress %q", accountID, saved.Cursor)
			progress = jmapImportProgress{}
		}
	}
	folders := mailboxes.folders
	limit := jmapImportPage
	if max := session.Core.MaxObjectsInGet; max > 0 && max < limit {
		limit = max
	}
	touched := map[string]bool{}
	seen := map[string]map[string]bool{}
	see := func(folderID, emailID string) {
		if seen[folderID] == nil {
			seen[folderID] = map[string]bool{}
		}
		seen[folderID][emailID] = true
	}

	if progress.State == "" {
		before, err := client.GetEmails(ctx, session, mailAccount, []string{}, nil)
		if err != nil {
			return err
		}
		progress.State = before.State
	} else {
		// The cleanup below needs every email this import has seen, and the
		// earlier attempts' list is gone. Those emails were all received at
		// or after the saved one, so list them again, ids and mailboxes only.
		log.Printf("jmap sync %s: continuing the import from emails received at %s", accountID, progress.ReceivedAt.Format(time.RFC3339))
		after := progress.ReceivedAt
		err := pageJMAPEmails(ctx, client, session, mailAccount, limit, &after, jmapPagePoint{}, []string{"id", "mailboxIds"}, func(emails []jmap.Email, _ jmapPagePoint) error {
			for _, email := range emails {
				for mailboxID, in := range email.MailboxIDs {
					if folderID, ok := folders[mailboxID]; ok && in {
						see(folderID, target.ref(email.ID))
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	err = pageJMAPEmails(ctx, client, session, mailAccount, limit, nil, progress.jmapPagePoint, jmap.EmailSummaryProperties, func(emails []jmap.Email, last jmapPagePoint) error {
		var rows []storage.ProviderSyncMessage
		for _, email := range emails {
			for _, row := range jmapEmailToProviderSyncs(accountID, target, email, folders) {
				rows = append(rows, row)
				touched[row.FolderID] = true
				see(row.FolderID, row.ProviderMessageID)
			}
		}
		if len(rows) > 0 {
			if _, err := repo.UpsertProviderSyncMessages(ctx, rows); err != nil {
				return err
			}
		}
		if last.ID == "" {
			return nil
		}
		progress.jmapPagePoint = last
		cursor, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		return repo.SaveLabelSyncCursor(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapImportScope), string(cursor))
	})
	if err != nil {
		return err
	}
	// After a new import of a known account, as when the server could no
	// longer give the changes since its cursor, messages deleted meanwhile are
	// still stored.
	for _, folderID := range folders {
		removed, err := repo.MarkProviderMessagesMissingFromFolder(ctx, accountID, folderID, seen[folderID])
		if err != nil {
			return err
		}
		if removed > 0 {
			touched[folderID] = true
		}
	}
	// The progress goes first: left behind, it would be taken up by a later
	// import that has nothing to do with this one.
	if err := repo.SaveLabelSyncCursor(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapImportScope), ""); err != nil {
		return err
	}
	if err := repo.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapEmailScope), progress.State, true); err != nil {
		return err
	}
	if err := repo.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapMailboxScope), mailboxes.state, true); err != nil {
		return err
	}
	for folderID := range touched {
		o.publishEvent(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folderID, Payload: map[string]any{"status": "complete", "provider": "jmap"}})
	}
	return nil
}

// pageJMAPEmails walks the account's emails newest first, those received at
// or after `after` when it is set, starting just past from (or at the newest
// when from is empty), and hands each page to store with the last email in
// it. When the email a page would continue from has been deleted, it
// continues from that email's date instead, which hands the emails of that
// same second over again.
func pageJMAPEmails(ctx context.Context, client *jmap.Client, session *jmap.Session, mailAccount string, limit int, after *time.Time, from jmapPagePoint, properties []string, store func(emails []jmap.Email, last jmapPagePoint) error) error {
	var before *time.Time
	for {
		query := jmap.EmailQuery{
			AccountID: mailAccount,
			Sort:      []jmap.Comparator{{Property: "receivedAt", IsAscending: false}},
			Limit:     limit,
		}
		if after != nil || before != nil {
			query.Filter = &jmap.EmailFilter{After: after, Before: before}
		}
		if from.ID != "" {
			query.Anchor, query.AnchorOffset = from.ID, 1
		}
		page, emails, err := client.QueryAndGetEmails(ctx, session, query, properties)
		if jmap.IsAnchorNotFound(err) {
			if from.ReceivedAt.IsZero() {
				// Without a date to go on, start over.
				from = jmapPagePoint{}
				continue
			}
			// Before is exclusive and dates have whole seconds.
			next := from.ReceivedAt.Truncate(time.Second).Add(time.Second)
			before, from = &next, jmapPagePoint{}
			continue
		}
		if err != nil {
			return err
		}
		if len(page.IDs) > 0 {
			from = jmapPagePoint{ID: page.IDs[len(page.IDs)-1]}
			for _, email := range emails.List {
				if email.ID == from.ID {
					from.ReceivedAt = email.ReceivedAt
				}
			}
		}
		if err := store(emails.List, from); err != nil {
			return err
		}
		// A server may cap the page below the limit asked for; a page shorter
		// than its own limit is the last one.
		pageLimit := limit
		if page.Limit > 0 && page.Limit < pageLimit {
			pageLimit = page.Limit
		}
		if len(page.IDs) == 0 || len(page.IDs) < pageLimit {
			return nil
		}
	}
}

// jmapEmailToProviderSyncs gives one row per known mailbox the email is in,
// which together set the folders the email shows in. Keywords carry the read,
// flagged and draft states, and every other keyword is a label.
func jmapEmailToProviderSyncs(accountID string, target jmapTarget, email jmap.Email, folders map[string]string) []storage.ProviderSyncMessage {
	id := strings.TrimSpace(email.ID)
	if id == "" {
		return nil
	}
	internetMessageID := ""
	if len(email.MessageID) > 0 {
		internetMessageID = "<" + strings.Trim(email.MessageID[0], "<>") + ">"
	}
	var fromName, fromEmail string
	if len(email.From) > 0 {
		fromName, fromEmail = email.From[0].Name, email.From[0].Email
	}
	flagged := email.Keywords[jmap.KeywordFlagged]
	base := storage.ProviderSyncMessage{
		AccountID:         accountID,
		ProviderMessageID: target.ref(id),
		InternetMessageID: internetMessageID,
		ProviderThreadID:  target.ref(strings.TrimSpace(email.ThreadID)),
		InReplyTo:         jmapMessageIDList(email.InReplyTo),
		References:        jmapMessageIDList(email.References),
		ListID:            message.CleanListID(email.ListID),
		Subject:           strings.TrimSpace(email.Subject),
		FromName:          fromName,
		FromEmail:         fromEmail,
		DateSent:          email.SentAt,
		DateReceived:      email.ReceivedAt,
		Snippet:           strings.TrimSpace(email.Preview),
		IsRead:            email.Keywords[jmap.KeywordSeen],
		IsStarred:         flagged,
		IsFlagged:         flagged,
		IsDraft:           email.Keywords[jmap.KeywordDraft],
		HasAttachments:    email.HasAttachment,
		SizeBytes:         email.Size,
		ToRecipients:      jmapRecipients(email.To),
		CCRecipients:      jmapRecipients(email.CC),
		BCCRecipients:     jmapRecipients(email.BCC),
		Labels:            jmapKeywordLabels(email.Keywords),
		LabelsKnown:       true,
		LabelProvider:     storage.LabelProviderKeyword,
	}
	var rows []storage.ProviderSyncMessage
	for mailboxID, in := range email.MailboxIDs {
		folderID, known := folders[mailboxID]
		if !in || !known {
			continue
		}
		row := base
		row.FolderID = folderID
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].FolderID < rows[j].FolderID })
	return rows
}

// jmapKeywordLabels gives the email's keywords that are labels rather than
// status, in a stable order. They are the same keywords IMAP clients see, so
// they go through the same aliases as IMAP keyword labels. Keywords differing
// only in case are one label, as in IMAP.
func jmapKeywordLabels(set map[string]bool) []storage.LabelInput {
	names := make([]string, 0, len(set))
	for name, on := range set {
		if name = strings.TrimSpace(name); on && name != "" && !keywords.IsStatus(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var labels []storage.LabelInput
	seen := map[string]bool{}
	for _, name := range names {
		if key := strings.ToLower(name); !seen[key] {
			seen[key] = true
			labels = append(labels, storage.LabelInput{Name: name, ProviderID: name, ProviderType: storage.LabelProviderKeyword})
		}
	}
	return labels
}

// jmapMessageIDList writes JMAP's bare message ids as a header value.
func jmapMessageIDList(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.Trim(strings.TrimSpace(id), "<>"); id != "" {
			out = append(out, "<"+id+">")
		}
	}
	return strings.Join(out, " ")
}

func jmapRecipients(addresses []jmap.EmailAddress) []storage.Recipient {
	out := make([]storage.Recipient, 0, len(addresses))
	for _, address := range addresses {
		if strings.TrimSpace(address.Email) != "" {
			out = append(out, storage.Recipient{Name: address.Name, Email: address.Email})
		}
	}
	return out
}

// jmapMailboxes is what an email sync needs of an account's mailboxes: the
// folder of each mailbox, folder names, and the Mailbox state they were read
// at.
type jmapMailboxes struct {
	folders map[string]string
	names   map[string]string
	state   string
}

// syncJMAPMailboxes brings the account's folders up to date with its
// mailboxes. When only mailbox counters changed since the saved Mailbox
// state, as when mail arrives, it updates those counters and nothing else.
// Any other change, or a state the server no longer knows, reads every
// mailbox again.
func (o *SyncOrchestrator) syncJMAPMailboxes(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session, target jmapTarget) (jmapMailboxes, error) {
	mailAccount := target.account
	saved, err := o.outlookRepository().GetLabelSyncState(ctx, accountID, storage.LabelProviderJMAP, target.scope(jmapMailboxScope))
	if err != nil {
		return jmapMailboxes{}, err
	}
	if saved.Cursor != "" {
		mailboxes, ok, err := o.syncJMAPMailboxCounts(ctx, accountID, client, session, target, saved.Cursor)
		switch {
		case jmap.IsCannotCalculateChanges(err) || jmap.IsUnknownMethod(err):
			log.Printf("jmap sync %s: mailbox changes since %q are unavailable (%v), reading every mailbox", accountID, saved.Cursor, err)
		case err != nil:
			return jmapMailboxes{}, err
		case ok:
			return mailboxes, nil
		}
	}
	list, err := client.GetMailboxes(ctx, session, mailAccount, nil)
	if err != nil {
		return jmapMailboxes{}, err
	}
	if err := o.syncJMAPFolders(ctx, accountID, target, list.List); err != nil {
		return jmapMailboxes{}, err
	}
	mailboxes := jmapMailboxes{folders: map[string]string{}, names: map[string]string{}, state: list.State}
	for _, mailbox := range list.List {
		folderID := jmapLocalFolderID(accountID, target.ref(mailbox.ID))
		mailboxes.folders[mailbox.ID] = folderID
		mailboxes.names[folderID] = mailbox.Name
	}
	return mailboxes, nil
}

// syncJMAPMailboxCounts follows the mailbox changes since a saved state. It
// reports false, having changed nothing, when more than counters changed.
func (o *SyncOrchestrator) syncJMAPMailboxCounts(ctx context.Context, accountID string, client *jmap.Client, session *jmap.Session, target jmapTarget, since string) (jmapMailboxes, bool, error) {
	mailAccount := target.account
	updated := map[string]bool{}
	for {
		changes, err := client.GetMailboxChanges(ctx, session, mailAccount, since, 0)
		if err != nil {
			return jmapMailboxes{}, false, err
		}
		if len(changes.Created) > 0 || len(changes.Destroyed) > 0 || (len(changes.Updated) > 0 && !jmapCountsOnly(changes.UpdatedProperties)) {
			return jmapMailboxes{}, false, nil
		}
		for _, id := range changes.Updated {
			updated[id] = true
		}
		more := changes.HasMoreChanges && changes.NewState != since
		since = changes.NewState
		if !more {
			break
		}
	}
	mailboxes := jmapMailboxes{folders: map[string]string{}, names: map[string]string{}, state: since}
	err := o.withSyncStore(ctx, func(db *storage.DB) error {
		listed, err := db.ListListedProviderFolders(ctx, accountID)
		if err != nil {
			return err
		}
		for _, folder := range listed {
			if id, ok := target.owns(folder.ProviderRemoteID); ok {
				mailboxes.folders[id] = folder.ID
				mailboxes.names[folder.ID] = folder.Name
			}
		}
		return nil
	})
	if err != nil {
		return jmapMailboxes{}, false, err
	}
	ids := make([]string, 0, len(updated))
	for id := range updated {
		if mailboxes.folders[id] == "" {
			// A mailbox Gofer doesn't list yet.
			return jmapMailboxes{}, false, nil
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return mailboxes, true, nil
	}
	sort.Strings(ids)
	counts, err := client.GetMailboxCounts(ctx, session, mailAccount, ids)
	if err != nil {
		return jmapMailboxes{}, false, err
	}
	if len(counts.NotFound) > 0 {
		return jmapMailboxes{}, false, nil
	}
	err = o.withSyncStore(ctx, func(db *storage.DB) error {
		for _, mailbox := range counts.List {
			if folderID := mailboxes.folders[mailbox.ID]; folderID != "" {
				if err := db.SetFolderCounts(ctx, folderID, mailbox.TotalEmails, mailbox.UnreadEmails); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return jmapMailboxes{}, false, err
	}
	return mailboxes, true, nil
}

// jmapCountsOnly reports whether the updated properties of a Mailbox/changes
// are counters only. Without them, anything may have changed.
func jmapCountsOnly(properties []string) bool {
	if properties == nil {
		return false
	}
	for _, property := range properties {
		if !slices.Contains(jmap.MailboxCountProperties, property) {
			return false
		}
	}
	return true
}

// withSyncStore runs fn on the database the account is synced into.
func (o *SyncOrchestrator) withSyncStore(ctx context.Context, fn func(*storage.DB) error) error {
	if o.imapScope != nil {
		return o.imapScope.call(ctx, fn)
	}
	return fn(o.db)
}

// syncJMAPFolders stores the account's mailboxes as folders. A mailbox keeps
// its folder through renames and moves, since folders are matched by mailbox
// id; mailboxes the server no longer lists go through the usual folder
// reconciliation.
func (o *SyncOrchestrator) syncJMAPFolders(ctx context.Context, accountID string, target jmapTarget, mailboxes []jmap.Mailbox) error {
	byID := make(map[string]jmap.Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		if strings.TrimSpace(mailbox.ID) != "" {
			byID[mailbox.ID] = mailbox
		}
	}
	ordered := make([]jmap.Mailbox, 0, len(byID))
	for _, mailbox := range byID {
		ordered = append(ordered, mailbox)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := jmapFolderSortOrder(jmapFolderRole(ordered[i]), 0), jmapFolderSortOrder(jmapFolderRole(ordered[j]), 0)
		if ri != rj {
			return ri < rj
		}
		if ordered[i].SortOrder != ordered[j].SortOrder {
			return ordered[i].SortOrder < ordered[j].SortOrder
		}
		return strings.ToLower(jmapMailboxPath(ordered[i], byID)) < strings.ToLower(jmapMailboxPath(ordered[j], byID))
	})

	inputs := make([]storage.UpsertFolderInput, 0, len(ordered))
	seen := make([]string, 0, len(ordered))
	for i, mailbox := range ordered {
		role := jmapFolderRole(mailbox)
		parentID := ""
		if _, ok := byID[mailbox.ParentID]; ok {
			parentID = jmapLocalFolderID(accountID, target.ref(mailbox.ParentID))
		}
		path := jmapMailboxPath(mailbox, byID)
		input := storage.UpsertFolderInput{
			ID:               jmapLocalFolderID(accountID, target.ref(mailbox.ID)),
			AccountID:        accountID,
			ParentID:         parentID,
			RemoteID:         path,
			ProviderRemoteID: target.ref(mailbox.ID),
			Name:             jmapFolderName(mailbox, role),
			Icon:             imap.RoleIcon(role),
			Role:             role,
			Selectable:       mailbox.MyRights.MayReadItems,
			SortOrder:        jmapFolderSortOrder(role, i),
			CountsKnown:      true,
			TotalCount:       mailbox.TotalEmails,
			UnreadCount:      mailbox.UnreadEmails,
		}
		if target.shared() {
			// A shared mailbox's folders are custom folders of the account,
			// grouped under the mailbox, as IMAP's shared folders are. Their
			// role in the mailbox finds its Sent, Drafts and Trash.
			input.RemoteID = target.mailbox + "/" + path
			input.Role = "custom"
			input.Name = strings.TrimSpace(mailbox.Name)
			input.SharedMailbox = target.mailbox
			if role != "custom" {
				input.SharedRole = role
			}
			input.SortOrder = jmapSharedFolderSortBase + i
		}
		inputs = append(inputs, input)
		seen = append(seen, target.ref(mailbox.ID))
	}
	repo := o.outlookRepository()
	if len(inputs) > 0 {
		if err := repo.UpsertFolders(ctx, inputs); err != nil {
			return err
		}
	}
	// The other JMAP accounts' folders are not this listing's to change.
	err := o.withSyncStore(ctx, func(db *storage.DB) error {
		listed, err := db.ListListedProviderFolders(ctx, accountID)
		if err != nil {
			return err
		}
		for _, folder := range listed {
			if _, ok := target.owns(folder.ProviderRemoteID); !ok {
				seen = append(seen, folder.ProviderRemoteID)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return o.reconcileJMAPFolders(ctx, accountID, seen)
}

// jmapSharedFolderSortBase puts shared mailboxes' folders after the
// account's own, as IMAP does.
const jmapSharedFolderSortBase = 10000

// reconcileJMAPFolders puts aside the account's JMAP folders whose mailbox
// is not among seen, the provider ids of every mailbox still listed.
func (o *SyncOrchestrator) reconcileJMAPFolders(ctx context.Context, accountID string, seen []string) error {
	repo := o.outlookRepository()
	if err := repo.MarkUnlistedProviderFoldersNonSelectable(ctx, accountID, seen); err != nil {
		return err
	}
	result, err := repo.ReconcileDiscoveredFolders(ctx, accountID, storage.FolderDiscoveryJMAP, seen, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("reconcile JMAP folders: %w", err)
	}
	if result.Changed() {
		log.Printf("reconciled JMAP folders %s: missing=%d removed=%d recovered=%d", accountID, len(result.MissingIDs), len(result.RemovedIDs), len(result.RecoveredIDs))
		o.publishFolderReconciliationChange(accountID, result)
	}
	return nil
}

func jmapLocalFolderID(accountID, mailboxID string) string {
	return storage.FolderIDForIdentity(accountID, "jmap", mailboxID)
}

// jmapFolderRole maps a mailbox role (RFC 8621 section 2, from the IANA IMAP
// mailbox name attributes) to Gofer's folder roles. Roles Gofer has no folder
// kind for, such as "all" or "important", are custom folders.
func jmapFolderRole(mailbox jmap.Mailbox) string {
	switch strings.ToLower(strings.TrimSpace(mailbox.Role)) {
	case "inbox":
		return "inbox"
	case "sent":
		return "sent"
	case "drafts":
		return "drafts"
	case "trash":
		return "trash"
	case "junk":
		return "junk"
	case "archive":
		return "archive"
	default:
		return "custom"
	}
}

// jmapFolderName shows special mailboxes with Gofer's usual names, whatever
// the server calls them, and others with their own name.
func jmapFolderName(mailbox jmap.Mailbox, role string) string {
	if role == "custom" {
		return strings.TrimSpace(mailbox.Name)
	}
	return displayName(mailbox.Name, role)
}

// jmapMailboxPath is the mailbox's name under its parents, such as
// "Projects/2026". A parent loop the server should never send is cut short.
func jmapMailboxPath(mailbox jmap.Mailbox, byID map[string]jmap.Mailbox) string {
	parts := []string{strings.TrimSpace(mailbox.Name)}
	seen := map[string]bool{mailbox.ID: true}
	for parent, ok := byID[mailbox.ParentID]; ok && !seen[parent.ID]; parent, ok = byID[parent.ParentID] {
		seen[parent.ID] = true
		parts = append([]string{strings.TrimSpace(parent.Name)}, parts...)
	}
	return strings.Join(parts, "/")
}

func jmapFolderSortOrder(role string, fallback int) int {
	return outlookGraphFolderSortOrder(role, fallback)
}

// jmapSharedMailboxNames asks, in one request, for each shared mailbox's
// own sender identity, whose name is what the server calls the mailbox, as
// Stalwart gives a group's description. A mailbox the login can't submit
// from, or one the server names nothing, is left out.
func jmapSharedMailboxNames(ctx context.Context, client *jmap.Client, session *jmap.Session, targets []jmapTarget) map[string]string {
	var calls []jmap.Invocation
	asked := map[string]jmapTarget{}
	for i, target := range targets {
		if _, ok := session.Accounts[target.account].AccountCapabilities[jmap.CapabilitySubmission]; !ok {
			continue
		}
		callID := strconv.Itoa(i)
		asked[callID] = target
		calls = append(calls, jmap.Invocation{Name: "Identity/get", Args: map[string]any{"accountId": target.account, "ids": nil}, CallID: callID})
	}
	if len(calls) == 0 {
		return nil
	}
	if most := session.Core.MaxCallsInRequest; most > 0 && len(calls) > most {
		calls = calls[:most]
	}
	responses, err := client.Call(ctx, session, []string{jmap.CapabilityMail, jmap.CapabilitySubmission}, calls...)
	if err != nil {
		log.Printf("jmap shared mailbox names: %v", err)
		return nil
	}
	names := map[string]string{}
	for _, call := range calls {
		var got jmap.GetResponse[jmap.Identity]
		if responses.Get(call.CallID, &got) != nil {
			continue
		}
		target := asked[call.CallID]
		for _, identity := range got.List {
			if strings.EqualFold(strings.TrimSpace(identity.Email), target.mailbox) && strings.TrimSpace(identity.Name) != "" {
				names[target.mailbox] = strings.TrimSpace(identity.Name)
				break
			}
		}
	}
	return names
}
