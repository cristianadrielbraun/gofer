package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) queueUserLabels(ctx context.Context, targets []ownedMessageTarget, name, operation string) labelMutationResult {
	var infos []storage.ThreadMessageMutationInfo
	for _, target := range targets {
		for _, info := range target.Infos {
			// Reject invalid keyword names before saving any part of the request.
			if info.AccountProvider == "imap" {
				if _, err := h.imapKeywordForAccountLabel(ctx, info.AccountID, name); err != nil {
					return labelMutationResult{Failed: len(targets)}
				}
			}
			infos = append(infos, info)
		}
	}
	if err := h.db.QueueUserLabels(ctx, h.userMutationState.owner, infos, name, operation); err != nil {
		return labelMutationResult{Failed: len(infos)}
	}
	for _, target := range targets {
		h.publishThreadMutation(target.Infos)
	}
	return labelMutationResult{Updated: len(targets), Messages: len(infos)}
}
