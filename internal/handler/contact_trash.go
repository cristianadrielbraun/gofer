package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Deleting a contact deletes its copies on accounts and moves it to the
// Trash (see trashContactsEverywhere); restoring writes it back. It is
// deleted for good when the user says so or once it has been in the Trash
// for storage.ContactTrashRetention.

var errContactNotInTrash = errors.New("contact isn't in the Trash")

// contactTrashPurgeBatch caps how many contacts one purge pass deletes for
// good for a user.
const contactTrashPurgeBatch = 20

// runContactWork runs fn as the user's service work when contacts live in
// per-user storage.
func (h *Handler) runContactWork(ctx context.Context, owner string, fn func(context.Context) error) error {
	if h.userStorage == nil {
		return fn(ctx)
	}
	return h.userIMAP.RunUserServiceWork(ctx, owner, fn)
}

func (h *Handler) moveContactsToTrash(ctx context.Context, owner string, ids []string) (count int, err error) {
	if h.userStorage == nil {
		return h.db.TrashContacts(ctx, owner, ids)
	}
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		count, err = h.userAccounts.TrashContacts(ctx, owner, ids)
		return err
	})
	return count, err
}

// restoreContacts brings contacts back from the Trash, to live in place
// when it is set, else where they lived.
func (h *Handler) restoreContacts(ctx context.Context, owner string, ids []string, place string) (count int, err error) {
	if h.userStorage == nil {
		return h.db.RestoreContactsTo(ctx, owner, ids, place)
	}
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		count, err = h.userAccounts.RestoreContactsTo(ctx, owner, ids, place)
		return err
	})
	return count, err
}

// deleteContactForever deletes a contact in the Trash for good, from its
// address books first. It runs as the user's contact work. It gives
// sql.ErrNoRows for a contact already gone and errContactNotInTrash for one
// that is back out of the Trash.
func (h *Handler) deleteContactForever(ctx context.Context, owner, id string) error {
	var profile *models.ContactProfile
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		profile, err = db.GetContactProfile(ctx, owner, id)
		return err
	})
	switch {
	case err != nil:
		return err
	case profile == nil || (profile.IsDeleted && profile.TrashedAt == ""):
		return sql.ErrNoRows
	case profile.TrashedAt == "":
		return errContactNotInTrash
	}
	if h.userStorage == nil {
		return h.deleteContact(ctx, owner, id)
	}
	_, err = h.deleteUserContact(ctx, owner, id)
	return err
}

func (h *Handler) contactTrashError(w http.ResponseWriter, r *http.Request, err error) {
	if h.userStorage != nil {
		userContactEditError(w, r, err)
		return
	}
	log.Printf("contacts: trash: %v", err)
	http.Error(w, i18n.T(r.Context(), "server.error.contacts_unavailable"), http.StatusInternalServerError)
}

// handleDeleteContact moves a contact to the Trash.
func (h *Handler) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	count, failed, err := h.trashContactsEverywhere(r.Context(), h.userID(r.Context()), []string{strings.TrimSpace(r.PathValue("id"))})
	switch {
	case err != nil:
		h.contactTrashError(w, r, err)
	case len(failed) > 0:
		http.Error(w, serverText(r.Context(), failed[0].Error), http.StatusBadGateway)
	case count == 0:
		http.NotFound(w, r)
	default:
		http.Redirect(w, r, "/contacts", http.StatusSeeOther)
	}
}

// handleDeleteContactForever deletes one contact in the Trash for good.
func (h *Handler) handleDeleteContactForever(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	err := h.runContactWork(r.Context(), owner, func(ctx context.Context) error {
		return h.deleteContactForever(ctx, owner, id)
	})
	var syncErr contactSyncDeleteError
	var provider userContactPreviewProviderError
	switch {
	case err == nil:
		http.Redirect(w, r, "/contacts?trash=1", http.StatusSeeOther)
	case errors.Is(err, errContactNotInTrash):
		http.Error(w, i18n.T(r.Context(), "server.error.this_contact_was_restored_from_the_trash"), http.StatusConflict)
	case errors.As(err, &syncErr):
		http.Error(w, serverText(r.Context(), "Contact sync delete failed: "+syncErr.err.Error()), http.StatusBadGateway)
	case h.userStorage == nil && errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case h.userStorage == nil:
		log.Printf("contacts: delete forever: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.delete_failed"), http.StatusInternalServerError)
	case errors.As(err, &provider) && !errors.Is(err, storage.ErrContactEditChanged) && !errors.Is(err, config.ErrAccountServicesChanged) && !errors.Is(err, storage.ErrAccountRoute) && !errors.Is(err, storage.ErrUserStoreOwner) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
		http.Error(w, serverText(r.Context(), "Contact sync delete failed: "+err.Error()), http.StatusBadGateway)
	default:
		userContactEditError(w, r, err)
	}
}

// handleContactTrashCount gives the count the sidebar's Trash shows.
func (h *Handler) handleContactTrashCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	var count int
	if err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		var err error
		count, err = db.CountContacts(ctx, userID, models.ContactFilters{Trash: true})
		return err
	}); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.contacts_unavailable"), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	views.ContactsTrashCount(count).Render(ctx, w)
}

// handleTrashContacts moves the posted contacts to the Trash. Ids that
// aren't the user's contacts are left out of the count.
func (h *Handler) handleTrashContacts(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactSelectionLimit)
	if !ok {
		return
	}
	count, failed, err := h.trashContactsEverywhere(r.Context(), h.userID(r.Context()), ids)
	if err != nil && count == 0 && len(failed) == 0 {
		h.contactTrashError(w, r, err)
		return
	}
	if failed == nil {
		failed = []contactDeleteFailure{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"trashed": count, "failed": failed})
}

// handleRestoreContacts brings the posted contacts back from the Trash.
func (h *Handler) handleRestoreContacts(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactSelectionLimit)
	if !ok {
		return
	}
	owner := h.userID(r.Context())
	// A place makes the restored contacts live there instead.
	place := strings.TrimSpace(r.FormValue("place"))
	if place != "" {
		if accepted, err := h.contactPlaceAccepted(r.Context(), owner, place); err != nil || !accepted {
			http.Error(w, i18n.T(r.Context(), "server.error.choose_another_place_for_the_contact"), http.StatusBadRequest)
			return
		}
	}
	count, err := h.restoreContacts(r.Context(), owner, ids, place)
	if err != nil {
		h.contactTrashError(w, r, err)
		return
	}
	// Restored contacts are written where they live now.
	h.writeBackRestoredContacts(context.WithoutCancel(r.Context()), owner, ids)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"restored": count})
}

// handleDeleteContactsForever deletes the posted contacts in the Trash for
// good, going on past any that fail. Emptying the Trash posts every contact
// in it, a batch at a time.
func (h *Handler) handleDeleteContactsForever(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactBulkDeleteLimit)
	if !ok {
		return
	}
	owner := h.userID(r.Context())
	var result contactBulkDeleteResult
	err := h.runContactWork(r.Context(), owner, func(ctx context.Context) error {
		for _, id := range ids {
			result.record(id, h.deleteContactForever(ctx, owner, id))
		}
		return nil
	})
	if err != nil && result.Deleted == 0 && len(result.Failed) == 0 {
		h.contactTrashError(w, r, err)
		return
	}
	writeContactBulkDeleteResult(w, r, result)
}

// purgeContactTrash deletes for good the user's contacts that have been in
// the Trash for the whole retention period, oldest first, up to limit. One
// whose address book can't be reached stays for the next pass. It gives how
// many went.
func (h *Handler) purgeContactTrash(ctx context.Context, owner string, limit int) (int, error) {
	var ids []string
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		ids, err = db.TrashedContactIDs(ctx, owner, time.Now().Add(-storage.ContactTrashRetention), limit)
		return err
	})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	purged := 0
	err = h.runContactWork(ctx, owner, func(ctx context.Context) error {
		var failures []error
		for _, id := range ids {
			if err := h.deleteContactForever(ctx, owner, id); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errContactNotInTrash) {
				failures = append(failures, err)
				continue
			}
			purged++
		}
		return errors.Join(failures...)
	})
	return purged, err
}

// purgeAllContactTrash runs purgeContactTrash for every user with contacts
// due, until none are left or a pass for a user makes no progress.
func (h *Handler) purgeAllContactTrash(ctx context.Context) {
	owners, err := h.db.ContactTrashOwners(ctx, time.Now().Add(-storage.ContactTrashRetention))
	if err != nil {
		log.Printf("contacts trash: list users: %v", err)
		return
	}
	for _, owner := range owners {
		for ctx.Err() == nil {
			purged, err := h.purgeContactTrash(ctx, owner, contactTrashPurgeBatch)
			if err != nil && ctx.Err() == nil {
				log.Printf("contacts trash %s: %v", owner, err)
			}
			if purged < contactTrashPurgeBatch {
				break
			}
		}
	}
}
