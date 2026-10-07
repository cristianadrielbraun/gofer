package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errUserMessageMutationUnsupported = errors.New("routed message mutation provider is not available")

// This state exists only during one leased local request. Events and worker wakes
// are delivered after the lease is released; no shared provider worker is used.
type userMessageMutationState struct {
	owner       string
	routing     *storage.AccountRouting
	accounts    *config.AccountStore
	events      []mail.Event
	checked     map[string]bool
	wake        map[string]bool
	credentials bool
}

func (h *Handler) checkUserMessageMutation(ctx context.Context, info *storage.MessageMutationInfo) error {
	s := h.userMutationState
	if s == nil || s.checked[info.AccountID] {
		return nil
	}
	state, err := s.routing.AccountStateForUser(ctx, s.owner, info.AccountID)
	if err != nil || state != storage.AccountActive {
		return errMessageTargetNotFound
	}
	cfg, err := s.accounts.GetConfig(ctx, info.AccountID)
	if err != nil {
		return err
	}
	if !((cfg.Provider == "imap" && cfg.AuthMethod == "plain") || (s.credentials && (cfg.Provider == "gmail" || cfg.Provider == "outlook") && cfg.AuthMethod == "oauth2")) {
		return errUserMessageMutationUnsupported
	}
	s.checked[info.AccountID] = true
	return nil
}

func (h *Handler) messageMutationContext(ctx context.Context) context.Context {
	if h.userMutationState != nil {
		return ctx
	}
	return context.WithoutCancel(ctx)
}

type userMutationResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *userMutationResponse) Header() http.Header { return w.header }
func (w *userMutationResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *userMutationResponse) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(data)
}

func (h *Handler) userMessageMutationHandler(selectHandler func(*Handler) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		stop := context.AfterFunc(h.userStorageContext, cancel)
		defer stop()
		r = r.WithContext(ctx)
		// Read the bounded payload before leasing storage. A stalled browser
		// upload must not occupy the owner's store or a shared cache slot.
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(payload))
		state := &userMessageMutationState{owner: h.userID(ctx), routing: h.userStorage, checked: make(map[string]bool), credentials: h.userCredentials != nil}
		response := &userMutationResponse{header: make(http.Header)}
		err = h.userAccounts.WithUser(ctx, state.owner, func(accounts *config.AccountStore, db *storage.DB) error {
			state.accounts = accounts
			defer func() { state.accounts = nil }()
			local := &Handler{db: db, auth: h.auth, userMutationState: state}
			// Only these local queue-writing handlers are mounted. Their worker
			// signal has no receiver here; their events are collected, not sent.
			selectHandler(local)(response, r)
			return nil
		})
		if err != nil {
			log.Printf("user mail changes unavailable: %v", err)
			http.Error(w, "mailbox unavailable", http.StatusServiceUnavailable)
			return
		}
		wake := make(map[string]bool)
		for _, event := range state.events {
			h.userIMAP.Events().Publish(event)
			wake[event.AccountID] = true
		}
		for id := range wake {
			if err := h.userIMAP.WakeMutations(ctx, state.owner, id); err != nil {
				log.Printf("user mail changes saved; queue wake %s: %v", id, err)
				// Retain the successful local result: retrying a toggle can undo it,
				// and repeating a move to Trash can become a permanent deletion.
				response.header.Set("X-Gofer-Mail-Delivery", "delayed")
			}
		}
		for key, values := range response.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		if response.status == 0 {
			response.status = http.StatusOK
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}
}

func (h *Handler) registerUserMessageMutations(private func(string, http.HandlerFunc)) {
	for _, route := range []struct {
		pattern string
		handler func(*Handler) http.HandlerFunc
	}{
		{"POST /api/messages/label", func(h *Handler) http.HandlerFunc { return h.handleLabelMessages }},
		{"POST /api/messages/unlabel", func(h *Handler) http.HandlerFunc { return h.handleUnlabelMessages }},
		{"POST /api/messages/{id}/label", func(h *Handler) http.HandlerFunc { return h.handleLabelMessage }},
		{"POST /api/messages/{id}/unlabel", func(h *Handler) http.HandlerFunc { return h.handleUnlabelMessage }},
		{"POST /api/messages/spam", func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesSpam }},
		{"POST /api/messages/not-spam", func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesNotSpam }},
		{"POST /api/messages/read", func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesRead }},
		{"POST /api/messages/star", func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesStarred }},
		{"POST /api/messages/archive", func(h *Handler) http.HandlerFunc { return h.handleArchiveMessages }},
		{"POST /api/messages/delete", func(h *Handler) http.HandlerFunc { return h.handleDeleteMessages }},
		{"POST /api/messages/move", func(h *Handler) http.HandlerFunc { return h.handleMoveMessages }},
		{"POST /api/messages/{id}/read", func(h *Handler) http.HandlerFunc { return h.handleToggleRead }},
		{"POST /api/messages/{id}/star", func(h *Handler) http.HandlerFunc { return h.handleToggleStar }},
		{"POST /api/messages/{id}/thread/read", func(h *Handler) http.HandlerFunc { return h.handleToggleThreadRead }},
		{"POST /api/messages/{id}/thread/archive", func(h *Handler) http.HandlerFunc { return h.handleArchiveThread }},
		{"DELETE /api/messages/{id}/thread", func(h *Handler) http.HandlerFunc { return h.handleDeleteThread }},
		{"DELETE /api/messages/{id}", func(h *Handler) http.HandlerFunc { return h.handleDeleteMessage }},
		{"POST /api/messages/{id}/move", func(h *Handler) http.HandlerFunc { return h.handleMoveMessage }},
	} {
		private(route.pattern, h.userMessageMutationHandler(route.handler))
	}
}

func (h *Handler) queueUserSpamTargets(w http.ResponseWriter, r *http.Request, targets []ownedMessageTarget, disposition spamDisposition) {
	updated, messages, failed := 0, 0, 0
	for _, target := range targets {
		destination, _, err := h.spamDestinationFolder(r.Context(), target.Infos[0].AccountID, disposition)
		if err != nil {
			log.Printf("user spam destination %s: %v", target.Infos[0].AccountID, err)
			failed += len(target.Infos)
			continue
		}
		if target.Infos[0].AccountProvider == "imap" {
			if err := h.db.QueueUserIMAPSpam(r.Context(), h.userMutationState.owner, target.Infos, destination, disposition == spamDispositionSpam); err != nil {
				log.Printf("queue user IMAP spam %s: %v", target.Infos[0].AccountID, err)
				failed += len(target.Infos)
				continue
			}
		} else {
			var moving []storage.ThreadMessageMutationInfo
			for _, info := range target.Infos {
				if info.FolderID != destination {
					moving = append(moving, info)
				}
			}
			if len(moving) == 0 {
				continue
			}
			if err := h.queueMessageMoves(r.Context(), moving, destination); err != nil {
				failed += len(moving)
				continue
			}
		}
		updated++
		messages += len(target.Infos)
		h.publishSpamMutation(target.Infos, destination)
	}
	if messages == 0 && failed > 0 {
		http.Error(w, "spam action could not be saved", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"updated": updated, "messages": messages, "failed": failed, "remote_failed": 0})
}
