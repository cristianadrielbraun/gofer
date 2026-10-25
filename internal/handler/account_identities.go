package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// loadAccountIdentitiesData gathers what the identities dialog of an account
// shows, or writes the error response and returns false.
func (h *Handler) loadAccountIdentitiesData(w http.ResponseWriter, r *http.Request) (models.AccountIdentitiesData, bool) {
	ctx := r.Context()
	accountID := r.PathValue("id")
	account, err := h.ownedAccount(ctx, accountID)
	if err != nil {
		log.Printf("identities account=%s: %v", accountID, err)
		http.Error(w, "failed to load account", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	if account == nil {
		http.NotFound(w, r)
		return models.AccountIdentitiesData{}, false
	}
	userID := h.userID(ctx)
	identities, err := h.db.ListAccountIdentities(ctx, userID, accountID)
	if err != nil {
		http.Error(w, "failed to load identities", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	signatures, err := h.db.ListSignatures(ctx, userID)
	if err != nil {
		http.Error(w, "failed to load signatures", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	return models.AccountIdentitiesData{Account: *account, Identities: identities, Signatures: signatures}, true
}

// handleManageAccountIdentities renders the identities dialog of an account.
func (h *Handler) handleManageAccountIdentities(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.AccountIdentitiesDialog(data).Render(r.Context(), w)
}

// handleAccountIdentitiesPanel renders the inside of the identities dialog,
// with the identity named by edit in the form, or an empty form.
func (h *Handler) handleAccountIdentitiesPanel(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if id := strings.TrimSpace(r.URL.Query().Get("edit")); id != "" {
		index := slices.IndexFunc(data.Identities, func(identity models.Identity) bool { return identity.ID == id })
		if index < 0 {
			http.NotFound(w, r)
			return
		}
		data.Editing = data.Identities[index]
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

func (h *Handler) renderAccountIdentitiesPanel(w http.ResponseWriter, r *http.Request, data models.AccountIdentitiesData) {
	w.Header().Set("Content-Type", "text/html")
	views.AccountIdentitiesPanel(data).Render(r.Context(), w)
}

// handleSaveAccountIdentity adds an identity, or changes the one named by id.
// A refused change shows its reason with the form as it was sent.
func (h *Handler) handleSaveAccountIdentity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	identity := models.Identity{
		ID:          strings.TrimSpace(r.FormValue("id")),
		AccountID:   r.PathValue("id"),
		Name:        r.FormValue("name"),
		Email:       r.FormValue("email"),
		ReplyTo:     r.FormValue("reply_to"),
		SignatureID: r.FormValue("signature_id"),
	}
	userID := h.userID(r.Context())
	if identity.ID != "" {
		// Provider identities only take a signature; the form shows the rest
		// read-only and does not send it.
		current, err := h.db.GetAccountIdentity(r.Context(), userID, identity.AccountID, identity.ID)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "failed to load identity", http.StatusInternalServerError)
			return
		}
		if current.Source == models.IdentitySourceProvider {
			identity.Name, identity.Email, identity.ReplyTo = current.Name, current.Email, current.ReplyTo
		}
	}
	_, saveErr := h.db.SaveAccountIdentityGuarded(r.Context(), userID, identity, h.signatureGuard)
	if errors.Is(saveErr, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if saveErr != nil {
		if errors.Is(saveErr, storage.ErrProviderIdentity) {
			data.Error = "Only the signature of an identity from the provider can be changed here."
		} else {
			data.Error = saveErr.Error()
		}
		data.Editing = identity
		if identity.ID != "" {
			if index := slices.IndexFunc(data.Identities, func(i models.Identity) bool { return i.ID == identity.ID }); index >= 0 {
				data.Editing.Source = data.Identities[index].Source
			}
		}
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

// handleDeleteAccountIdentity removes a manual identity.
func (h *Handler) handleDeleteAccountIdentity(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteAccountIdentityGuarded(r.Context(), h.userID(r.Context()), r.PathValue("id"), r.PathValue("identity"), h.signatureGuard)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if errors.Is(err, storage.ErrProviderIdentity) {
		data.Error = "An identity from the provider can only be removed at the provider."
	} else if err != nil {
		log.Printf("delete identity account=%s: %v", r.PathValue("id"), err)
		data.Error = "The identity could not be deleted."
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

// handleMoveAccountIdentity moves an identity one place up or down.
func (h *Handler) handleMoveAccountIdentity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	ids := make([]string, len(data.Identities))
	for i, identity := range data.Identities {
		ids[i] = identity.ID
	}
	from := slices.Index(ids, r.PathValue("identity"))
	if from < 0 {
		http.NotFound(w, r)
		return
	}
	to := from - 1
	if r.FormValue("direction") == "down" {
		to = from + 1
	}
	if to >= 0 && to < len(ids) {
		ids[from], ids[to] = ids[to], ids[from]
		if err := h.db.SetAccountIdentityOrderGuarded(r.Context(), h.userID(r.Context()), data.Account.ID, ids, h.signatureGuard); err != nil {
			log.Printf("order identities account=%s: %v", data.Account.ID, err)
			data.Error = "The identities could not be reordered."
		} else if data, ok = h.loadAccountIdentitiesData(w, r); !ok {
			return
		}
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}
