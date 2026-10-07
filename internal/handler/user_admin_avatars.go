package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// adminAvatarRead keeps the shared-cache read within the same bounded request
// as its copied local visibility snapshot. No user-store lease survives setup.
type adminAvatarRead struct {
	ctx      context.Context
	close    context.CancelFunc
	emails   []string
	validate func() error
}

func (h *Handler) beginAdminAvatarRead(parent context.Context, scope models.AdminWebmailScope) (*adminAvatarRead, error) {
	read := &adminAvatarRead{ctx: parent, close: func() {}, validate: func() error { return parent.Err() }}
	if h.ownedMailbox == nil {
		return read, nil
	}
	read.ctx, read.close = h.ownedAdminDiagnosticContext(parent)
	user := auth.GetCurrentUser(read.ctx)
	if user == nil {
		read.close()
		return nil, storage.ErrUserDiagnosticsAccess
	}
	actor := storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion}
	read.validate = func() error {
		if scope.SelectedUserID != "" {
			return h.ownedMailbox.userStorage.ValidateDiagnosticsAccess(read.ctx, actor, scope.SelectedUserID)
		}
		return h.ownedMailbox.userStorage.ValidateDiagnosticsAdministrator(read.ctx, actor)
	}
	if err := read.validate(); err != nil {
		read.close()
		return nil, err
	}
	if scope.SelectedUserID != "" {
		data, err := h.ownedMailbox.userStorage.ReadUserDiagnostics(read.ctx, actor, scope.SelectedUserID, storage.UserDiagnosticsAvatars)
		if err != nil {
			read.close()
			return nil, err
		}
		read.emails = data.AvatarEmails
		if read.emails == nil {
			read.emails = []string{}
		}
	}
	return read, nil
}

func (h *Handler) adminProviderContactAvatars(ctx context.Context, scope models.AdminWebmailScope, emails []string) (map[string]string, error) {
	if h.ownedMailbox == nil {
		if scope.SelectedUserID == "" {
			return h.db.GetInstanceProviderContactAvatarsByEmail(ctx, emails)
		}
		return h.db.GetProviderContactAvatarsByEmail(ctx, scope.SelectedUserID, emails)
	}
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return nil, storage.ErrUserDiagnosticsAccess
	}
	actor := storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion}
	routing := h.ownedMailbox.userStorage
	if err := routing.ValidateDiagnosticsAdministrator(ctx, actor); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(emails) == 0 {
		return out, nil
	}
	direct := map[string]storage.AdminProviderAvatar{}
	links := []storage.AdminProviderAvatarLink{}
	// Direct profiles always win, even over a newer fallback donor. Preserve the
	// legacy two-stage query rather than choosing the newest URL of either kind.
	for _, owner := range scope.Users {
		if scope.SelectedUserID != "" && scope.SelectedUserID != owner.ID {
			continue
		}
		data, err := routing.ReadAdminProviderAvatarContacts(ctx, actor, owner.ID, emails)
		if err != nil {
			return nil, err
		}
		for _, candidate := range data.Direct {
			current, ok := direct[candidate.Email]
			if !ok || candidate.UpdatedAt.After(current.UpdatedAt) {
				direct[candidate.Email] = candidate
			}
		}
		links = append(links, data.Links...)
	}
	for email, candidate := range direct {
		out[email] = candidate.URL
	}
	keys := []storage.ProviderAvatarIdentity{}
	keyEmails := map[storage.ProviderAvatarIdentity]map[string]bool{}
	for _, link := range links {
		if out[link.Email] != "" {
			continue
		}
		if keyEmails[link.Identity] == nil {
			keyEmails[link.Identity] = map[string]bool{}
			keys = append(keys, link.Identity)
		}
		keyEmails[link.Identity][link.Email] = true
	}
	if len(keys) > 0 {
		fallback := map[string]storage.AdminProviderAvatarDonor{}
		// A selected profile may borrow its provider photo from another retained
		// owner's matching provider account and contact, just as the shared query
		// did. Every donor store receives an independent administrator check.
		for _, owner := range scope.Users {
			donors, err := routing.ReadAdminProviderAvatarDonors(ctx, actor, owner.ID, keys)
			if err != nil {
				return nil, err
			}
			for _, candidate := range donors {
				for email := range keyEmails[candidate.Identity] {
					current, ok := fallback[email]
					if !ok || candidate.UpdatedAt.After(current.UpdatedAt) {
						fallback[email] = candidate
					}
				}
			}
		}
		for email, candidate := range fallback {
			out[email] = candidate.URL
		}
	}
	if err := routing.ValidateDiagnosticsAdministrator(ctx, actor); err != nil {
		return nil, err
	}
	return out, nil
}
