package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A contact lives in one place. Duplicate makes an independent copy of it in
// another place: a new contact with the same details, written there when the
// place is an account. Move changes where it lives: the copy is written to
// the new place first, and only once it is there is the old contact deleted,
// from its account too, so a failed write leaves the contact where it was.
// A contact with Gofer Sync on is already in each of its places, and
// deleting it would delete it from all of them: it isn't moved.

// contactMoveWriteTimeout is how long a move waits for the new copy.
const contactMoveWriteTimeout = time.Minute

func (h *Handler) handleMoveContact(w http.ResponseWriter, r *http.Request) {
	h.placeContactCopy(w, r, true)
}

func (h *Handler) handleDuplicateContact(w http.ResponseWriter, r *http.Request) {
	h.placeContactCopy(w, r, false)
}

func (h *Handler) placeContactCopy(w http.ResponseWriter, r *http.Request, move bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.bad_request"), http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	place := strings.TrimSpace(r.FormValue("place"))
	// The browser may go; a move that started finishes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), contactMoveWriteTimeout+2*time.Minute)
	defer cancel()

	original, err := h.loadPlacedContact(ctx, owner, id)
	if err != nil {
		log.Printf("contacts: place copy %s: %v", id, err)
		http.Error(w, i18n.T(r.Context(), "server.error.contacts_unavailable"), http.StatusServiceUnavailable)
		return
	}
	if original == nil {
		http.NotFound(w, r)
		return
	}
	home := strings.TrimSpace(original.StoredIn)
	if home == "" {
		home = "local"
	}
	if accepted, err := h.contactPlaceAccepted(ctx, owner, place); err != nil || !accepted || place == home {
		http.Error(w, i18n.T(r.Context(), "server.error.choose_another_place_for_the_contact"), http.StatusBadRequest)
		return
	}
	if h.contactWaitsToResolve(ctx, owner, id) {
		http.Error(w, i18n.T(r.Context(), "server.error.resolve_contact_first"), http.StatusConflict)
		return
	}
	if move && original.GoferSyncEnabled {
		http.Error(w, i18n.T(r.Context(), "server.error.turn_off_gofer_sync_to_move_contact"), http.StatusConflict)
		return
	}

	copy := *original
	copy.ID, copy.StoredIn, copy.Source = "", place, ""
	copy.SaveTargets, copy.GoferSyncEnabled, copy.RemoveAvatar = nil, false, false
	created, operation, err := h.createPlacedContact(ctx, owner, copy)
	if err != nil {
		if h.userStorage != nil {
			userContactEditError(w, r, err)
			return
		}
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return
	}
	writing := operation != ""
	if move && operation != "" {
		progress := waitContactOperations(ctx, []string{operation}, time.Now().Add(contactMoveWriteTimeout), h.deliverContactOperation(owner), h.contactOperationProgress(owner), nil)
		if progress.Done != 1 {
			// The contact stays where it was; the copy that couldn't be
			// written goes.
			if err := h.removePlacedContact(ctx, owner, created); err != nil {
				log.Printf("contacts: discard unwritten move copy %s: %v", created, err)
			}
			reason := serverText(r.Context(), progress.Reason)
			if reason == "" {
				reason = i18n.T(r.Context(), "js.common.please_try_again")
			}
			http.Error(w, i18n.T(r.Context(), "server.error.contact_move_write_failed", "reason", reason), http.StatusBadGateway)
			return
		}
		writing = false
	}
	if move {
		if err := h.takeOverPlacedContact(ctx, owner, id, created); err != nil {
			log.Printf("contacts: move %s to %s: take over: %v", id, created, err)
		}
		if err := h.removePlacedContact(ctx, owner, id); err != nil {
			log.Printf("contacts: move %s: remove the old contact: %v", id, err)
			http.Error(w, i18n.T(r.Context(), "server.error.contact_move_remove_failed", "reason", serverText(r.Context(), err.Error())), http.StatusBadGateway)
			return
		}
	} else if operation != "" {
		h.wakeContactOperations(ctx, owner)
	}
	location := "/contacts?contact=" + url.QueryEscape(created)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "contact_id": created, "location": location, "writing": writing})
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) loadPlacedContact(ctx context.Context, owner, id string) (contact *models.Contact, err error) {
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		contact, err = db.GetContact(ctx, owner, id)
		return err
	})
	if contact != nil && contact.TrashedAt != "" {
		return nil, err
	}
	return contact, err
}

func (h *Handler) contactPlaceAccepted(ctx context.Context, owner, place string) (bool, error) {
	if h.userStorage == nil {
		targets := h.contactSaveTargets(ctx, place)
		return len(targets) == 1 && targets[0] == place, nil
	}
	accepted := false
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		accepted, err = db.ContactPlaceAccepted(ctx, owner, place)
		return err
	})
	return accepted, err
}

// createPlacedContact saves contact as a new one in contact.StoredIn and
// queues its write there when that is an account. It gives the new contact
// and the queued write.
func (h *Handler) createPlacedContact(ctx context.Context, owner string, contact models.Contact) (id, operation string, err error) {
	if h.userStorage == nil {
		saved, err := h.db.SaveContact(ctx, owner, contact)
		if err != nil {
			return "", "", err
		}
		if contact.StoredIn != "local" {
			operation, err = h.db.EnqueueContactStoreOperation(ctx, owner, saved, contact.StoredIn)
		}
		return saved.ID, operation, err
	}
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, contact)
		if err != nil {
			return err
		}
		saved, err := h.userAccounts.SaveContactEdit(ctx, snapshot, false)
		if err != nil {
			return err
		}
		id = saved.Contact.ID
		if contact.StoredIn != "local" {
			operation, err = h.userAccounts.EnqueueContactStore(ctx, owner, id, contact.StoredIn)
		}
		return err
	})
	return id, operation, err
}

func (h *Handler) deliverContactOperation(owner string) func(context.Context) bool {
	if h.userStorage == nil {
		return func(ctx context.Context) bool {
			ops, err := h.db.ClaimContactSyncOperations(ctx, 1, 5*time.Minute)
			if err != nil || len(ops) == 0 {
				return false
			}
			h.processContactSyncOperation(ctx, ops[0])
			return true
		}
	}
	return func(ctx context.Context) bool {
		processed, _ := h.ProcessUserContactSyncOperations(ctx, owner, 1)
		return processed > 0
	}
}

func (h *Handler) contactOperationProgress(owner string) func(context.Context, []string) (storage.ContactOperationProgress, error) {
	return func(ctx context.Context, ids []string) (progress storage.ContactOperationProgress, err error) {
		err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
			progress, err = db.ContactOperationProgress(ctx, owner, ids)
			return err
		})
		return progress, err
	}
}

func (h *Handler) wakeContactOperations(ctx context.Context, owner string) {
	if h.userStorage == nil {
		h.signalContactSyncOperationWorker()
		return
	}
	if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
		log.Printf("owned contacts: wake contact copy: %v", err)
	}
}

func (h *Handler) takeOverPlacedContact(ctx context.Context, owner, from, to string) error {
	return h.withUserDB(ctx, owner, func(db *storage.DB) error {
		return db.TakeOverContact(ctx, owner, from, to)
	})
}

// removePlacedContact deletes a contact for good, from its account too,
// without keeping Gofer from collecting its address from mail again.
func (h *Handler) removePlacedContact(ctx context.Context, owner, id string) error {
	return h.runContactWork(ctx, owner, func(ctx context.Context) error {
		if h.userStorage == nil {
			contact, err := h.db.GetContactIncludingTrash(ctx, owner, id)
			if err != nil || contact == nil {
				return err
			}
			if err := h.deleteContactFromAccounts(ctx, owner, *contact); err != nil {
				return contactSyncDeleteError{err}
			}
			return h.db.DeleteContact(ctx, owner, id, false)
		}
		_, err := h.deleteUserContact(ctx, owner, id)
		var syncErr contactSyncDeleteError
		if errors.As(err, &syncErr) {
			return syncErr.err
		}
		return err
	})
}
