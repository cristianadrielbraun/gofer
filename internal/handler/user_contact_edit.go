package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	case errors.Is(err, storage.ErrContactEditInvalid), errors.Is(err, storage.ErrContactSetupInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, storage.ErrContactEditChanged), errors.Is(err, config.ErrAccountServicesChanged):
		http.Error(w, "Contact or sync locations changed; reload the contact and retry.", http.StatusConflict)
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
	r.Body = http.MaxBytesReader(w, r.Body, contactImportMaxBytes+(128<<10))
	if err := r.ParseMultipartForm(contactImportMaxBytes); err != nil {
		http.Error(w, "Invalid vCard import", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("vcard")
	if err != nil {
		http.Error(w, "Missing vCard file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, contactImportMaxBytes+1))
	if err != nil || int64(len(data)) > contactImportMaxBytes {
		http.Error(w, "vCard file is too large", http.StatusBadRequest)
		return
	}
	// The editor snapshot applies the owned destination allowlist. vCard imports
	// retain the existing disabled-sync behavior and never perform provider HTTP.
	contacts, err := parseVCardContacts(bytes.NewReader(data), strings.Split(r.FormValue("save_targets"), ","))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner, imported := h.userID(r.Context()), 0
	err = h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		for _, contact := range contacts {
			snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, contact)
			if err != nil {
				return err
			}
			if _, err := h.userAccounts.SaveContactEdit(ctx, snapshot, false); err != nil {
				return err
			}
			imported++
		}
		return nil
	})
	if err != nil && imported < len(contacts) {
		if imported == 0 {
			userContactEditError(w, r, err)
		} else {
			noun := "contacts"
			if imported == 1 {
				noun = "contact"
			}
			http.Error(w, fmt.Sprintf("Import stopped after saving %d %s. The remaining contacts were not saved.", imported, noun), http.StatusServiceUnavailable)
		}
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/contacts?imported=%d", imported), http.StatusSeeOther)
}

func (h *Handler) handleUserSaveContact(w http.ResponseWriter, r *http.Request) {
	// Include an encoded maximum-size avatar plus ordinary editor fields.
	r.Body = http.MaxBytesReader(w, r.Body, 4*contactAvatarMaxBytes+(128<<10))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	contact, err := contactFromForm(r, strings.Split(r.FormValue("save_targets"), ","))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
