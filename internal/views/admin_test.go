package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestAdminLabelsPageRendersOutlookGraphDiagnostics(t *testing.T) {
	status := models.LabelAdminStatus{
		Accounts: []models.LabelAccountSyncStatus{{
			AccountID:       "acc_outlook",
			AccountName:     "Outlook",
			AccountEmail:    "user@example.com",
			AccountProvider: "outlook",
			LabelProvider:   "outlook_category",
			TotalMessages:   3,
			OutlookGraph: &models.OutlookGraphDiagnostics{
				GraphBackedMessages:              1,
				IMAPBackedMessages:               2,
				MessageParityDelta:               -1,
				MessagesMissingGraphID:           2,
				MissingGraphIDWithInternetID:     1,
				MissingGraphIDWithoutInternetID:  1,
				MissingGraphIDWithoutGraphFolder: 1,
				LocalFolders:                     2,
				GraphBackedFolders:               1,
				FoldersMissingGraphID:            1,
			},
		}},
	}

	var out bytes.Buffer
	if err := AdminLabelsPage(status).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminLabelsPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"Outlook Graph parity", "Graph IDs", "IMAP rows", "Parity delta", "Needs repair", "Backfillable", "No Graph folder"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin labels page missing %q: %s", want, html)
		}
	}
}
