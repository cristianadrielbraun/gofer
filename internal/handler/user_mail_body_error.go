package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// emailBodyFailure is what the reading pane needs to replace its loader with an
// explanation. The iframe cannot show the error itself because it stays hidden
// until the body reports its height.
type emailBodyFailure struct {
	Type      string `json:"type"`
	EmailID   string `json:"emailId"`
	Kind      string `json:"kind"` // reconnect, missing, large, oversize or unavailable
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
	var tooLarge *message.TooLargeError
	switch {
	case errors.As(err, &tooLarge):
		failure.Kind, failure.Message, status = messageTooLargeFailure(tooLarge)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, sql.ErrNoRows):
		failure.Kind, failure.Message, status = "missing", "This message is no longer available.", http.StatusNotFound
	case isPermanentOAuthError(err):
		log.Printf("routed account needs reconnect: %v", err)
		failure.Kind, status = "reconnect", http.StatusConflict
		failure.Message = i18n.T(ctx, "server.msg.this_account_needs_to_be_reconnected_before")
		failure.Detail = sentence(oauthReconnectReason(err))
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
		log.Printf("message %s body unavailable: %v", emailID, err)
		failure.Detail = err.Error()
	}
	failure.Message, failure.Detail = serverText(ctx, failure.Message), serverText(ctx, failure.Detail)
	payload, _ := json.Marshal(failure) // HTML-escaped, safe inside <script>.
	nonce := setEmailBodyDocumentHeaders(w)
	if failure.Kind == "reconnect" {
		w.Header().Set(accountReconnectHeader, "true")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body><script nonce="` + nonce + `">parent.postMessage(` + string(payload) + `,'*')</script></body></html>`))
}

// messageTooLargeFailure explains a message that wasn't downloaded because of
// its size. Kind "large" lets the reader download it anyway.
func messageTooLargeFailure(err *message.TooLargeError) (kind, text string, status int) {
	if err.CanDownloadFully() {
		if err.Size > 0 {
			return "large", "This message is " + formatMessageSize(err.Size) + ", so it wasn't downloaded automatically.", http.StatusRequestEntityTooLarge
		}
		return "large", "This message is larger than " + formatMessageSize(err.Limit) + ", so it wasn't downloaded automatically.", http.StatusRequestEntityTooLarge
	}
	if err.Size > 0 {
		return "oversize", "This message is " + formatMessageSize(err.Size) + ". Gofer can show messages up to " + formatMessageSize(message.MaxDownloadBytes) + ".", http.StatusRequestEntityTooLarge
	}
	return "oversize", "This message is larger than the " + formatMessageSize(message.MaxDownloadBytes) + " Gofer can show.", http.StatusRequestEntityTooLarge
}

func formatMessageSize(size int64) string {
	return strconv.FormatInt((size+(1<<20)-1)>>20, 10) + " MB"
}
