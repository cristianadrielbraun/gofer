package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"slices"
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
	Parked                bool   `json:"parked"`
	ParkedReason          string `json:"parked_reason,omitempty"`
	CanUndo               bool   `json:"can_undo"`
}

type mailOperationsResponse struct {
	Operations     []mailOperationResponse `json:"operations"`
	Total          int                     `json:"total"`
	ActionRequired int                     `json:"action_required"`
	Retryable      int                     `json:"retryable"`
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
		Parked:                operation.Parked,
		ParkedReason:          operation.ParkedReason,
		CanUndo:               operation.CanUndo,
	}
}

func mailOperationsResponseFrom(status models.MailOperationsStatus) mailOperationsResponse {
	response := mailOperationsResponse{
		Operations:     make([]mailOperationResponse, 0, len(status.Operations)),
		Total:          status.Total,
		ActionRequired: status.ActionRequired,
		Retryable:      status.Retryable,
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

// mailOperationOpenGroupsLimit is how many open groups a reload of the list
// fills in; any others load their messages again when opened.
const mailOperationOpenGroupsLimit = 10

// handleSettingsMailOperationsContent renders the Mail operations list. The
// open parameters name the groups the page has open, which come back with
// their messages already listed, so a reload doesn't empty them.
func (h *Handler) handleSettingsMailOperationsContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	list, err := h.mailOperationsList(r.Context(), r.URL.Query()["open"])
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_mail_operations"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SettingsOperationsContent(list).Render(r.Context(), w); err != nil {
		return
	}
}

// handleSettingsMailActionChanges renders the messages of one group in the
// Mail operations list. An action of another user lists nothing.
func (h *Handler) handleSettingsMailActionChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	actionID := strings.TrimSpace(r.PathValue("id"))
	var changes []models.MailOperationGroupMessage
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		changes, err = db.ListMailActionMessagesForUser(ctx, h.userID(ctx), actionID, views.MailOperationGroupChangesLimit)
		return err
	})
	if err != nil {
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_mail_operations"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailOperationGroupChanges(changes).Render(ctx, w)
}

// mailOperationsList is what the list in Settings shows: one row per action,
// then the changes with no recorded action. The groups named in open, up to
// mailOperationOpenGroupsLimit, come with their messages.
func (h *Handler) mailOperationsList(ctx context.Context, open []string) (models.MailOperationsList, error) {
	var list models.MailOperationsList
	owner := h.userID(ctx)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if list.Groups, err = db.ListMailOperationGroupsForUser(ctx, owner); err != nil {
			return err
		}
		if list.Operations, err = db.ListUngroupedMailOperationsForUser(ctx, owner); err != nil {
			return err
		}
		retryable, err := db.ListRetryableMailOperationsForUser(ctx, owner)
		if err != nil {
			return err
		}
		list.Retryable = len(retryable)
		filled := 0
		for i := range list.Groups {
			if filled >= mailOperationOpenGroupsLimit || !slices.Contains(open, list.Groups[i].ActionID) {
				continue
			}
			filled++
			if list.Groups[i].Changes, err = db.ListMailActionMessagesForUser(ctx, owner, list.Groups[i].ActionID, views.MailOperationGroupChangesLimit); err != nil {
				return err
			}
			list.Groups[i].ChangesLoaded = true
		}
		return nil
	})
	if err != nil {
		return models.MailOperationsList{}, err
	}
	for _, group := range list.Groups {
		if group.NeedsAttention() {
			list.ActionRequired++
		}
	}
	for _, operation := range list.Operations {
		if operation.NeedsAttention() {
			list.ActionRequired++
		}
	}
	return list, nil
}

func (h *Handler) mailOperationsStatus(ctx context.Context) (models.MailOperationsStatus, error) {
	var operations, retryable []models.MailOperationSummary
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		if operations, err = db.ListMailOperationsForUser(ctx, h.userID(ctx)); err != nil {
			return err
		}
		retryable, err = db.ListRetryableMailOperationsForUser(ctx, h.userID(ctx))
		return err
	})
	if err != nil {
		return models.MailOperationsStatus{}, err
	}
	status := models.MailOperationsStatus{Operations: operations, Total: len(operations), Retryable: len(retryable)}
	for _, operation := range operations {
		if operation.NeedsAttention() {
			status.ActionRequired++
		}
	}
	return status, nil
}

func (h *Handler) handleRetryMailOperation(w http.ResponseWriter, r *http.Request) {
	operationID := strings.TrimSpace(r.PathValue("id"))
	if operationID == "" {
		writeMailOperationsJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(r.Context(), "server.msg.operation_id_is_required")})
		return
	}
	operation, err := h.db.RetryMailOperationForUser(r.Context(), h.userID(r.Context()), operationID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrMailOperationNotRetryable) {
		writeMailOperationsJSON(w, http.StatusConflict, map[string]string{"error": i18n.T(r.Context(), "server.msg.this_mail_operation_cannot_be_retried_in")})
		return
	}
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(r.Context(), "server.msg.failed_to_retry_mail_operation")})
		return
	}
	h.signalMailOperation(operation)
	writeMailOperationsJSON(w, http.StatusOK, mailOperationResponseFrom(operation))
}

// retryFailedMailOperationsResponse says how many operations Retry all
// failed scheduled again, and how many it couldn't.
type retryFailedMailOperationsResponse struct {
	Retried int `json:"retried"`
	Skipped int `json:"skipped"`
}

// groupMailOperationsByAccount returns the operations' IDs by account, and
// the accounts in the order they first appear.
func groupMailOperationsByAccount(operations []models.MailOperationSummary) ([]string, map[string][]string) {
	var accounts []string
	byAccount := map[string][]string{}
	for _, operation := range operations {
		if _, ok := byAccount[operation.AccountID]; !ok {
			accounts = append(accounts, operation.AccountID)
		}
		byAccount[operation.AccountID] = append(byAccount[operation.AccountID], operation.ID)
	}
	return accounts, byAccount
}

func (h *Handler) handleRetryFailedMailOperations(w http.ResponseWriter, r *http.Request) {
	userID := h.userID(r.Context())
	operations, err := h.db.ListRetryableMailOperationsForUser(r.Context(), userID)
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(r.Context(), "server.msg.failed_to_retry_mail_operation")})
		return
	}
	ids := make([]string, 0, len(operations))
	for _, operation := range operations {
		ids = append(ids, operation.ID)
	}
	retried, skipped, err := h.db.RetryMailOperationsForUserGuarded(r.Context(), userID, ids, nil)
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(r.Context(), "server.msg.failed_to_retry_mail_operation")})
		return
	}
	// Wake each kind of queue once per account rather than once per change.
	woken := map[[2]string]bool{}
	for _, operation := range operations {
		key := [2]string{operation.Type, operation.AccountID}
		if !woken[key] {
			woken[key] = true
			h.signalMailOperation(operation)
		}
	}
	writeMailOperationsJSON(w, http.StatusOK, retryFailedMailOperationsResponse{Retried: retried, Skipped: skipped})
}

// mailActionChangeResponse says how many of an action's changes a group
// Retry or Undo changed.
type mailActionChangeResponse struct {
	Changed int `json:"changed"`
	Skipped int `json:"skipped"`
}

// handleRetryMailAction retries every failed change one action queued.
func (h *Handler) handleRetryMailAction(w http.ResponseWriter, r *http.Request) {
	h.changeMailAction(w, r, true)
}

// handleUndoMailAction undoes, in Gofer only, every change one action queued
// that can't be completed.
func (h *Handler) handleUndoMailAction(w http.ResponseWriter, r *http.Request) {
	h.changeMailAction(w, r, false)
}

func (h *Handler) changeMailAction(w http.ResponseWriter, r *http.Request, retry bool) {
	ctx := r.Context()
	owner, actionID := h.userID(ctx), strings.TrimSpace(r.PathValue("id"))
	accounts, err := h.db.MailActionAccountsForUser(ctx, owner, actionID)
	if err != nil {
		writeMailActionError(w, r, retry, err)
		return
	}
	result := mailActionChangeResponse{}
	for _, account := range accounts {
		var changed int
		if retry {
			changed, err = h.db.RetryMailActionForUser(ctx, owner, actionID, account, nil)
		} else {
			changed, err = h.db.UndoMailActionForUser(ctx, owner, actionID, account, nil)
		}
		if err != nil {
			log.Printf("mail action %s: account %s: %v", actionID, account, err)
			result.Skipped++
			continue
		}
		result.Changed += changed
		if retry && changed > 0 {
			h.signalMessageMutationWorker()
			if h.syncer != nil {
				h.syncer.SyncAccount(context.Background(), account)
			}
		}
	}
	writeMailActionResult(w, r, retry, result)
}

// writeMailActionResult answers a group Retry or Undo. Changing nothing, also
// when an account refused, is a conflict, worded as for a single change.
func writeMailActionResult(w http.ResponseWriter, r *http.Request, retry bool, result mailActionChangeResponse) {
	if result.Changed == 0 {
		key := "server.msg.mail_operation_cannot_be_undone"
		if retry {
			key = "server.msg.this_mail_operation_cannot_be_retried_in"
		}
		writeMailOperationsJSON(w, http.StatusConflict, map[string]string{"error": i18n.T(r.Context(), key)})
		return
	}
	writeMailOperationsJSON(w, http.StatusOK, result)
}

func writeMailActionError(w http.ResponseWriter, r *http.Request, retry bool, err error) {
	log.Printf("mail action: %v", err)
	key := "server.msg.failed_to_undo_mail_operation"
	if retry {
		key = "server.msg.failed_to_retry_mail_operation"
	}
	writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(r.Context(), key)})
}

func (h *Handler) handleUndoMailOperation(w http.ResponseWriter, r *http.Request) {
	operationID := strings.TrimSpace(r.PathValue("id"))
	if operationID == "" {
		writeMailOperationsJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(r.Context(), "server.msg.operation_id_is_required")})
		return
	}
	operation, err := h.db.UndoMailOperationForUser(r.Context(), h.userID(r.Context()), operationID, nil)
	h.writeMailOperationUndoResult(w, r, operation, err)
}

// writeMailOperationUndoResult answers an undo request with the operation as
// it was before the undo, or an error people can read.
func (h *Handler) writeMailOperationUndoResult(w http.ResponseWriter, r *http.Request, operation models.MailOperationSummary, err error) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrMailOperationNotUndoable) {
		writeMailOperationsJSON(w, http.StatusConflict, map[string]string{"error": i18n.T(r.Context(), "server.msg.mail_operation_cannot_be_undone")})
		return
	}
	if err != nil {
		writeMailOperationsJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(r.Context(), "server.msg.failed_to_undo_mail_operation")})
		return
	}
	writeMailOperationsJSON(w, http.StatusOK, mailOperationResponseFrom(operation))
}

func (h *Handler) handleAdminMailOperationsStatus(w http.ResponseWriter, r *http.Request) {
	scope, err := h.adminWebmailScope(r.Context(), r.URL.Query().Get("user_id"))
	if errors.Is(err, errAdminWebmailScopeNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_webmail_accounts"), http.StatusInternalServerError)
		return
	}
	status, err := h.mailOperationsAdminStatus(r.Context(), scope)
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_get_mail_operation_status"), http.StatusInternalServerError)
		return
	}
	writeMailOperationsJSON(w, http.StatusOK, status)
}

func (h *Handler) handleAdminOperations(w http.ResponseWriter, r *http.Request) {
	scope, err := h.adminWebmailScope(r.Context(), r.URL.Query().Get("user_id"))
	if errors.Is(err, errAdminWebmailScopeNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_webmail_accounts"), http.StatusInternalServerError)
		return
	}
	status, err := h.mailOperationsAdminStatus(r.Context(), scope)
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_get_mail_operation_status"), http.StatusInternalServerError)
		return
	}
	h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
		Section: views.AdminSectionOperations, Scope: status.Scope,
		Content: views.AdminMailOperationsPage(status),
	})
}

func (h *Handler) mailOperationsAdminStatus(ctx context.Context, scope models.AdminWebmailScope) (models.MailOperationsAdminStatus, error) {
	if h.ownedMailbox != nil {
		return h.ownedMailOperationsAdminStatus(ctx, scope)
	}
	var status models.MailOperationsAdminStatus
	var err error
	if scope.SelectedUserID == "" {
		status, err = h.db.ListMailOperationsAdminStatus(ctx)
	} else {
		status, err = h.db.ListMailOperationsAdminStatusForUser(ctx, scope.SelectedUserID)
	}
	if err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	status.Scope = scope
	if err := h.mergeIDLEAdminHealth(ctx, &status.Health, scope.SelectedUserID); err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	if scope.SelectedUserID == "" {
		status.Health.SMTPProfile = h.smtpDeliveryProfile()
		status.Retention = h.mailRetentionDiagnostics()
	}
	return status, nil
}

func (h *Handler) mergeIDLEAdminHealth(ctx context.Context, health *models.MailOperationAdminHealth, userID string) error {
	var configured []storage.ConfiguredIdleFolder
	var err error
	if userID == "" {
		configured, err = h.db.ListConfiguredIdleFolders(ctx)
	} else {
		configured, err = h.db.ListConfiguredIdleFoldersForUser(ctx, userID)
	}
	if err != nil {
		return err
	}
	for _, folder := range configured {
		health.IDLE.Configured++
		if h.syncer == nil {
			health.IDLE.Pending++
			continue
		}
		runtime := h.syncer.IDLEFolderStatuses(folder.AccountID)
		status, ok := runtime[folder.FolderID]
		if !ok || (!status.Healthy && status.Reason == "") {
			health.IDLE.Pending++
		} else if status.Healthy {
			health.IDLE.Healthy++
		} else {
			health.IDLE.Fallback++
		}
	}
	return nil
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
