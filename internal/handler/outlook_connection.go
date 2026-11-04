package handler

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func (h *Handler) testOutlookGraphMail(ctx context.Context, accountID string) []models.ConnectionTestResult {
	result := models.ConnectionTestResult{
		Service: "graph",
		Message: i18n.T(ctx, "server.msg.microsoft_graph_mail"),
	}
	if h.mailCredentials() == nil {
		result.Error = i18n.T(ctx, "server.msg.microsoft_graph_auth_is_not_configured")
		return []models.ConnectionTestResult{result}
	}
	token, err := h.mailCredentials().GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		result.Error = serverText(ctx, err.Error())
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
		result.Error = serverText(ctx, err.Error())
		return []models.ConnectionTestResult{result}
	}
	result.Success = true
	result.Message = i18n.T(ctx, "server.msg.microsoft_graph_mail_access_successful")
	return []models.ConnectionTestResult{result}
}

func outlookGraphMailFoldersProbeEndpoint() string {
	values := url.Values{}
	values.Set("$top", "1")
	values.Set("$select", "id,displayName")
	return outlookGraphBaseURL + "/me/mailFolders?" + values.Encode()
}
