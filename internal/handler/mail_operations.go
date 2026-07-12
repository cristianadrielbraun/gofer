package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

type mailOperationResponse struct {
	ID                    string `json:"id"`
	Type                  string `json:"type"`
	AccountID             string `json:"account_id"`
	AccountEmail          string `json:"account_email,omitempty"`
	Provider              string `json:"provider"`
	MessageID             int64  `json:"message_id,omitempty"`
	FolderID              string `json:"folder_id,omitempty"`
	FolderName            string `json:"folder_name,omitempty"`
	DestinationFolderID   string `json:"destination_folder_id,omitempty"`
	DestinationFolderName string `json:"destination_folder_name,omitempty"`
	LabelName             string `json:"label_name,omitempty"`
	Operation             string `json:"operation"`
	DraftKey              string `json:"draft_key,omitempty"`
	State                 string `json:"state"`
	Attempts              int    `json:"attempts"`
	LastError             string `json:"last_error,omitempty"`
	NextRetryAt           string `json:"next_retry_at,omitempty"`
	CreatedAt             string `json:"created_at,omitempty"`
	UpdatedAt             string `json:"updated_at,omitempty"`
	CanRetry              bool   `json:"can_retry"`
	CanReconcile          bool   `json:"can_reconcile"`
	CanCancel             bool   `json:"can_cancel"`
	Ambiguous             bool   `json:"ambiguous"`
}

type mailOperationsResponse struct {
	Operations     []mailOperationResponse `json:"operations"`
	Total          int                     `json:"total"`
	ActionRequired int                     `json:"action_required"`
}

func mailOperationTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func mailOperationResponseFrom(operation models.MailOperationSummary) mailOperationResponse {
	return mailOperationResponse{
		ID:                    operation.ID,
		Type:                  operation.Type,
		AccountID:             operation.AccountID,
		AccountEmail:          operation.AccountEmail,
		Provider:              operation.Provider,
		MessageID:             operation.MessageID,
		FolderID:              operation.FolderID,
		FolderName:            operation.FolderName,
		DestinationFolderID:   operation.DestinationFolderID,
		DestinationFolderName: operation.DestinationFolderName,
		LabelName:             operation.LabelName,
		Operation:             operation.Operation,
		DraftKey:              operation.DraftKey,
		State:                 operation.State,
		Attempts:              operation.Attempts,
		LastError:             operation.LastError,
		NextRetryAt:           mailOperationTime(operation.NextRetryAt),
		CreatedAt:             mailOperationTime(operation.CreatedAt),
		UpdatedAt:             mailOperationTime(operation.UpdatedAt),
		CanRetry:              operation.CanRetry,
		CanReconcile:          operation.CanReconcile,
		CanCancel:             operation.CanCancel,
		Ambiguous:             operation.Ambiguous,
	}
}

func mailOperationsResponseFrom(status models.MailOperationsStatus) mailOperationsResponse {
	response := mailOperationsResponse{
		Operations:     make([]mailOperationResponse, 0, len(status.Operations)),
		Total:          status.Total,
		ActionRequired: status.ActionRequired,
	}
	for _, operation := range status.Operations {
		response.Operations = append(response.Operations, mailOperationResponseFrom(operation))
	}
	return response
}

func writeMailOperationsJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *Handler) handleMailOperations(w http.ResponseWriter, r *http.Request) {
	status, err := h.mailOperationsStatus(r.Context())
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load mail operations"})
		return
	}
	writeMailOperationsJSON(w, http.StatusOK, mailOperationsResponseFrom(status))
}

func (h *Handler) handleSettingsMailOperationsContent(w http.ResponseWriter, r *http.Request) {
	status, err := h.mailOperationsStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to load mail operations", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SettingsOperationsContent(status).Render(r.Context(), w); err != nil {
		return
	}
}

func (h *Handler) mailOperationsStatus(ctx context.Context) (models.MailOperationsStatus, error) {
	operations, err := h.db.ListMailOperationsForUser(ctx, h.userID(ctx))
	if err != nil {
		return models.MailOperationsStatus{}, err
	}
	status := models.MailOperationsStatus{Operations: operations, Total: len(operations)}
	for _, operation := range operations {
		if operation.CanRetry || operation.CanReconcile || operation.CanCancel {
			status.ActionRequired++
		}
	}
	return status, nil
}

func (h *Handler) handleRetryMailOperation(w http.ResponseWriter, r *http.Request) {
	operationID := strings.TrimSpace(r.PathValue("id"))
	if operationID == "" {
		writeMailOperationsJSON(w, http.StatusBadRequest, map[string]string{"error": "operation id is required"})
		return
	}
	operation, err := h.db.RetryMailOperationForUser(r.Context(), h.userID(r.Context()), operationID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrMailOperationNotRetryable) {
		writeMailOperationsJSON(w, http.StatusConflict, map[string]string{"error": "this mail operation cannot be retried in its current state"})
		return
	}
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to retry mail operation"})
		return
	}
	h.signalMailOperation(operation)
	writeMailOperationsJSON(w, http.StatusOK, mailOperationResponseFrom(operation))
}

func (h *Handler) handleAdminMailOperationsStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.db.ListMailOperationsAdminStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get mail operation status", http.StatusInternalServerError)
		return
	}
	writeMailOperationsJSON(w, http.StatusOK, status)
}

func (h *Handler) handleAdminOperations(w http.ResponseWriter, r *http.Request) {
	status, err := h.db.ListMailOperationsAdminStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get mail operation status", http.StatusInternalServerError)
		return
	}
	uiSettings := h.db.GetUISettings(r.Context(), h.userID(r.Context()))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		_ = views.AdminPartial(models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, status, "operations", "").Render(r.Context(), w)
		return
	}
	_ = views.AdminLayout(uiSettings, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, status, "operations", "").Render(r.Context(), w)
}

func (h *Handler) signalMailOperation(operation models.MailOperationSummary) {
	switch operation.Type {
	case models.MailOperationMessageMutation:
		h.signalMessageMutationWorker()
	case models.MailOperationIMAPDraft, models.MailOperationSentCopy:
		h.signalOutgoingWorker()
	case models.MailOperationLabelMutation:
		if h.syncer != nil && operation.AccountID != "" {
			h.syncer.SyncAccount(context.Background(), operation.AccountID)
		}
	}
}
