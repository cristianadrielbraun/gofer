package handler

import (
	"context"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserSuppressedContactsSettings(w http.ResponseWriter, r *http.Request) {
	contacts, count, err := h.userAccounts.ReadContactSuppression(r.Context(), h.userID(r.Context()))
	if err != nil {
		userContactEditError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SettingsSuppressedContacts(contacts, count).Render(r.Context(), w); err != nil {
		log.Printf("owned contacts: render suppression list: %v", err)
	}
}

func (h *Handler) handleUserClearSuppressedContacts(w http.ResponseWriter, r *http.Request) {
	h.clearUserContactSuppression(w, r, nil)
}
func (h *Handler) handleUserClearSuppressedContact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.clearUserContactSuppression(w, r, &id)
}
func (h *Handler) clearUserContactSuppression(w http.ResponseWriter, r *http.Request, id *string) {
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		_, err := h.userAccounts.ClearContactSuppression(ctx, h.userID(r.Context()), id)
		return err
	})
	if err != nil {
		userContactEditError(w, r, err)
		return
	}
	h.handleUserSuppressedContactsSettings(w, r)
}

func (h *Handler) handleUserDeleteObservedContacts(w http.ResponseWriter, r *http.Request) {
	owner := h.userID(r.Context())
	var committed bool
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		_, err := h.userAccounts.DeleteObservedContacts(ctx, owner)
		if err == nil {
			committed = true
			if wakeErr := h.WakeUserContactQueue(ctx, owner); wakeErr != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed observed delete: %v", wakeErr)
			}
		}
		return err
	})
	if err != nil && !committed {
		userContactEditError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/contacts", http.StatusSeeOther)
}
