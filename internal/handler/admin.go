package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/avatars/", http.StatusFound)
}

func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	activeTab, ok := adminAvatarTab(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	avatarStatus, err := h.avatarStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(avatarStatus, models.ContactAdminStatus{}, "avatars", activeTab).Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, avatarStatus, models.ContactAdminStatus{}, "avatars", activeTab).Render(ctx, w)
}

func (h *Handler) handleAdminContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	contactStatus, err := h.contactAdminStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get contact admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(models.AvatarStatus{}, contactStatus, "contacts", "").Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, models.AvatarStatus{}, contactStatus, "contacts", "").Render(ctx, w)
}

func (h *Handler) handleContactAdminStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.contactAdminStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get contact admin status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) contactAdminStatus(ctx context.Context) (models.ContactAdminStatus, error) {
	status, err := h.db.GetContactAdminStatus(ctx, h.userID(ctx))
	if err != nil {
		return status, err
	}
	status.Backfill = h.getContactBackfillState()
	running := h.contactSyncRunningAccounts()
	for i := range status.AccountSync {
		status.AccountSync[i].Running = running[status.AccountSync[i].AccountID]
	}
	return status, nil
}

func (h *Handler) handleForceContactBackfill(w http.ResponseWriter, r *http.Request) {
	started := h.startContactBackfill(context.WithoutCancel(r.Context()), h.userID(r.Context()))
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		if !started {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": started})
		return
	}
	http.Redirect(w, r, "/admin/contacts", http.StatusSeeOther)
}

func (h *Handler) startContactBackfill(ctx context.Context, userID string) bool {
	h.contactBackfillMu.Lock()
	if h.contactBackfillState.InProgress {
		h.contactBackfillMu.Unlock()
		return false
	}
	total, err := h.db.CountObservedContactBackfillCandidates(ctx, userID)
	if err != nil {
		total = 0
	}
	state := models.ContactBackfillState{InProgress: true, Total: total, StartedAt: time.Now().UTC()}
	h.contactBackfillState = state
	h.contactBackfillMu.Unlock()
	h.publishContactBackfill(userID, state)

	_ = h.db.LogContactActivity(ctx, userID, "backfill_forced", "", "Manual contact backfill requested", 0)
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := h.db.BackfillObservedContactsWithProgress(backfillCtx, userID, func(processed int) {
			h.contactBackfillMu.Lock()
			h.contactBackfillState.Processed = processed
			state := h.contactBackfillState
			h.contactBackfillMu.Unlock()
			h.publishContactBackfill(userID, state)
		})
		h.contactBackfillMu.Lock()
		h.contactBackfillState.InProgress = false
		h.contactBackfillState.FinishedAt = time.Now().UTC()
		if err != nil {
			h.contactBackfillState.LastError = err.Error()
			log.Printf("contacts: manual backfill failed: %v", err)
		} else {
			h.contactBackfillState.LastError = ""
			h.contactBackfillState.Processed = h.contactBackfillState.Total
		}
		state := h.contactBackfillState
		h.contactBackfillMu.Unlock()
		h.publishContactBackfill(userID, state)
	}()
	return true
}

func (h *Handler) getContactBackfillState() models.ContactBackfillState {
	h.contactBackfillMu.RLock()
	defer h.contactBackfillMu.RUnlock()
	return h.contactBackfillState
}

func (h *Handler) publishContactBackfill(userID string, state models.ContactBackfillState) {
	if h.syncer == nil {
		return
	}
	h.syncer.Events().Publish(mail.Event{Type: mail.EventContactBackfill, Payload: map[string]any{"user_id": userID, "backfill": state}})
}

func adminAvatarTab(r *http.Request) (string, bool) {
	tab := r.PathValue("tab")
	if tab == "" {
		return "overview", true
	}
	switch tab {
	case "overview", "senders", "providers", "events":
		return tab, true
	default:
		return "", false
	}
}
