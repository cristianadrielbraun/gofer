package handler

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func (h *Handler) testGmailAPIMail(ctx context.Context, accountID string) []models.ConnectionTestResult {
	result := models.ConnectionTestResult{
		Service: "gmail",
		Message: i18n.T(ctx, "server.msg.gmail_api_mail_access"),
	}
	if h.mailCredentials() == nil {
		result.Error = i18n.T(ctx, "server.msg.google_auth_is_not_configured")
		return []models.ConnectionTestResult{result}
	}
	token, err := h.mailCredentials().GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		result.Error = serverText(ctx, err.Error())
		return []models.ConnectionTestResult{result}
	}
	var response struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := runAccountConnectionTest(ctx, accountConnectionTestRetryDelay, func() error {
		return doGoogleJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/profile", token, nil, &response)
	}); err != nil {
		result.Error = serverText(ctx, err.Error())
		return []models.ConnectionTestResult{result}
	}
	result.Success = true
	result.Message = i18n.T(ctx, "server.msg.gmail_api_mail_access_successful")
	return []models.ConnectionTestResult{result}
}
