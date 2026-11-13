package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// fetchJMAPMessageMIME downloads the original message of a JMAP account's
// email. It reports whether the message belongs to a JMAP account at all, so
// other accounts go on to their own provider or IMAP.
func (h *Handler) fetchJMAPMessageMIME(ctx context.Context, messageID int64) ([]byte, bool, error) {
	info, err := h.db.GetMessageMutationInfoInternal(ctx, messageID)
	if err != nil || info == nil || info.AccountProvider != providers.ProviderJMAP {
		return nil, false, err
	}
	cfg, err := h.accountStore.GetConfig(ctx, info.AccountID)
	if err != nil {
		return nil, true, err
	}
	password, err := h.accountStore.DecryptPassword(ctx, info.AccountID)
	if err != nil {
		return nil, true, err
	}
	raw, err := mail.FetchJMAPRawMessage(ctx, cfg, password, info.RemoteMessageID)
	return raw, true, err
}
