package handler

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/google/uuid"
)

// jmapDrafts opens a JMAP account's drafts with its stored login.
func (h *Handler) jmapDrafts(ctx context.Context, accountID string) (*mail.JMAPDrafts, error) {
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return nil, err
	}
	password, err := h.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return mail.NewJMAPDrafts(cfg, password), nil
}

// saveJMAPDraft saves a draft to the JMAP account's Drafts mailbox, replacing
// the version saved before, and records the new version's id on the local
// draft. A version that could not be removed stays on the server; the draft
// itself is saved, and the error says so.
func (h *Handler) saveJMAPDraft(ctx context.Context, accountID string, localMessageID int64, msg *message.OutgoingMessage) error {
	drafts, err := h.jmapDrafts(ctx, accountID)
	if err != nil {
		return err
	}
	raw, err := message.BuildMIMEMessageForIMAPDraft(msg, uuid.NewString())
	if err != nil {
		return err
	}
	previous := ""
	if existing, err := h.db.GetDraftProviderInfoInternal(ctx, accountID, msg.MessageID); err == nil && existing != nil {
		previous = strings.TrimSpace(existing.ProviderMessageID)
	}
	id, saveErr := drafts.ReplaceDraft(ctx, previous, raw)
	if id == "" {
		return saveErr
	}
	if err := h.db.SetMessageProviderMessageID(ctx, localMessageID, id); err != nil {
		return err
	}
	return saveErr
}

// deleteJMAPDraft removes a draft from the JMAP server, provided it is still
// the draft with that Message-ID.
func (h *Handler) deleteJMAPDraft(ctx context.Context, accountID, draftKey, emailID string) error {
	if strings.TrimSpace(emailID) == "" {
		return nil
	}
	drafts, err := h.jmapDrafts(ctx, accountID)
	if err != nil {
		return err
	}
	return drafts.DeleteDraft(ctx, mail.UserProviderDraft{ID: emailID, InternetMessageID: draftKey})
}
