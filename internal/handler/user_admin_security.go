package handler

import (
	"context"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) mailSecurityExceptions(ctx context.Context) ([]models.MailSecurityException, error) {
	if h.ownedMailbox == nil {
		return h.db.ListMailSecurityExceptions(ctx)
	}
	ctx, cancel := h.ownedAdminDiagnosticContext(ctx)
	defer cancel()
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return nil, storage.ErrUserDiagnosticsAccess
	}
	actor := storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion}
	if err := h.ownedMailbox.userStorage.ValidateDiagnosticsAdministrator(ctx, actor); err != nil {
		return nil, err
	}
	items, err := h.db.ListMailSecurityPolicies(ctx)
	if err != nil {
		return nil, err
	}
	plaintext := false
	for _, item := range items {
		if item.Kind == models.MailSecurityExceptionPlaintextTransport {
			plaintext = true
		}
	}
	if plaintext {
		_, err = h.eachUserDiagnostic(ctx, "", storage.UserDiagnosticsTransports, func(_ context.Context, _ models.AdminWebmailUserOption, data storage.UserDiagnostics, _ storage.DiagnosticsActor) error {
			for _, account := range data.Transports {
				for i := range items {
					item := &items[i]
					if item.Kind != models.MailSecurityExceptionPlaintextTransport {
						continue
					}
					host, port, mode := account.IMAPHost, account.IMAPPort, account.IMAPTLSMode
					if item.Protocol == "smtp" {
						host, port, mode = account.SMTPHost, account.SMTPPort, account.SMTPTLSMode
					}
					if host == item.Host && port == item.Port && mode == "plaintext" {
						item.Accounts = append(item.Accounts, models.MailSecurityExceptionAccount{ID: account.ID, Email: account.Email})
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for i := range items {
		sort.SliceStable(items[i].Accounts, func(a, b int) bool {
			left, right := items[i].Accounts[a], items[i].Accounts[b]
			if strings.ToLower(left.Email) != strings.ToLower(right.Email) {
				return strings.ToLower(left.Email) < strings.ToLower(right.Email)
			}
			return left.ID < right.ID
		})
	}
	if err := h.ownedMailbox.userStorage.ValidateDiagnosticsAdministrator(ctx, actor); err != nil {
		return nil, err
	}
	return items, nil
}

func (h *Handler) stopMailSecurityAccount(id string) {
	if h.ownedMailbox != nil {
		if h.ownedMailbox.userIMAP != nil {
			h.ownedMailbox.userIMAP.StopAccount(id)
		}
		return
	}
	h.closeBodyClient(id)
	if h.syncer != nil {
		h.syncer.StopAccount(id)
	}
}

func (h *Handler) validateMailSecurityAdministrator(ctx context.Context) error {
	if h.ownedMailbox == nil {
		return nil
	}
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return storage.ErrUserDiagnosticsAccess
	}
	return h.ownedMailbox.userStorage.ValidateDiagnosticsAdministrator(ctx, storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion})
}
