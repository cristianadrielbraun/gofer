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
	// The editor snapshot applies the owned destination allowlist. vCard imports
	// retain the existing disabled-sync behavior and never perform provider HTTP.
	request, ok := readContactImport(w, r, func(raw string) []string { return strings.Split(raw, ",") })
	if !ok {
		return
	}
	owner := h.userID(r.Context())
	if request.preview {
		previewContactImport(w, r, request.contacts, func(email string) (inUse bool, err error) {
			err = h.userAccounts.WithUser(r.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
				inUse, err = db.ContactEmailInUse(r.Context(), owner, email)
				return err
			})
			return inUse, err
		})
		return
	}
	run := func(work func(context.Context) error) error {
		return h.userIMAP.RunUserServiceWork(r.Context(), owner, work)
	}
	save := func(ctx context.Context, contact models.Contact, skipExisting bool) (bool, bool, error) {
		snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, contact)
		if err != nil {
			return false, false, err
		}
		existed := snapshot.Previous() != nil
		if existed && skipExisting {
			return true, false, nil
		}
		if _, err := h.userAccounts.SaveContactEdit(ctx, snapshot, false); err != nil {
			return existed, false, err
		}
		return existed, true, nil
	}
	runContactImport(w, r, request, run, save, userContactEditError)
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
	var result storage.ContactEditResult
	var previous *models.Contact
	err = h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, contact)
		if err != nil {
			return err
		}
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
		if result.OperationID != "" {
			if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed editor sync: %v", err)
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
