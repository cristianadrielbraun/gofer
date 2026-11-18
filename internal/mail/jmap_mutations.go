package mail

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// JMAPMutation is one queued change to a JMAP email, with the server ids it
// needs: the email, and for a move the mailboxes it leaves and enters.
// FolderCopyIDs are further emails of the same message in that mailbox, which
// the change reaches too.
type JMAPMutation struct {
	Kind          string
	TargetValue   bool
	EmailID       string
	FolderCopyIDs []string
	FromMailboxID string
	ToMailboxID   string
}

// ApplyJMAPMutation sends one queued change to the JMAP server as a single
// Email/set. Read and starred set or clear one keyword; a move swaps one
// mailbox for another, leaving others the email is in; a delete destroys the
// email for good, as Gofer's delete does for Gmail and Outlook. It reports
// gone when the server has none of the emails any more, which needs no retry.
func ApplyJMAPMutation(ctx context.Context, cfg *models.AccountConfig, password string, m JMAPMutation) (gone bool, err error) {
	gone, _, err = ApplyJMAPMutationMoving(ctx, cfg, password, m)
	return gone, err
}

// ApplyJMAPMutationMoving is ApplyJMAPMutation that also gives the email's
// new stored id when a move took it into another JMAP account, as between the
// login's own mailbox and one shared with it, where the server makes a new
// email; it is "" when the email kept its id.
func ApplyJMAPMutationMoving(ctx context.Context, cfg *models.AccountConfig, password string, m JMAPMutation) (gone bool, movedID string, err error) {
	if m.EmailID == "" {
		return false, "", errors.New("JMAP email identity is unavailable")
	}
	client, session, err := openJMAPSession(ctx, cfg, password)
	if err != nil {
		return false, "", err
	}
	account, emailIDs, err := jmapIDsIn(session, append([]string{m.EmailID}, m.FolderCopyIDs...))
	if err != nil {
		return false, "", err
	}
	if m.Kind == storage.MessageMutationMove {
		if m.ToMailboxID == "" {
			return false, "", errors.New("JMAP move destination has no mailbox identity")
		}
		toAccount, toMailbox, err := jmapAccountOf(session, m.ToMailboxID)
		if err != nil {
			return false, "", err
		}
		_, from, _ := jmapAccountOf(session, m.FromMailboxID)
		if toAccount != account {
			return moveJMAPEmailAcross(ctx, client, session, account, toAccount, emailIDs, from, toMailbox)
		}
		m.FromMailboxID, m.ToMailboxID = from, toMailbox
	}
	gone, err = applyJMAPMutationIn(ctx, client, session, account, emailIDs, m)
	return gone, "", err
}

// moveJMAPEmailAcross moves an email out of fromMailbox into a mailbox of
// another JMAP account: the server copies it there, keywords and all, and
// then the original goes, or, when it is in other mailboxes of its account
// too, only leaves fromMailbox. Further copies of the message in the mailbox
// it leaves go with it. A move tried again, after one that copied the email
// but could not remove the original, finds the copy it made and makes no
// second one.
func moveJMAPEmailAcross(ctx context.Context, client *jmap.Client, session *jmap.Session, fromAccount, toAccount string, emailIDs []string, fromMailbox, toMailbox string) (bool, string, error) {
	current, err := client.GetEmails(ctx, session, fromAccount, emailIDs[:1], []string{"keywords", "mailboxIds", "messageId", "receivedAt"})
	if err != nil {
		return false, "", err
	}
	if len(current.List) == 0 {
		return true, "", nil
	}
	original := current.List[0]
	elsewhere := false
	for mailbox, in := range original.MailboxIDs {
		if in && mailbox != fromMailbox {
			elsewhere = true
		}
	}
	moved := ""
	if len(original.MessageID) > 0 {
		window := jmap.Window{From: original.ReceivedAt.Add(-time.Minute), To: original.ReceivedAt.Add(time.Minute)}
		found, err := client.FindEmails(ctx, session, toAccount, toMailbox, original.MessageID[0], []jmap.Window{window})
		if err != nil {
			return false, "", fmt.Errorf("look for an earlier copy of the email: %w", err)
		}
		if len(found) > 0 {
			moved = found[0]
		}
	}
	if moved == "" {
		moved, err = client.CopyEmail(ctx, session, fromAccount, toAccount, emailIDs[0], toMailbox, original.Keywords)
		var exists *jmap.SetError
		switch {
		case errors.As(err, &exists) && exists.Type == "alreadyExists" && exists.ExistingID != "":
			// The other account has this message already, as a shared
			// mailbox has its own sent copy of a message sent to the login:
			// it is put in the mailbox moved to, and the original goes.
			moved = exists.ExistingID
			result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: toAccount, Update: map[string]jmap.PatchObject{
				moved: jmap.MovePatch("", toMailbox),
			}})
			if err == nil {
				err = jmapSetFailure(result.NotUpdated, []string{moved}, false)
			}
			if err != nil {
				return false, "", fmt.Errorf("put the email already there in the mailbox: %w", err)
			}
		case err != nil:
			return false, "", err
		}
	}
	ref := jmapRefIn(session, toAccount, moved)
	if elsewhere {
		// Kept for its other mailboxes, it only leaves this one, if known;
		// it is never destroyed.
		if fromMailbox != "" {
			result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: fromAccount, Update: map[string]jmap.PatchObject{
				emailIDs[0]: {"mailboxIds/" + fromMailbox: nil},
			}})
			if err == nil {
				err = jmapSetFailure(result.NotUpdated, emailIDs[:1], false)
			}
			if err != nil {
				return false, ref, fmt.Errorf("take the email out of the mailbox it left: %w", err)
			}
		}
		emailIDs = emailIDs[1:]
	}
	if len(emailIDs) > 0 {
		result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: fromAccount, Destroy: emailIDs})
		if err == nil {
			err = jmapSetFailure(result.NotDestroyed, emailIDs, true)
		}
		if err != nil {
			return false, ref, fmt.Errorf("remove the original after the move: %w", err)
		}
	}
	return false, ref, nil
}

// jmapSetFailure is the first of ids a set call did not change, as its
// response's notUpdated or notDestroyed lists them. With goneIsDone, an email
// already gone (notFound) counts as destroyed.
func jmapSetFailure(failed map[string]jmap.SetError, ids []string, goneIsDone bool) error {
	for _, id := range ids {
		failure, ok := failed[id]
		if !ok || (goneIsDone && failure.Type == "notFound") {
			continue
		}
		return fmt.Errorf("email %s: %w", id, &failure)
	}
	return nil
}

// applyJMAPMutationIn sends one change to emails of one JMAP account.
func applyJMAPMutationIn(ctx context.Context, client *jmap.Client, session *jmap.Session, account string, emailIDs []string, m JMAPMutation) (gone bool, err error) {
	var patch jmap.PatchObject
	switch m.Kind {
	case storage.MessageMutationRead:
		patch = jmap.KeywordPatch(jmap.KeywordSeen, m.TargetValue)
	case storage.MessageMutationStarred:
		patch = jmap.KeywordPatch(jmap.KeywordFlagged, m.TargetValue)
	case storage.MessageMutationMove:
		if m.ToMailboxID == "" {
			return false, errors.New("JMAP move destination has no mailbox identity")
		}
		patch = jmap.MovePatch(m.FromMailboxID, m.ToMailboxID)
	case storage.MessageMutationDelete:
	default:
		return false, fmt.Errorf("unsupported JMAP mutation %q", m.Kind)
	}
	args := jmap.EmailSet{AccountID: account}
	if m.Kind == storage.MessageMutationDelete {
		args.Destroy = emailIDs
	} else {
		args.Update = map[string]jmap.PatchObject{}
		for _, id := range emailIDs {
			args.Update[id] = patch
		}
	}
	result, err := client.SetEmails(ctx, session, args)
	if err != nil {
		return false, err
	}
	gone = true
	for _, id := range emailIDs {
		failure, failed := result.NotUpdated[id]
		if m.Kind == storage.MessageMutationDelete {
			failure, failed = result.NotDestroyed[id]
		}
		switch {
		case !failed:
			gone = false
		case failure.Type != "notFound":
			return false, &failure
		}
	}
	return gone, nil
}

// JMAPBatchEmail is one message of a batched change: its email, and the
// further emails of the same message in that mailbox.
type JMAPBatchEmail struct {
	EmailID       string
	FolderCopyIDs []string
}

// jmapDefaultObjectsInSet is used when the server does not say how many
// objects one Email/set may name.
const jmapDefaultObjectsInSet = 500

// ApplyJMAPBatch sends one read, starred or delete change for many
// messages' emails, in as few Email/set requests as the server's
// maxObjectsInSet allows: read and starred set or clear a keyword, and a
// delete destroys the emails for good, as single changes do. failures[i] is
// nil when emails[i] changed or the server no longer has it, and gone[i]
// tells the latter apart. err is set when no request could be sent at all.
func ApplyJMAPBatch(ctx context.Context, cfg *models.AccountConfig, password, kind string, target bool, emails []JMAPBatchEmail) (failures []error, gone []bool, err error) {
	client, session, err := openJMAPSession(ctx, cfg, password)
	if err != nil {
		return nil, nil, err
	}
	// Each JMAP account, the login's own and every shared one, gets its own
	// requests, with its own ids.
	failures, gone = make([]error, len(emails)), make([]bool, len(emails))
	groups := map[string][]int{}
	var order []string
	byAccount := map[string][]JMAPBatchEmail{}
	for i, email := range emails {
		account, ids, err := jmapIDsIn(session, append([]string{email.EmailID}, email.FolderCopyIDs...))
		if err != nil {
			failures[i] = err
			continue
		}
		if _, ok := groups[account]; !ok {
			order = append(order, account)
		}
		groups[account] = append(groups[account], i)
		byAccount[account] = append(byAccount[account], JMAPBatchEmail{EmailID: ids[0], FolderCopyIDs: ids[1:]})
	}
	for _, account := range order {
		groupFailures, groupGone, err := applyJMAPBatchIn(ctx, client, session, account, kind, target, byAccount[account])
		if err != nil {
			if len(order) == 1 {
				return nil, nil, err
			}
			for _, i := range groups[account] {
				failures[i] = err
			}
			continue
		}
		for n, i := range groups[account] {
			failures[i], gone[i] = groupFailures[n], groupGone[n]
		}
	}
	return failures, gone, nil
}

// applyJMAPBatchIn is ApplyJMAPBatch for emails of one JMAP account, by the
// server's ids.
func applyJMAPBatchIn(ctx context.Context, client *jmap.Client, session *jmap.Session, account, kind string, target bool, emails []JMAPBatchEmail) (failures []error, gone []bool, err error) {
	var patch jmap.PatchObject
	switch kind {
	case storage.MessageMutationRead:
		patch = jmap.KeywordPatch(jmap.KeywordSeen, target)
	case storage.MessageMutationStarred:
		patch = jmap.KeywordPatch(jmap.KeywordFlagged, target)
	case storage.MessageMutationDelete:
	default:
		return nil, nil, fmt.Errorf("unsupported JMAP batch mutation %q", kind)
	}
	limit := session.Core.MaxObjectsInSet
	if limit <= 0 {
		limit = jmapDefaultObjectsInSet
	}
	failures = make([]error, len(emails))
	gone = make([]bool, len(emails))
	var ids []string
	first := 0
	send := func(end int) {
		args := jmap.EmailSet{AccountID: account}
		if kind == storage.MessageMutationDelete {
			args.Destroy = ids
		} else {
			args.Update = map[string]jmap.PatchObject{}
			for _, id := range ids {
				args.Update[id] = patch
			}
		}
		result, err := client.SetEmails(ctx, session, args)
		for i := first; i < end; i++ {
			if err != nil {
				failures[i] = err
				continue
			}
			gone[i] = true
			for _, id := range append([]string{emails[i].EmailID}, emails[i].FolderCopyIDs...) {
				failure, failed := result.NotUpdated[id]
				if kind == storage.MessageMutationDelete {
					failure, failed = result.NotDestroyed[id]
				}
				switch {
				case !failed:
					gone[i] = false
				case failure.Type != "notFound":
					failures[i], gone[i] = &failure, false
				}
				if failures[i] != nil {
					break
				}
			}
		}
		ids = nil
	}
	for i, email := range emails {
		emailIDs := append([]string{email.EmailID}, email.FolderCopyIDs...)
		if len(ids) > 0 && len(ids)+len(emailIDs) > limit {
			send(i)
			first = i
		}
		ids = append(ids, emailIDs...)
	}
	if len(ids) > 0 {
		send(len(emails))
	}
	return failures, gone, nil
}

// openJMAPMail opens the account's JMAP session and gives its mail account.
func openJMAPMail(ctx context.Context, cfg *models.AccountConfig, password string) (*jmap.Client, *jmap.Session, string, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, nil, "", err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("JMAP session: %w", err)
	}
	account := session.PrimaryAccount(jmap.CapabilityMail)
	if account == "" {
		return nil, nil, "", errors.New("the JMAP server offers no mail account for this login")
	}
	return client, session, account, nil
}

// JMAPSpamKeyword gives the JMAP spelling of a spam training keyword, which
// IMAP spells $Junk and $NotJunk, and reports whether name is one.
func JMAPSpamKeyword(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "$junk":
		return "$junk", true
	case "$notjunk":
		return "$notjunk", true
	}
	return "", false
}

// SetJMAPKeyword adds a keyword label to JMAP emails, the copies of one
// message, or removes it. Servers may match keywords exactly, so a removal
// reads the emails' keywords first and clears the keyword in every casing
// each email carries. It reports gone when the server has none of the emails
// any more, which needs no retry.
func SetJMAPKeyword(ctx context.Context, cfg *models.AccountConfig, password string, emailIDs []string, keyword string, add bool) (gone bool, err error) {
	if len(emailIDs) == 0 || emailIDs[0] == "" {
		return false, errors.New("JMAP email identity is unavailable")
	}
	client, session, err := openJMAPSession(ctx, cfg, password)
	if err != nil {
		return false, err
	}
	account, emailIDs, err := jmapIDsIn(session, emailIDs)
	if err != nil {
		return false, err
	}
	update := map[string]jmap.PatchObject{}
	if add {
		for _, id := range emailIDs {
			update[id] = jmap.KeywordPatch(keyword, true)
		}
	} else {
		current, err := client.GetEmails(ctx, session, account, emailIDs, []string{"keywords"})
		if err != nil {
			return false, err
		}
		if len(current.List) == 0 {
			return true, nil
		}
		for _, email := range current.List {
			patch := jmap.PatchObject{}
			for name, on := range email.Keywords {
				if on && strings.EqualFold(name, keyword) {
					maps.Copy(patch, jmap.KeywordPatch(name, false))
				}
			}
			if len(patch) > 0 {
				update[email.ID] = patch
			}
		}
		if len(update) == 0 {
			return false, nil
		}
	}
	result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: account, Update: update})
	if err != nil {
		return false, err
	}
	gone = true
	for id := range update {
		failure, failed := result.NotUpdated[id]
		switch {
		case !failed:
			gone = false
		case failure.Type != "notFound":
			return false, &failure
		}
	}
	return gone, nil
}

// applyJMAPMutation replays one queued change of a JMAP account in a routed
// worker, then records it as IMAP changes are recorded.
func (s *UserIMAP) applyJMAPMutation(ctx context.Context, scope *userIMAPScope, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if mutation.AccountID != scope.id || info.AccountID != scope.id || mutation.ProviderType != storage.MessageMutationProviderJMAP || info.AccountProvider != "jmap" {
		return errors.New("queued mutation account or provider changed")
	}
	request := JMAPMutation{Kind: mutation.Kind, TargetValue: mutation.TargetValue, EmailID: info.RemoteMessageID, FolderCopyIDs: info.FolderCopyIDs}
	if mutation.Kind == storage.MessageMutationMove {
		if err := scope.call(ctx, func(db *storage.DB) error {
			var err error
			if request.FromMailboxID, err = db.GetFolderProviderRemoteID(ctx, mutation.FolderID); err != nil {
				return err
			}
			request.ToMailboxID, err = db.GetFolderProviderRemoteID(ctx, mutation.DestinationFolderID)
			return err
		}); err != nil {
			return err
		}
	}
	gone, movedID, err := ApplyJMAPMutationMoving(ctx, scope.config, scope.password, request)
	if err != nil {
		return err
	}
	if movedID != "" {
		request.EmailID = movedID
	}
	return scope.call(ctx, func(db *storage.DB) error {
		switch {
		case mutation.Kind == storage.MessageMutationDelete:
			if err := db.CompleteMessageDeleteMutation(ctx, mutation.ID); err != nil {
				return err
			}
			if gone {
				return db.ConfirmMessageDeleteMutation(ctx, mutation.ID)
			}
			return nil
		case mutation.Kind == storage.MessageMutationMove && !gone:
			// JMAP ids stay the same when an email changes mailbox.
			if err := db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, request.EmailID, 0); err != nil {
				return err
			}
		}
		return db.CompleteMessageMutation(ctx, mutation.ID)
	})
}
