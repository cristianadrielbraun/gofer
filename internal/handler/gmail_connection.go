package handler

import (
	"context"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func (h *Handler) testGmailAPIMail(ctx context.Context, accountID string) []models.ConnectionTestResult {
	result := models.ConnectionTestResult{
		Service: "gmail",
		Message: "Gmail API mail access",
	}
	if h.auth == nil {
		result.Error = "Google auth is not configured"
		return []models.ConnectionTestResult{result}
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		result.Error = err.Error()
		return []models.ConnectionTestResult{result}
	}
	var response struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := runAccountConnectionTest(ctx, accountConnectionTestRetryDelay, func() error {
		return doGoogleJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/profile", token, nil, &response)
	}); err != nil {
		result.Error = err.Error()
		return []models.ConnectionTestResult{result}
	}
	result.Success = true
	result.Message = "Gmail API mail access successful"
	return []models.ConnectionTestResult{result}
}
