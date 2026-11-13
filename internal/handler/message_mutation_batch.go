package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

// messageMutationBatchLimit is how many changes the worker sends to a
// provider together.
const messageMutationBatchLimit = 500

// messageMutationBatchKey groups claimed changes that can share one request.
func messageMutationBatchKey(m storage.MessageMutation) string {
	return strings.Join([]string{m.AccountID, m.ProviderType, m.Kind, fmt.Sprint(m.TargetValue), m.FolderID, fmt.Sprint(m.SourceUIDValidity)}, "\x00")
}

// applyQueuedMessageMutationBatch sends claimed changes of one account,
// folder, kind and value, with any further due ones, to the provider in one
// request. Read and starred go as one IMAP STORE over their UIDs, Email/set
// calls naming many emails on JMAP, messages.batchModify on Gmail and Graph
// $batch on Outlook; permanent deletes as one IMAP expunge, Email/set
// destroys, messages.batchDelete and $batch deletes. A change whose message
// must be looked up on the server first, or that a batch cannot carry, goes
// one by one, and one whose message is gone is dropped.
func (h *Handler) applyQueuedMessageMutationBatch(parent context.Context, group []storage.MessageMutation) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	lead := group[0]
	if more, err := h.db.ClaimMessageMutationBatch(ctx, lead, time.Now(), messageMutationBatchLimit-len(group)); err != nil {
		log.Printf("message-mutation: claim %s batch: %v", lead.Kind, err)
	} else {
		group = append(group, more...)
	}
	deleting := lead.Kind == storage.MessageMutationDelete

	var grouped []storage.MessageMutation
	var infos []storage.MessageMutationInfo
	for _, m := range group {
		// Looked up as applyRemoteMessageMutation does for each kind.
		var info *storage.MessageMutationInfo
		var err error
		switch {
		case deleting:
			info, err = h.db.GetMessageMutationInfoIncludingDeletedInFolder(ctx, m.MessageID, m.FolderID)
		case m.FolderID == "":
			info, err = h.db.GetMessageMutationInfoInternal(ctx, m.MessageID)
		default:
			info, err = h.db.GetMessageMutationInfoInFolder(ctx, m.MessageID, m.FolderID)
		}
		switch {
		case err != nil:
			h.finishQueuedMessageMutation(m, err, false)
		case info == nil:
			if err := h.db.DiscardMessageMutation(ctx, m.ID); err != nil {
				log.Printf("message-mutation: discard id=%s: %v", m.ID, err)
			}
		case info.AccountID != m.AccountID:
			h.finishQueuedMessageMutation(m, errors.New("message account changed"), false)
		case messageMutationProvider(info.AccountProvider) != m.ProviderType:
			h.finishQueuedMessageMutation(m, fmt.Errorf("message provider changed from %s", m.ProviderType), false)
		case m.ProviderType == storage.MessageMutationProviderIMAP && info.RemoteUID == 0,
			m.ProviderType != storage.MessageMutationProviderIMAP && strings.TrimSpace(info.RemoteMessageID) == "":
			h.applyQueuedMessageMutation(ctx, m)
		case m.ProviderType == storage.MessageMutationProviderIMAP && strings.TrimSpace(info.FolderRemoteID) == "":
			h.finishQueuedMessageMutation(m, errors.New("message has no remote IMAP folder identity"), false)
		default:
			grouped = append(grouped, m)
			infos = append(infos, *info)
		}
	}
	if len(grouped) == 0 {
		return
	}

	var failures []error
	gone := make([]bool, len(grouped))
	switch lead.ProviderType {
	case storage.MessageMutationProviderIMAP:
		failures = h.sendIMAPBatch(ctx, lead, infos)
	case storage.MessageMutationProviderJMAP:
		failures, gone = h.sendJMAPBatch(ctx, lead, infos)
	case storage.MessageMutationProviderGmail, storage.MessageMutationProviderOutlook:
		failures, gone = h.sendProviderBatch(ctx, lead, infos)
	default:
		failures = sameMutationError(len(grouped), fmt.Errorf("unsupported batched provider %q", lead.ProviderType))
	}
	mail.LogRefusedBatch(lead.AccountID, lead.ProviderType, lead.Kind, failures)
	for i, m := range grouped {
		if errors.Is(failures[i], mail.ErrBatchRefused) {
			h.applyQueuedMessageMutation(ctx, m)
			continue
		}
		h.finishQueuedMessageMutation(m, failures[i], gone[i])
	}
}

// finishQueuedMessageMutation records a change's result as
// applyQueuedMessageMutation does: applied, with a delete the provider no
// longer had confirmed, or failed with a retry.
func (h *Handler) finishQueuedMessageMutation(m storage.MessageMutation, applyErr error, alreadyGone bool) {
	if applyErr == nil {
		complete := h.db.CompleteMessageMutation
		if m.Kind == storage.MessageMutationDelete {
			complete = h.db.CompleteMessageDeleteMutation
		}
		applyErr = complete(context.Background(), m.ID)
		if applyErr == nil {
			if alreadyGone && m.Kind == storage.MessageMutationDelete {
				if err := h.db.ConfirmMessageDeleteMutation(context.Background(), m.ID); err != nil {
					log.Printf("message-mutation: confirm already deleted id=%s: %v", m.ID, err)
				}
			}
			return
		}
		applyErr = errors.New("Provider update succeeded, but Gofer could not save the result: " + applyErr.Error())
	}
	nextAttempt := time.Now().Add(sentCopyRetryDelay(m.AttemptCount))
	if err := h.db.FinishMessageMutationWithError(context.Background(), m.ID, applyErr.Error(), nextAttempt); err != nil {
		log.Printf("message-mutation: save failure id=%s: %v", m.ID, err)
		return
	}
	log.Printf("message-mutation: %s message=%d failed; retry at %s: %v", m.Kind, m.MessageID, nextAttempt.Format(time.RFC3339), applyErr)
}

// sendProviderBatch sends a Gmail or Outlook batch with the account's token,
// through the request helpers single changes use. gone[i] is set for a
// delete whose message the provider no longer had.
func (h *Handler) sendProviderBatch(ctx context.Context, lead storage.MessageMutation, infos []storage.MessageMutationInfo) (failures []error, gone []bool) {
	gone = make([]bool, len(infos))
	if h.mailCredentials() == nil {
		return sameMutationError(len(infos), fmt.Errorf("%s authentication is not available", lead.ProviderType)), gone
	}
	messages := make([]mail.ProviderBatchMessage, len(infos))
	for i, info := range infos {
		messages[i] = mail.ProviderBatchMessage{IDs: append([]string{strings.TrimSpace(info.RemoteMessageID)}, info.FolderCopyIDs...)}
	}
	deleting := lead.Kind == storage.MessageMutationDelete
	if lead.ProviderType == storage.MessageMutationProviderGmail {
		token, err := h.mailCredentials().GetOAuthTokenForAccount(ctx, lead.AccountID)
		if err != nil {
			return sameMutationError(len(infos), fmt.Errorf("get Gmail token: %w", err)), gone
		}
		request := func(method, endpoint string, body, out any) error {
			err := doGoogleJSON(ctx, method, endpoint, token, body, out)
			if apiErr, ok := err.(googleAPIError); ok {
				return mail.ProviderStatusError(apiErr.Status, err)
			}
			return err
		}
		if deleting {
			return mail.SendGmailDeleteBatch(request, gmailAPIBaseURL, messages), gone
		}
		return mail.SendGmailFlagBatch(request, gmailAPIBaseURL, lead.Kind, lead.TargetValue, messages), gone
	}
	token, err := h.mailCredentials().GetMicrosoftGraphMailTokenForAccount(ctx, lead.AccountID)
	if err != nil {
		return sameMutationError(len(infos), fmt.Errorf("get Outlook token: %w", err)), gone
	}
	request := func(method, endpoint string, body, out any) error {
		err := h.doOutlookJSON(ctx, method, endpoint, token, body, out)
		if apiErr, ok := err.(outlookAPIError); ok {
			return mail.ProviderStatusError(apiErr.Status, err)
		}
		return err
	}
	if deleting {
		return mail.SendOutlookDeleteBatch(request, outlookGraphBaseURL, messages)
	}
	return mail.SendOutlookFlagBatch(request, outlookGraphBaseURL, lead.Kind, lead.TargetValue, messages), gone
}

// sendIMAPBatch sets or clears a flag on every message's UID with one login
// and one STORE, or deletes them all for good with one expunge, checking
// UIDVALIDITY as a single delete does. They succeed or fail together.
func (h *Handler) sendIMAPBatch(ctx context.Context, lead storage.MessageMutation, infos []storage.MessageMutationInfo) []error {
	uids := make([]uint32, len(infos))
	for i, info := range infos {
		uids[i] = info.RemoteUID
	}
	err := func() error {
		if h.accountStore == nil {
			return errors.New("IMAP account storage is not available")
		}
		validity := lead.SourceUIDValidity
		if lead.Kind == storage.MessageMutationDelete && validity == 0 {
			var err error
			if validity, err = h.db.GetFolderUIDValidity(ctx, lead.FolderID); err != nil {
				return err
			}
			if validity == 0 {
				return errors.New("IMAP UIDVALIDITY is unavailable; waiting for folder sync before deleting")
			}
		}
		cfg, err := h.accountStore.GetConfig(ctx, lead.AccountID)
		if err != nil {
			return err
		}
		password, err := h.resolvePassword(ctx, cfg, lead.AccountID)
		if err != nil {
			return err
		}
		factory := h.messageMutationIMAPFactory
		if factory == nil {
			factory = func(ctx context.Context, cfg *models.AccountConfig, password string) (messageMutationIMAPClient, error) {
				return imap.NewClient(ctx, cfg, password)
			}
		}
		client, err := factory(ctx, cfg, password)
		if err != nil {
			return err
		}
		defer client.Close()
		if lead.Kind == storage.MessageMutationDelete {
			changed, err := client.DeleteMessagesIfUIDValidity(ctx, infos[0].FolderRemoteID, uids, validity)
			if err == nil && changed {
				err = errors.New("IMAP UIDVALIDITY changed; waiting for folder resync before deleting")
			}
			return err
		}
		op, flag := goimap.StoreFlagsDel, goimap.FlagSeen
		if lead.TargetValue {
			op = goimap.StoreFlagsAdd
		}
		if lead.Kind == storage.MessageMutationStarred {
			flag = goimap.FlagFlagged
		}
		return client.StoreFlagsBatch(ctx, infos[0].FolderRemoteID, uids, op, []goimap.Flag{flag})
	}()
	if err != nil {
		return sameMutationError(len(infos), err)
	}
	return make([]error, len(infos))
}

// sendJMAPBatch sends a read, starred or delete change for every message's
// emails in as few Email/set requests as the server allows; each message
// gets its own result, and gone[i] is set for a delete the server no longer
// had.
func (h *Handler) sendJMAPBatch(ctx context.Context, lead storage.MessageMutation, infos []storage.MessageMutationInfo) (failures []error, gone []bool) {
	gone = make([]bool, len(infos))
	cfg, err := h.accountStore.GetConfig(ctx, lead.AccountID)
	if err != nil {
		return sameMutationError(len(infos), err), gone
	}
	password, err := h.accountStore.DecryptPassword(ctx, lead.AccountID)
	if err != nil {
		return sameMutationError(len(infos), err), gone
	}
	emails := make([]mail.JMAPBatchEmail, len(infos))
	for i, info := range infos {
		emails[i] = mail.JMAPBatchEmail{EmailID: info.RemoteMessageID, FolderCopyIDs: info.FolderCopyIDs}
	}
	failures, jmapGone, err := mail.ApplyJMAPBatch(ctx, cfg, password, lead.Kind, lead.TargetValue, emails)
	if err != nil {
		return sameMutationError(len(infos), err), gone
	}
	if lead.Kind == storage.MessageMutationDelete {
		gone = jmapGone
	}
	return failures, gone
}

func sameMutationError(n int, err error) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
}
