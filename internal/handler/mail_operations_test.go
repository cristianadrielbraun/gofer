package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func mailOperationsRequest(req *http.Request, userID string, isAdmin bool) *http.Request {
	return req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: userID, Email: userID + "@example.com", IsAdmin: isAdmin}))
}

func seedHandlerMailOperation(t *testing.T, h *Handler, db *storage.DB) string {
	t.Helper()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (101, 'victim-account', '<handler-operation@example.com>', 'Private operation', 'sender@example.com');
		INSERT INTO message_mutations (
			id, account_id, message_id, folder_id, provider_type, kind, target_value,
			status, attempt_count, last_error, next_attempt_at
		) VALUES ('handler-mut', 'victim-account', 101, '', 'imap', 'read', 0,
			'failed', 1, 'access_token=handler-secret', CURRENT_TIMESTAMP);`); err != nil {
		t.Fatalf("seed handler operation: %v", err)
	}
	return "message_mutation:handler-mut"
}

func TestMailOperationsHandlersAreUserScoped(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	operationID := seedHandlerMailOperation(t, h, db)

	req := httptest.NewRequest(http.MethodGet, "/api/mail-operations", nil)
	rec := httptest.NewRecorder()
	h.handleMailOperations(rec, mailOperationsRequest(req, "owner", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d body = %q", rec.Code, rec.Body.String())
	}
	var ownerResponse struct {
		Operations []map[string]any `json:"operations"`
		Total      int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&ownerResponse); err != nil {
		t.Fatalf("decode owner operations: %v", err)
	}
	if ownerResponse.Total != 1 || len(ownerResponse.Operations) != 1 || ownerResponse.Operations[0]["id"] != operationID {
		t.Fatalf("owner operations = %#v", ownerResponse)
	}
	if strings.Contains(rec.Body.String(), "handler-secret") {
		t.Fatalf("owner response leaked provider secret: %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/mail-operations", nil)
	rec = httptest.NewRecorder()
	h.handleMailOperations(rec, mailOperationsRequest(req, "attacker", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("attacker status = %d body = %q", rec.Code, rec.Body.String())
	}
	var attackerResponse struct {
		Operations []map[string]any `json:"operations"`
		Total      int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&attackerResponse); err != nil {
		t.Fatalf("decode attacker operations: %v", err)
	}
	if attackerResponse.Total != 0 || len(attackerResponse.Operations) != 0 {
		t.Fatalf("attacker operations = %#v, want empty", attackerResponse)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/mail-operations/"+operationID+"/retry", nil)
	req.SetPathValue("id", operationID)
	rec = httptest.NewRecorder()
	h.handleRetryMailOperation(rec, mailOperationsRequest(req, "attacker", false))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attacker retry status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/mail-operations/"+operationID+"/retry", nil)
	req.SetPathValue("id", operationID)
	rec = httptest.NewRecorder()
	h.handleRetryMailOperation(rec, mailOperationsRequest(req, "owner", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner retry status = %d body = %q", rec.Code, rec.Body.String())
	}
	var retryResponse map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&retryResponse); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryResponse["state"] != storage.MessageMutationPending {
		t.Fatalf("retry response = %#v", retryResponse)
	}
}

func TestMailOperationsAdminRoutesAreRegisteredAndProtected(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	_ = seedHandlerMailOperation(t, h, db)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/mail-operations/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mailOperationsRequest(req, "owner", false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d body = %q, want 403", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/mail-operations/status", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, mailOperationsRequest(req, "admin", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %q", rec.Code, rec.Body.String())
	}
	var status map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode admin status: %v", err)
	}
	if status["total"] != float64(1) || status["action_required"] != float64(1) {
		t.Fatalf("admin status = %#v", status)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/operations/", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, mailOperationsRequest(req, "admin", true))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Mail operations") {
		t.Fatalf("admin operations page status = %d body prefix = %q", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 200)])
	}
}

func TestMailOperationsAdminStatusIncludesRetentionDiagnostics(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	runAt := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	h.runMailRetentionAt(t.Context(), runAt)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/mail-operations/status", nil)
	rec := httptest.NewRecorder()
	h.handleAdminMailOperationsStatus(rec, mailOperationsRequest(req, "admin", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %q", rec.Code, rec.Body.String())
	}
	var status models.MailOperationsAdminStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode admin status: %v", err)
	}
	if !status.Retention.LastRunAt.Equal(runAt) {
		t.Fatalf("retention last run = %s, want %s", status.Retention.LastRunAt, runAt)
	}
	if status.Retention.LastError != "" {
		t.Fatalf("retention error = %q", status.Retention.LastError)
	}
}
