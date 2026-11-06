package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func userContactEditError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, storage.ErrContactEditInvalid), errors.Is(err, storage.ErrContactEmailInvalid), errors.Is(err, storage.ErrContactSetupInvalid):
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
	case errors.Is(err, storage.ErrContactEditChanged), errors.Is(err, config.ErrAccountServicesChanged):
		http.Error(w, i18n.T(r.Context(), "server.error.contact_or_sync_locations_changed_reload_the"), http.StatusConflict)
	default:
		userAccountError(w, r, err)
	}
}

func (h *Handler) handleUserUnifyContact(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	var result storage.ContactEditResult
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactUnify(ctx, owner, id)
		if err != nil {
			return err
		}
		result, err = h.userAccounts.SaveContactEdit(ctx, snapshot, false)
		if err == nil && result.OperationID != "" {
			if wakeErr := h.WakeUserContactQueue(ctx, owner); wakeErr != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed unification: %v", wakeErr)
			}
		}
		return err
	})
	if err != nil && result.Contact.ID == "" {
		userContactEditError(w, r, err)
		return
	}
	location := "/contacts?contact=" + url.QueryEscape(result.Contact.ID)
	if result.OperationID != "" {
		location += "&sync=queued"
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "action": "unify", "contact_id": result.Contact.ID, "location": location, "contact_sync_queued": result.OperationID != "", "refresh_detail": true})
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) handleUserImportContacts(w http.ResponseWriter, r *http.Request) {
	owner := h.userID(r.Context())
	withDB := func(ctx context.Context, read func(*storage.DB) error) error {
		return h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error { return read(db) })
	}
	store := contactImportStore{
		accepted: func(ctx context.Context, place string) (ok bool, err error) {
			err = withDB(ctx, func(db *storage.DB) error {
				ok, err = db.ContactPlaceAccepted(ctx, owner, place)
				return err
			})
			return ok, err
		},
		storedIn: func(ctx context.Context, email, place string) (ok bool, err error) {
			err = withDB(ctx, func(db *storage.DB) error {
				ok, err = db.ContactStoredIn(ctx, owner, email, place)
				return err
			})
			return ok, err
		},
		run: func(ctx context.Context, work func(context.Context) error) error {
			return h.userIMAP.RunUserServiceWork(ctx, owner, work)
		},
		// The editor snapshot applies the owned destination allowlist and
		// rereads the contact before it is saved.
		save: func(ctx context.Context, contact models.Contact, request contactImportRequest) (contactImportSaved, error) {
			var result contactImportSaved
			// Only a contact already in the store place is the same contact;
			// one with the address elsewhere is another, left alone.
			var inPlace string
			if err := withDB(ctx, func(db *storage.DB) error {
				var err error
				inPlace, err = db.ContactInPlace(ctx, owner, contact.Email, request.storeIn)
				return err
			}); err != nil {
				return result, err
			}
			result.existed = inPlace != ""
			if result.existed && request.skipExisting {
				return result, nil
			}
			contact.ID = inPlace
			first, err := h.userAccounts.SnapshotContactEdit(ctx, owner, importedContact(contact, nil, request))
			if err != nil {
				return result, err
			}
			snapshot := first
			if previous := first.Previous(); previous != nil {
				if snapshot, err = h.userAccounts.SnapshotContactEdit(ctx, owner, importedContact(contact, previous, request)); err != nil {
					return result, err
				}
			}
			saved, err := h.userAccounts.SaveContactEdit(ctx, snapshot, false)
			if err != nil {
				return result, err
			}
			result.saved, result.operation = true, saved.OperationID
			if contactImportNeedsStore(snapshot.Contact(), request) {
				result.operation, err = h.userAccounts.EnqueueContactStore(ctx, owner, saved.Contact.ID, request.storeIn)
			}
			return result, err
		},
		deliver: func(ctx context.Context) bool {
			processed, _ := h.ProcessUserContactSyncOperations(ctx, owner, 1)
			return processed > 0
		},
		progress: func(ctx context.Context, ids []string) (progress storage.ContactOperationProgress, err error) {
			err = withDB(ctx, func(db *storage.DB) error {
				progress, err = db.ContactOperationProgress(ctx, owner, ids)
				return err
			})
			return progress, err
		},
		fail: userContactEditError,
	}
	request, ok := readContactImport(w, r, store)
	if !ok {
		return
	}
	if request.preview {
		previewContactImport(w, r, request, store)
		return
	}
	runContactImport(w, r, request, store)
	// Writes that failed once are tried again by the worker.
	if err := h.WakeUserContactQueue(context.WithoutCancel(r.Context()), owner); err != nil {
		log.Printf("owned contacts: wake import queue: %v", err)
	}
}

func (h *Handler) handleUserSaveContact(w http.ResponseWriter, r *http.Request) {
	// Include an encoded maximum-size avatar plus ordinary editor fields.
	r.Body = http.MaxBytesReader(w, r.Body, 4*contactAvatarMaxBytes+(128<<10))
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.bad_request"), http.StatusBadRequest)
		return
	}
	contact, err := contactFromForm(r, strings.Split(r.FormValue("save_targets"), ","))
	if err != nil {
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return
	}
	owner := h.userID(r.Context())
	if h.contactWaitsToResolve(r.Context(), owner, contact.ID) {
		http.Error(w, i18n.T(r.Context(), "server.error.resolve_contact_first"), http.StatusConflict)
		return
	}
	// A new contact lives where it was asked to, when that is a place it can
	// be saved to; an existing one keeps its home.
	if contact.ID == "" {
		accepted := false
		if err := h.userAccounts.WithUser(r.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			accepted, err = db.ContactPlaceAccepted(r.Context(), owner, contact.StoredIn)
			return err
		}); err != nil || !accepted {
			contact.StoredIn = "local"
		}
	} else {
		contact.StoredIn = ""
	}
	var result storage.ContactEditResult
	var previous *models.Contact
	created := false
	removeFailed := ""
	err = h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		syncPlaces, err := h.contactSyncPlaces(ctx, owner, contact.ID)
		if err != nil {
			return err
		}
		snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, contact)
		if err != nil {
			return err
		}
		created = snapshot.Previous() == nil
		if contact.ID != "" {
			previous = snapshot.Previous()
		}
		requested := snapshot.Contact()
		// New sync locations follow the existing setup dialog. Without a new
		// location, the legacy preflight has no provider query to perform.
		setupRequired := contactSyncSetupRequired(requested, previous)
		result, err = h.userAccounts.SaveContactEdit(ctx, snapshot, setupRequired)
		if err != nil {
			return err
		}
		// The commit already succeeded. UI hydration may fail during shutdown;
		// retain the committed projection rather than hiding its saved identity.
		if ctx.Err() == nil {
			if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
				saved, err := db.GetContact(ctx, owner, result.Contact.ID)
				if err == nil && saved != nil {
					result.Contact = *saved
				}
				return err
			}); err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: hydrate saved contact: %v", err)
			}
		}
		// A contact living on an account is written there: a new one now, and
		// an edit to one whatever Gofer Sync says. With Gofer Sync on, its
		// sync writes it there with its other places.
		home := contact.StoredIn
		if !created {
			home = result.Contact.StoredIn
		}
		if home != "" && home != "local" && !result.Contact.GoferSyncEnabled {
			id, err := h.userAccounts.EnqueueContactStore(ctx, owner, result.Contact.ID, home)
			if err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: store new contact: %v", err)
			}
			if result.OperationID == "" {
				result.OperationID = id
			}
		}
		if result.OperationID != "" {
			if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed editor sync: %v", err)
			}
		}
		// A place taken out of Gofer Sync no longer holds the contact.
		if !created {
			if err := h.removeDroppedSyncPlaces(ctx, owner, result.Contact.ID, result.Contact.StoredIn, syncPlaces); err != nil {
				log.Printf("owned contacts: remove %s from dropped sync places: %v", result.Contact.ID, err)
				removeFailed = contactDeleteFailureMessage(result.Contact.ID, err)
			}
		}
		return nil
	})
	if err != nil && result.Contact.ID == "" {
		userContactEditError(w, r, err)
		return
	}
	saved := result.Contact
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"ok": true, "contact_id": saved.ID, "location": "/contacts?contact=" + saved.ID,
			"contact_sync_queued": result.OperationID != "", "refresh_detail": previous != nil,
			"avatar_hash": saved.AvatarHash, "avatar_url": saved.AvatarURL}
		if removeFailed != "" {
			response["contact_place_remove_failed"] = serverText(r.Context(), removeFailed)
		}
		if result.SetupDeferred {
			response["contact_sync_setup_url"] = "/api/contacts/" + url.PathEscape(saved.ID) + "/sync-setup"
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	location := "/contacts?contact=" + saved.ID
	if result.SetupDeferred {
		location += "&sync_setup=1"
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}
