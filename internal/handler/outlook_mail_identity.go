package handler

import (
	"context"
	"log"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) outlookGraphMessageIdentity(ctx context.Context, messageID int64, info storage.MessageMutationInfo, operation string) (string, string, bool) {
	if strings.TrimSpace(info.AccountProvider) != providers.ProviderOutlook || h.auth == nil {
		return "", "", false
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		log.Printf("outlook %s token account=%s message=%d: %v", operation, info.AccountID, messageID, err)
		return "", "", false
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, messageID, info)
		if err != nil {
			log.Printf("outlook %s resolve account=%s message=%d: %v", operation, info.AccountID, messageID, err)
			return "", "", false
		}
	}
	if providerMessageID == "" {
		return "", "", false
	}
	return token, providerMessageID, true
}
