package handler

import (
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const sseKeepaliveInterval = 20 * time.Second

// Streams share only the event bus. Snapshots are copied before writing, and
// current central ownership is checked even for accounts added after connection.
func (h *Handler) handleUserSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, i18n.T(r.Context(), "server.error.streaming_not_supported"), http.StatusInternalServerError)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	releaseActive, err := h.userIMAP.BeginActiveUserSession(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	defer releaseActive()
	go h.wakeUserCalendarOnline(ctx, owner)
	// Events here name their owner; AnyAccount also passes those naming only
	// an account, whose ownership writeEvent checks.
	events := h.userIMAP.Events().SubscribeFor(mail.SubscribeOptions{UserID: owner, AnyAccount: true})
	defer events.Close()
	manual, err := h.userIMAP.ManualSyncSnapshot(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	idle, err := h.userIMAP.IdleEventsForUser(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()
	writeEvent := func(event mail.Event) bool {
		if !sseEventVisible(event, owner, map[string]bool{event.AccountID: true}, false) {
			return true
		}
		if err := h.userStorage.ValidateUser(ctx, owner); err != nil {
			return false
		}
		if event.AccountID != "" {
			state, err := h.userStorage.AccountStateForUser(ctx, owner, event.AccountID)
			if err != nil || state != storage.AccountActive {
				return true
			}
		}
		writeSSEEvent(ctx, w, flusher, event)
		return true
	}
	for _, event := range manual {
		if !writeEvent(event) {
			return
		}
	}
	for _, event := range idle {
		if !writeEvent(event) {
			return
		}
	}
	tick := time.NewTicker(1200 * time.Millisecond)
	defer tick.Stop()
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.userStorageContext.Done():
			return
		case <-keepalive.C:
			// Comment lines keep idle proxies from closing the stream.
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-tick.C:
			if err := h.userStorage.ValidateUser(ctx, owner); err != nil {
				return
			}
		case <-events.Ready():
			for _, event := range events.Take() {
				if !writeEvent(event) {
					return
				}
			}
		}
	}
}
