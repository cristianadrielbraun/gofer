package mail

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
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
	FlagColor     keywords.FlagColor
	EmailID       string
	FolderCopyIDs []string
	FromMailboxID string
	ToMailboxID   string
}

// ApplyJMAPMutation sends one queued change to the JMAP server as a single
// Email/set. Read and starred patch their keywords; a move swaps one
// mailbox for another, leaving others the email is in; a delete destroys the
// email for good, as Gofer's delete does for Gmail and Outlook. It reports
// gone when the server has none of the emails any more, which needs no retry.
func ApplyJMAPMutation(ctx context.Context, cfg *models.AccountConfig, password string, m JMAPMutation) (gone bool, err error) {
	if m.EmailID == "" {
		return false, errors.New("JMAP email identity is unavailable")
	}
	client, session, account, err := openJMAPMail(ctx, cfg, password)
	if err != nil {
		return false, err
	}
	emailIDs := append([]string{m.EmailID}, m.FolderCopyIDs...)
	var patch jmap.PatchObject
	switch m.Kind {
	case storage.MessageMutationRead:
		patch = jmap.KeywordPatch(jmap.KeywordSeen, m.TargetValue)
	case storage.MessageMutationStarred:
		patch = jmapStarredPatch(m.TargetValue, m.FlagColor)
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
// maxObjectsInSet allows: read and starred patch their keywords, and a
// delete destroys the emails for good, as single changes do. failures[i] is
// nil when emails[i] changed or the server no longer has it, and gone[i]
// tells the latter apart. err is set when no request could be sent at all.
func ApplyJMAPBatch(ctx context.Context, cfg *models.AccountConfig, password string, mutation storage.MessageMutation, emails []JMAPBatchEmail) (failures []error, gone []bool, err error) {
	kind, target := mutation.Kind, mutation.TargetValue
	var patch jmap.PatchObject
	switch kind {
	case storage.MessageMutationRead:
		patch = jmap.KeywordPatch(jmap.KeywordSeen, target)
	case storage.MessageMutationStarred:
		patch = jmapStarredPatch(target, mutation.TargetFlagColor)
	case storage.MessageMutationDelete:
	default:
		return nil, nil, fmt.Errorf("unsupported JMAP batch mutation %q", kind)
	}
	client, session, account, err := openJMAPMail(ctx, cfg, password)
	if err != nil {
		return nil, nil, err
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

func jmapStarredPatch(starred bool, color keywords.FlagColor) jmap.PatchObject {
	patch := jmap.KeywordPatch(jmap.KeywordFlagged, starred)
	if starred && color == 0 {
		return patch
	}
	for i, name := range keywords.FlagBitKeywords() {
		on := starred && color.Valid() && int(color)&(1<<i) != 0
		maps.Copy(patch, jmap.KeywordPatch(strings.ToLower(name), on))
	}
	return patch
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
	client, session, account, err := openJMAPMail(ctx, cfg, password)
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
	request := JMAPMutation{Kind: mutation.Kind, TargetValue: mutation.TargetValue, FlagColor: mutation.TargetFlagColor, EmailID: info.RemoteMessageID, FolderCopyIDs: info.FolderCopyIDs}
	if mutation.IsScopedFlagColor() {
		request.FolderCopyIDs = nil
	}
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
	gone, err := ApplyJMAPMutation(ctx, scope.config, scope.password, request)
	if err != nil {
		return err
	}
	return scope.call(ctx, func(db *storage.DB) error {
		switch {
		case gone && mutation.TargetFlagCopy != "":
			return db.DiscardMessageMutation(ctx, mutation.ID)
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
