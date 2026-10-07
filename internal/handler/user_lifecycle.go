package handler

import (
	"context"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// Authentication owns the durable status transition. Refresh its effects even
// on an unchanged-status retry, so a failed postcommit wake can be repaired.
// Central identity and scheduling are used without opening any owner stores.
func (h *Handler) refreshOwnedUserStatus(ctx context.Context, owner string, status auth.UserStatus) error {
	if h.ownedMailbox == nil {
		return nil
	}
	user, err := h.auth.GetUserByID(ctx, owner)
	if err != nil {
		return err
	}
	if user == nil || user.UserType != auth.UserTypeWebmail || user.IsAdmin {
		return nil
	}
	owned := h.ownedMailbox
	if status == auth.UserStatusDisabled {
		if owned.userIMAP != nil {
			owned.userIMAP.StopUser(owner)
		}
		return nil
	}
	if status != auth.UserStatusActive {
		return nil
	}
	var receive error
	if owned.userIMAP != nil {
		receive = owned.userIMAP.RefreshUserSettings(ctx, owner)
	}
	return errors.Join(receive, owned.WakeUserContactQueue(ctx, owner), owned.WakeUserContactAccount(ctx, owner, ""), owned.WakeUserCalendarAccount(ctx, owner, ""))
}
