package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// outgoingSendSummaryResponse is intentionally a small, user-scoped view of
// an outbox row. In particular, it never includes the stored MIME payload or
// the SMTP envelope.
type outgoingSendSummaryResponse struct {
	ID                    string `json:"id"`
	AccountID             string `json:"account_id"`
	MessageID             int64  `json:"message_id,omitempty"`
	DraftID               string `json:"draft_id,omitempty"`
	Transport             string `json:"transport"`
	Status                string `json:"status"`
	AttemptCount          int    `json:"attempt_count"`
	LastError             string `json:"last_error,omitempty"`
	SendAfter             string `json:"send_after,omitempty"`
	NextAttemptAt         string `json:"next_attempt_at,omitempty"`
	IsScheduled           bool   `json:"is_scheduled"`
	SentCopyStatus        string `json:"sent_copy_status"`
	SentCopyAttemptCount  int    `json:"sent_copy_attempt_count"`
	SentCopyLastError     string `json:"sent_copy_last_error,omitempty"`
	SentCopyNextAttemptAt string `json:"sent_copy_next_attempt_at,omitempty"`
	CreatedAt             string `json:"created_at,omitempty"`
	UpdatedAt             string `json:"updated_at,omitempty"`

	CanRetry                 bool `json:"can_retry"`
	CanRetryNow              bool `json:"can_retry_now"`
	CanCancel                bool `json:"can_cancel"`
	AmbiguousWarningRequired bool `json:"ambiguous_warning_required"`
}

type outgoingSendListResponse struct {
	Sends []outgoingSendSummaryResponse `json:"sends"`
}

type outgoingSendConfirmRequest struct {
	Confirm bool `json:"confirm"`
}

func outgoingSendTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func outgoingSendSummaryResponseFrom(summary storage.OutgoingSendSummary) outgoingSendSummaryResponse {
	return outgoingSendSummaryResponse{
		ID:                    summary.ID,
		AccountID:             summary.AccountID,
		MessageID:             summary.MessageID,
		DraftID:               summary.DraftID,
		Transport:             summary.Transport,
		Status:                summary.Status,
		AttemptCount:          summary.AttemptCount,
		LastError:             summary.LastError,
		SendAfter:             outgoingSendTime(summary.SendAfter),
		NextAttemptAt:         outgoingSendTime(summary.NextAttemptAt),
		IsScheduled:           summary.IsScheduled,
		SentCopyStatus:        summary.SentCopyStatus,
		SentCopyAttemptCount:  summary.SentCopyAttempts,
		SentCopyLastError:     summary.SentCopyLastError,
		SentCopyNextAttemptAt: outgoingSendTime(summary.SentCopyNextTry),
		CreatedAt:             outgoingSendTime(summary.CreatedAt),
		UpdatedAt:             outgoingSendTime(summary.UpdatedAt),

		CanRetry:                 summary.Status == storage.OutgoingSendFailed || summary.Status == storage.OutgoingSendAmbiguous,
		CanRetryNow:              summary.Status == storage.OutgoingSendPending,
		CanCancel:                summary.Status == storage.OutgoingSendPending || summary.Status == storage.OutgoingSendFailed || summary.Status == storage.OutgoingSendAmbiguous,
		AmbiguousWarningRequired: summary.Status == storage.OutgoingSendAmbiguous,
	}
}

func writeOutgoingSendJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOutgoingSendError(w http.ResponseWriter, status int, message string, ambiguous bool) {
	writeOutgoingSendJSON(w, status, map[string]any{
		"error":                      message,
		"ambiguous_warning_required": ambiguous,
	})
}

func (h *Handler) handleOutgoingSendList(w http.ResponseWriter, r *http.Request) {
	summaries, err := h.db.ListOutgoingSendSummariesForUser(r.Context(), h.userID(r.Context()))
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to load outgoing send status", false)
		return
	}
	response := outgoingSendListResponse{Sends: make([]outgoingSendSummaryResponse, 0, len(summaries))}
	for _, summary := range summaries {
		response.Sends = append(response.Sends, outgoingSendSummaryResponseFrom(summary))
	}
	writeOutgoingSendJSON(w, http.StatusOK, response)
}

func (h *Handler) handleOutgoingSendGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOutgoingSendError(w, http.StatusBadRequest, "outgoing send id is required", false)
		return
	}
	summary, err := h.db.GetOutgoingSendSummaryForUser(r.Context(), h.userID(r.Context()), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to load outgoing send status", false)
		return
	}
	writeOutgoingSendJSON(w, http.StatusOK, outgoingSendSummaryResponseFrom(summary))
}

func (h *Handler) parseOutgoingSendConfirmation(r *http.Request) (bool, error) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType == "application/json" {
		var request outgoingSendConfirmRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return false, err
		}
		return request.Confirm, nil
	}
	if err := r.ParseForm(); err != nil {
		return false, err
	}
	value := strings.ToLower(strings.TrimSpace(r.FormValue("confirm")))
	return value == "1" || value == "true" || value == "yes" || value == "on", nil
}

func (h *Handler) handleOutgoingSendRetry(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOutgoingSendError(w, http.StatusBadRequest, "outgoing send id is required", false)
		return
	}
	confirm, err := h.parseOutgoingSendConfirmation(r)
	if err != nil {
		writeOutgoingSendError(w, http.StatusBadRequest, "invalid retry request", false)
		return
	}
	send, err := h.db.RetryOutgoingSend(r.Context(), h.userID(r.Context()), id, confirm)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrOutgoingSendAmbiguousConfirmation) {
		writeOutgoingSendError(w, http.StatusConflict, "Gofer lost the connection after sending this message, so it may already have been delivered. Check Sent before retrying. Retrying can send a duplicate.", true)
		return
	}
	if errors.Is(err, storage.ErrOutgoingSendNotRetryable) {
		writeOutgoingSendError(w, http.StatusConflict, "this outgoing send cannot be retried in its current state", false)
		return
	}
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to retry outgoing send", false)
		return
	}
	h.signalOutgoingWorker()
	h.writeOutgoingSendResponse(w, r, send)
}

func (h *Handler) handleOutgoingSendRetryNow(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOutgoingSendError(w, http.StatusBadRequest, "outgoing send id is required", false)
		return
	}
	send, err := h.db.RetryOutgoingSendNow(r.Context(), h.userID(r.Context()), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrOutgoingSendNotRetryable) {
		writeOutgoingSendError(w, http.StatusConflict, "this outgoing send cannot be retried now", false)
		return
	}
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to retry outgoing send now", false)
		return
	}
	h.signalOutgoingWorker()
	h.writeOutgoingSendResponse(w, r, send)
}

func (h *Handler) handleOutgoingSendCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOutgoingSendError(w, http.StatusBadRequest, "outgoing send id is required", false)
		return
	}
	send, err := h.db.CancelOutgoingSend(r.Context(), h.userID(r.Context()), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrOutgoingSendNotCancelable) {
		writeOutgoingSendError(w, http.StatusConflict, "this outgoing send cannot be canceled in its current state", false)
		return
	}
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to cancel outgoing send", false)
		return
	}
	h.writeOutgoingSendResponse(w, r, send)
}

func (h *Handler) writeOutgoingSendResponse(w http.ResponseWriter, r *http.Request, send storage.OutgoingSend) {
	summary, err := h.db.GetOutgoingSendSummaryForUser(r.Context(), h.userID(r.Context()), send.ID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeOutgoingSendError(w, http.StatusInternalServerError, "failed to load outgoing send status", false)
		return
	}
	writeOutgoingSendJSON(w, http.StatusOK, outgoingSendSummaryResponseFrom(summary))
}
