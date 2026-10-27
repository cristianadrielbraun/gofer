package handler

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// mailAllowlistDB is the central database holding the allowed mail lists:
// the routed handler's own, which request-local handlers don't have.
func (h *Handler) mailAllowlistDB() *storage.DB {
	if h.userStorage != nil {
		return h.userStorage.System()
	}
	return h.db
}

// mailAllowlist returns the allowed mail lists as they apply to the person
// making the request, or in full when nobody is signed in.
func (h *Handler) mailAllowlist(ctx context.Context) (storage.MailAllowlist, error) {
	list, err := h.mailAllowlistDB().MailAllowlistForUser(ctx, mailAllowlistOwner(ctx))
	if err != nil {
		return storage.MailAllowlist{}, fmt.Errorf("check allowed mail lists: %w", err)
	}
	return list, nil
}

// checkMailServer refuses a server the person making the request may not
// connect to.
func (h *Handler) checkMailServer(ctx context.Context, host string) error {
	list, err := h.mailAllowlist(ctx)
	if err != nil {
		return err
	}
	return list.CheckServer(host)
}

type mailAllowlistOwnerKey struct{}

// withMailAllowlistOwner marks work done for a person outside one of their
// requests, so CalDAV and CardDAV connections apply that person's lists.
func withMailAllowlistOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, mailAllowlistOwnerKey{}, owner)
}

// mailAllowlistOwner is the person a connection is made for: the one marked
// on the context, or else the one signed in.
func mailAllowlistOwner(ctx context.Context) string {
	if owner, ok := ctx.Value(mailAllowlistOwnerKey{}).(string); ok && owner != "" {
		return owner
	}
	if user := auth.GetCurrentUser(ctx); user != nil {
		return user.ID
	}
	return ""
}

// davServerAllowed reports whether the allowed mail lists let a CalDAV or
// CardDAV request go to host. A connection made for nobody in particular is
// held to the lists in full.
func davServerAllowed(db *storage.DB) func(context.Context, string) error {
	// Read the lists now, so connections find them kept in memory.
	if _, err := db.GetMailAllowlist(context.Background()); err != nil {
		log.Printf("read allowed mail lists: %v", err)
	}
	return func(ctx context.Context, host string) error {
		list, err := db.MailAllowlistForUser(ctx, mailAllowlistOwner(ctx))
		if err != nil {
			log.Printf("check allowed mail lists for DAV server %s: %v", host, err)
			return errors.New("Gofer could not check the allowed mail servers")
		}
		return list.CheckServer(host)
	}
}
