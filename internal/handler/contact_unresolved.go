package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// A contact with Gofer Sync on that one of its places deleted waits to be
// resolved, apart from the list. The resolver dialog lists them all, to
// delete each everywhere, which is moving it to the Trash, or keep it
// without that place, which is handled here.

// handleUnresolvedContactCount gives the count the sidebar's Por resolver
// shows.
func (h *Handler) handleUnresolvedContactCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	var count int
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		count, err = db.CountUnresolvedContacts(ctx, owner)
		return err
	}); err != nil {
		http.Error(w, i18n.T(ctx, "server.error.contacts_unavailable"), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	views.ContactsUnresolvedCount(count).Render(ctx, w)
}

// contactResolverLimit is how many contacts the resolver lists at once.
const contactResolverLimit = 500

// handleContactResolver gives the dialog that lists the contacts to resolve.
func (h *Handler) handleContactResolver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	var contacts []models.Contact
	var accounts []models.Account
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if contacts, err = db.ListUnresolvedContacts(ctx, owner, contactResolverLimit); err != nil {
			return err
		}
		accounts, err = db.GetAccounts(ctx, owner)
		return err
	}); err != nil {
		http.Error(w, i18n.T(ctx, "server.error.contacts_unavailable"), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	views.ContactResolverDialog(contacts, accounts).Render(ctx, w)
}

// handleKeepContactsWithoutDeletedPlaces keeps the posted contacts without
// the places that deleted them, and lets their sync go on with the rest. It
// gives how many were kept; ones already resolved are left out.
func (h *Handler) handleKeepContactsWithoutDeletedPlaces(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactResolverLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	var kept []string
	err := h.runContactWork(ctx, owner, func(ctx context.Context) error {
		return h.withUserDB(ctx, owner, func(db *storage.DB) error {
			for _, id := range ids {
				done, err := db.KeepContactWithoutDeletedPlaces(ctx, owner, id)
				if err != nil {
					return err
				}
				if done {
					kept = append(kept, id)
				}
			}
			return nil
		})
	})
	if err != nil {
		log.Printf("contacts: keep contacts without their deleted places: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.save_failed"), http.StatusInternalServerError)
		return
	}
	// Their sync waited while they were to resolve: it goes on now.
	for _, id := range kept {
		h.resumeContactSync(context.WithoutCancel(ctx), owner, id)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"kept": len(kept)})
}

// contactWaitsToResolve says whether a contact waits to be resolved: it is
// not changed until then.
func (h *Handler) contactWaitsToResolve(ctx context.Context, owner, id string) bool {
	if id == "" {
		return false
	}
	unresolved := false
	_ = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		unresolved, err = db.ContactUnresolved(ctx, owner, id)
		return err
	})
	return unresolved
}

// resumeContactSync writes a contact out to its places after its sync
// waited.
func (h *Handler) resumeContactSync(ctx context.Context, owner, id string) {
	contact, err := h.loadPlacedContact(ctx, owner, id)
	if err != nil || contact == nil || !contact.GoferSyncEnabled {
		return
	}
	queued := false
	if h.userStorage == nil {
		queued = h.scheduleContactAccountSync(ctx, owner, *contact, nil)
	} else if op, err := h.userAccounts.EnqueueContactSyncForUser(ctx, owner, id); err == nil && op != "" {
		queued = true
	}
	if queued {
		h.wakeContactOperations(ctx, owner)
	}
}
