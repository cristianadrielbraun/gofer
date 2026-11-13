package handler

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// normalizeSubjectThreadingSettings stores the subject grouping window and
// size limit as the numbers storage uses.
func normalizeSubjectThreadingSettings(updates map[string]string) {
	if value, ok := updates["subject_threading_window_days"]; ok {
		updates["subject_threading_window_days"] = strconv.Itoa(storage.SubjectThreadingWindowDays(value))
	}
	if value, ok := updates["subject_threading_max_messages"]; ok {
		updates["subject_threading_max_messages"] = strconv.Itoa(storage.SubjectThreadingMaxMessages(value))
	}
}

// regroupBatchSize is how many messages one transaction relinks, so sync can
// write between batches.
const regroupBatchSize = 500

// regroupProgress is how a user's regroup is going, as the events tell it.
type regroupProgress struct {
	processed, total int
}

// handleRegroupConversations starts applying the subject grouping settings
// to the mail already saved: grouping it when they were turned on, splitting
// it when they were turned off. It runs in the background and reports its
// progress in conversation-regroup events, which pages show in a toast; a
// user runs one at a time, and asking again repeats where it is.
func (h *Handler) handleRegroupConversations(w http.ResponseWriter, r *http.Request) {
	userID := h.userID(r.Context())
	if current, running := h.startRegroup(userID); running {
		h.publishRegroup(userID, "running", current)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	job := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		h.runRegroup(ctx, userID)
	}
	if h.userIMAP != nil {
		// Shutdown stops and waits for it.
		if err := h.userIMAP.StartBackgroundService(context.Background(), job); err != nil {
			h.finishRegroup(userID)
			h.publishRegroup(userID, "failed", regroupProgress{})
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	} else {
		go job(context.Background())
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) runRegroup(ctx context.Context, userID string) {
	defer h.finishRegroup(userID)
	started := time.Now()
	var ids []int64
	if err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		var err error
		ids, err = db.SubjectRegroupMessageIDs(ctx, userID)
		return err
	}); err != nil {
		log.Printf("threading: list messages to regroup: %v", err)
		h.publishRegroup(userID, "failed", regroupProgress{})
		return
	}
	progress := regroupProgress{total: len(ids)}
	h.setRegroupProgress(userID, progress)
	h.publishRegroup(userID, "running", progress)
	for start := 0; start < len(ids); start += regroupBatchSize {
		batch := ids[start:min(start+regroupBatchSize, len(ids))]
		if err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
			return db.RegroupSubjectThreads(ctx, userID, batch)
		}); err != nil {
			log.Printf("threading: regroup conversations: %v", err)
			h.publishRegroup(userID, "failed", progress)
			if start > 0 {
				h.publishUserMutation(userID)
			}
			return
		}
		progress.processed = start + len(batch)
		h.setRegroupProgress(userID, progress)
		h.publishRegroup(userID, "running", progress)
	}
	log.Printf("threading: regrouped %d messages by subject in %s", len(ids), time.Since(started).Round(time.Millisecond))
	h.publishRegroup(userID, "done", progress)
	if len(ids) > 0 {
		h.publishUserMutation(userID)
	}
}

// startRegroup claims the user's regroup, or reports how the one already
// running is going.
func (h *Handler) startRegroup(userID string) (regroupProgress, bool) {
	h.regroupMu.Lock()
	defer h.regroupMu.Unlock()
	if h.regroups == nil {
		h.regroups = map[string]regroupProgress{}
	}
	if current, running := h.regroups[userID]; running {
		return current, true
	}
	h.regroups[userID] = regroupProgress{}
	return regroupProgress{}, false
}

func (h *Handler) setRegroupProgress(userID string, progress regroupProgress) {
	h.regroupMu.Lock()
	defer h.regroupMu.Unlock()
	if _, running := h.regroups[userID]; running {
		h.regroups[userID] = progress
	}
}

func (h *Handler) finishRegroup(userID string) {
	h.regroupMu.Lock()
	defer h.regroupMu.Unlock()
	delete(h.regroups, userID)
}

// publishRegroup tells the user's pages how their regroup is going: state
// is running, done or failed.
func (h *Handler) publishRegroup(userID, state string, progress regroupProgress) {
	h.publishUserEvent(mail.Event{Type: mail.EventConversationRegroup, UserID: userID, Payload: map[string]any{
		"state": state, "processed": progress.processed, "total": progress.total,
	}})
}

// publishUserMutation tells the user's open pages their mail lists changed.
func (h *Handler) publishUserMutation(userID string) {
	h.publishUserEvent(mail.Event{Type: mail.EventMutation, UserID: userID})
}

func (h *Handler) publishUserEvent(event mail.Event) {
	switch {
	case h.userIMAP != nil:
		h.userIMAP.Events().Publish(event)
	case h.syncer != nil:
		h.syncer.Events().Publish(event)
	}
}
