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
