package handler

import (
	"context"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func (h *Handler) testOutlookGraphMail(ctx context.Context, accountID string) []models.ConnectionTestResult {
	result := models.ConnectionTestResult{
		Service: "graph",
		Message: "Microsoft Graph mail",
	}
	if h.auth == nil {
		result.Error = "Microsoft Graph auth is not configured"
		return []models.ConnectionTestResult{result}
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		result.Error = err.Error()
		return []models.ConnectionTestResult{result}
	}
	var response struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := runAccountConnectionTest(ctx, accountConnectionTestRetryDelay, func() error {
		return h.doOutlookJSON(ctx, http.MethodGet, outlookGraphMailFoldersProbeEndpoint(), token, nil, &response)
	}); err != nil {
		result.Error = err.Error()
		return []models.ConnectionTestResult{result}
	}
	result.Success = true
	result.Message = "Microsoft Graph mail access successful"
	return []models.ConnectionTestResult{result}
}

func outlookGraphMailFoldersProbeEndpoint() string {
	values := url.Values{}
	values.Set("$top", "1")
	values.Set("$select", "id,displayName")
	return outlookGraphBaseURL + "/me/mailFolders?" + values.Encode()
}
