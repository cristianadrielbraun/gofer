package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func outgoingStatusRequest(req *http.Request, userID string) *http.Request {
	return req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: userID, Email: userID + "@example.com"}))
}

func TestOutgoingSendStatusIsScopedToCurrentUser(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	queued, err := db.QueueOutgoingSend(t.Context(), storage.QueueOutgoingSendInput{
		ID:                 "owner-send",
		AccountID:          "victim-account",
		DraftID:            "owner-draft",
		Transport:          storage.OutgoingTransportSMTP,
		EnvelopeFrom:       "owner@example.com",
		EnvelopeRecipients: []string{"recipient@example.com"},
		MIMEData:           []byte("Subject: private\r\n\r\nbody"),
		MessageJSON:        []byte(`{"subject":"private"}`),
		SendAfter:          time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/outgoing-sends/active", nil)
	rec := httptest.NewRecorder()
	h.handleOutgoingSendList(rec, outgoingStatusRequest(req, "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d body = %q", rec.Code, rec.Body.String())
	}
	var ownerResponse struct {
		Sends []map[string]any `json:"sends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&ownerResponse); err != nil {
		t.Fatalf("decode owner response: %v", err)
	}
	if len(ownerResponse.Sends) != 1 || ownerResponse.Sends[0]["id"] != queued.ID {
		t.Fatalf("owner response = %#v", ownerResponse)
	}
	if _, exposed := ownerResponse.Sends[0]["mime_data"]; exposed {
		t.Fatal("outgoing status exposed MIME payload")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/outgoing-sends/active", nil)
	rec = httptest.NewRecorder()
	h.handleOutgoingSendList(rec, outgoingStatusRequest(req, "attacker"))
	if rec.Code != http.StatusOK {
		t.Fatalf("attacker list status = %d body = %q", rec.Code, rec.Body.String())
	}
	var attackerResponse struct {
		Sends []map[string]any `json:"sends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&attackerResponse); err != nil {
		t.Fatalf("decode attacker response: %v", err)
	}
	if len(attackerResponse.Sends) != 0 {
		t.Fatalf("attacker response = %#v, want empty", attackerResponse)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/outgoing-sends/owner-send", nil)
	req.SetPathValue("id", queued.ID)
	rec = httptest.NewRecorder()
	h.handleOutgoingSendGet(rec, outgoingStatusRequest(req, "attacker"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attacker detail status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestOutgoingSendStatusActionsHandleAmbiguousRetryAndCancel(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	queued, err := db.QueueOutgoingSend(t.Context(), storage.QueueOutgoingSendInput{
		ID:                 "ambiguous-send",
		AccountID:          "victim-account",
		DraftID:            "ambiguous-draft",
		Transport:          storage.OutgoingTransportSMTP,
		EnvelopeFrom:       "owner@example.com",
		EnvelopeRecipients: []string{"recipient@example.com"},
		MIMEData:           []byte("Subject: ambiguous\r\n\r\nbody"),
		MessageJSON:        []byte(`{"subject":"ambiguous"}`),
		SendAfter:          time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if _, err := db.ClaimDueOutgoingSends(t.Context(), time.Now().Add(time.Second), 1); err != nil {
		t.Fatalf("ClaimDueOutgoingSends() error = %v", err)
	}
	if err := db.FinishOutgoingSendWithError(t.Context(), queued.ID, storage.OutgoingSendAmbiguous, "connection lost"); err != nil {
		t.Fatalf("FinishOutgoingSendWithError() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/outgoing-sends/ambiguous-send/retry", strings.NewReader(`{"confirm":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", queued.ID)
	rec := httptest.NewRecorder()
	h.handleOutgoingSendRetry(rec, outgoingStatusRequest(req, "owner"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("ambiguous retry status = %d body = %q, want 409", rec.Code, rec.Body.String())
	}
	var warning map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&warning); err != nil {
		t.Fatalf("decode ambiguous warning: %v", err)
	}
	if warning["ambiguous_warning_required"] != true {
		t.Fatalf("ambiguous warning = %#v", warning)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/outgoing-sends/ambiguous-send/retry", strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", queued.ID)
	rec = httptest.NewRecorder()
	h.handleOutgoingSendRetry(rec, outgoingStatusRequest(req, "attacker"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign retry status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/outgoing-sends/ambiguous-send/retry", strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", queued.ID)
	rec = httptest.NewRecorder()
	h.handleOutgoingSendRetry(rec, outgoingStatusRequest(req, "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed retry status = %d body = %q", rec.Code, rec.Body.String())
	}
	var retried map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&retried); err != nil {
		t.Fatalf("decode retried response: %v", err)
	}
	if retried["status"] != storage.OutgoingSendPending {
		t.Fatalf("retried response = %#v", retried)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/outgoing-sends/ambiguous-send/cancel", nil)
	req.SetPathValue("id", queued.ID)
	rec = httptest.NewRecorder()
	h.handleOutgoingSendCancel(rec, outgoingStatusRequest(req, "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body = %q", rec.Code, rec.Body.String())
	}
	var canceled map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&canceled); err != nil {
		t.Fatalf("decode canceled response: %v", err)
	}
	if canceled["status"] != storage.OutgoingSendCanceled {
		t.Fatalf("canceled response = %#v", canceled)
	}
}

func TestOutgoingSendStatusRoutesAreRegistered(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/outgoing-sends/active", nil)
	req = outgoingStatusRequest(req, "owner")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("registered active route status = %d body = %q", rec.Code, rec.Body.String())
	}
}
