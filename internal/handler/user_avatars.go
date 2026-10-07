package handler

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) senderAvatarVisible(ctx context.Context, email string) (visible bool, err error) {
	err = h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		visible, err = db.IsSenderAvatarEmailVisibleToUser(ctx, email, h.userID(ctx))
		return err
	})
	return
}

func (h *Handler) providerAvatarVisible(ctx context.Context, url string) (visible bool, err error) {
	err = h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		visible, err = db.IsProviderAvatarURLVisibleToUser(ctx, url, h.userID(ctx))
		return err
	})
	return
}

func (h *Handler) ensureAvatarCandidates(ctx context.Context) error {
	if h.avatarRouting == nil {
		_, err := h.db.EnsureSenderAvatarCandidates(ctx)
		return err
	}
	cursor := ""
	for {
		rows, err := h.db.Read().QueryContext(ctx, `SELECT id FROM users WHERE id>? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0 ORDER BY id LIMIT 64`, cursor)
		if err != nil {
			return err
		}
		var owners []string
		for rows.Next() {
			var owner string
			if err = rows.Scan(&owner); err != nil {
				break
			}
			owners = append(owners, owner)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return nil
		}
		for _, owner := range owners {
			after := ""
			for {
				var emails []string
				err := h.avatarRouting.WithExistingUser(ctx, owner, func(db *storage.DB) error {
					var err error
					emails, err = db.ListUserSenderAvatarEmails(ctx, owner, after, 256)
					return err
				})
				if errors.Is(err, os.ErrNotExist) {
					var hasAccounts bool
					if err := h.db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id=? AND state<>'deleted')`, owner).Scan(&hasAccounts); err != nil {
						return err
					}
					if !hasAccounts {
						break
					}
				}
				if err != nil {
					return err
				}
				if len(emails) == 0 {
					break
				}
				if err := h.avatarRouting.RecordUserAvatarInterests(ctx, owner, emails); err != nil {
					return err
				}
				after = emails[len(emails)-1]
			}
			cursor = owner
		}
	}
}

func (h *Handler) publishUserAvatarUpdated(ctx context.Context, hash, email string, expires time.Time) {
	after := ""
	for ctx.Err() == nil {
		owners, err := h.avatarRouting.ListAvatarInterestedUsers(ctx, hash, after, 64)
		if err != nil {
			log.Printf("avatar update: interested owners: %v", err)
			return
		}
		if len(owners) == 0 {
			return
		}
		var visible []string
		for _, owner := range owners {
			allowed := false
			err := h.avatarRouting.WithExistingUser(ctx, owner, func(db *storage.DB) error {
				var err error
				allowed, err = db.IsSenderAvatarEmailVisibleToUser(ctx, email, owner)
				return err
			})
			if err == nil && allowed {
				visible = append(visible, owner)
			}
			after = owner
		}
		if len(visible) > 0 {
			h.syncer.Events().Publish(mail.Event{Type: mail.EventAvatarUpdated, UserIDs: visible, AvatarHash: hash, AvatarURL: storage.SenderAvatarURL(hash, expires)})
		}
	}
}
