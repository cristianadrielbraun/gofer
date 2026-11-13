package handler

import (
	"context"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// applyJMAPMessageMutation sends one queued change of a JMAP account's email.
// The queue worker records it like other providers' changes: a move advances
// to its destination here, and a delete the server no longer has is done.
func (h *Handler) applyJMAPMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	cfg, err := h.accountStore.GetConfig(ctx, info.AccountID)
	if err != nil {
		return err
	}
	password, err := h.accountStore.DecryptPassword(ctx, info.AccountID)
	if err != nil {
		return err
	}
	request := mail.JMAPMutation{Kind: mutation.Kind, TargetValue: mutation.TargetValue, EmailID: info.RemoteMessageID, FolderCopyIDs: info.FolderCopyIDs}
	if mutation.Kind == storage.MessageMutationMove {
		if request.FromMailboxID, err = h.db.GetFolderProviderRemoteID(ctx, mutation.FolderID); err != nil {
			return fmt.Errorf("load JMAP source mailbox: %w", err)
		}
		if request.ToMailboxID, err = h.db.GetFolderProviderRemoteID(ctx, mutation.DestinationFolderID); err != nil {
			return fmt.Errorf("load JMAP destination mailbox: %w", err)
		}
	}
	gone, err := mail.ApplyJMAPMutation(ctx, cfg, password, request)
	switch {
	case err != nil:
		return err
	case gone && mutation.Kind == storage.MessageMutationDelete:
		return errProviderDeleteAlreadyGone
	case mutation.Kind == storage.MessageMutationMove && !gone:
		return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, request.EmailID, 0)
	}
	return nil
}

// setJMAPMessageKeyword adds or removes a keyword label on a JMAP email. The
// label name maps to its keyword as on IMAP: through the account's aliases,
// or for a removal, the keyword the email's label was synced with.
func (h *Handler) setJMAPMessageKeyword(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string, add bool) (storage.LabelInput, error) {
	var keyword string
	var err error
	if add {
		keyword, err = h.keywordForAccountLabel(ctx, info.AccountID, labelName)
	} else {
		keyword, err = h.keywordForMessageLabel(ctx, messageID, info.AccountID, labelName)
	}
	if err != nil {
		return storage.LabelInput{}, err
	}
	cfg, err := h.accountStore.GetConfig(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	password, err := h.accountStore.DecryptPassword(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	if _, err := mail.SetJMAPKeyword(ctx, cfg, password, info.LabelCopyIDs(info.RemoteMessageID), keyword, add); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         labelName,
		ProviderID:   keyword,
		ProviderType: storage.LabelProviderKeyword,
	}, nil
}
