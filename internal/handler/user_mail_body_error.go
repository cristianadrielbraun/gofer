package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// emailBodyFailure is what the reading pane needs to replace its loader with an
// explanation. The iframe cannot show the error itself because it stays hidden
// until the body reports its height.
type emailBodyFailure struct {
	Type      string `json:"type"`
	EmailID   string `json:"emailId"`
	Kind      string `json:"kind"` // reconnect, missing or unavailable
	Message   string `json:"message"`
	Detail    string `json:"detail,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
}

// writeEmailBodyError answers a body iframe with a page that posts the failure
// to the reading pane. Without it the pane waits for a resize that never comes.
func (h *Handler) writeEmailBodyError(w http.ResponseWriter, ctx context.Context, owner, emailID, accountID string, err error) {
	failure := emailBodyFailure{Type: "emailBodyError", EmailID: emailID, Kind: "unavailable", Message: "This message couldn't be downloaded from the mail server."}
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, sql.ErrNoRows):
		failure.Kind, failure.Message, status = "missing", "This message is no longer available.", http.StatusNotFound
	case isPermanentOAuthError(err):
		log.Printf("routed account needs reconnect: %v", err)
		failure.Kind, status = "reconnect", http.StatusConflict
		failure.Message = "This account needs to be reconnected before this message can be downloaded."
		failure.Detail = "The " + oauthReconnectReason(err) + "."
		failure.AccountID = accountID
		if accountID == "" || h.userAccounts == nil {
			break
		}
		if data, dataErr := h.userEditData(ctx, owner, accountID); dataErr == nil && data != nil &&
			(data.Provider == providers.ProviderGmail || data.Provider == providers.ProviderOutlook) {
			failure.Provider, failure.Email, failure.Name = data.Provider, data.EmailAddress, data.DisplayName
			failure.Message = data.EmailAddress + " needs to be reconnected before this message can be downloaded."
		}
	default:
		log.Printf("routed account operation failed: %v", err)
		failure.Detail = err.Error()
	}
	payload, _ := json.Marshal(failure) // HTML-escaped, safe inside <script>.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if failure.Kind == "reconnect" {
		w.Header().Set(accountReconnectHeader, "true")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body><script>parent.postMessage(` + string(payload) + `,'*')</script></body></html>`))
}
