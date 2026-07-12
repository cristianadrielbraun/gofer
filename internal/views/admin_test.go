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

func TestAdminSecurityPageShowsExceptionsAndWarnings(t *testing.T) {
	data := models.MailSecurityAdminData{Exceptions: []models.MailSecurityException{
		{ID: "http", Kind: models.MailSecurityExceptionHTTPDiscovery, Host: "lab.example.test", CreatedBy: "admin"},
		{ID: "imap", Kind: models.MailSecurityExceptionPlaintextTransport, Protocol: "imap", Host: "mail.test", Port: 1143, CreatedBy: "admin", Accounts: []models.MailSecurityExceptionAccount{{ID: "account", Email: "user@example.com"}}},
		{ID: "private", Kind: models.MailSecurityExceptionPrivateTarget, Protocol: "http", Host: "127.0.0.1", Port: 8080, CreatedBy: "admin"},
	}}

	var out bytes.Buffer
	if err := AdminSecurityPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminSecurityPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"Mail security", "lab.example.test", "IMAP mail.test:1143", "HTTP 127.0.0.1:8080", "user@example.com", "OAuth tokens are never allowed", "Only approve endpoints you control"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin security page missing %q: %s", want, html)
		}
	}
}

func TestAdminMailOperationsPageRendersSMTPBaseline(t *testing.T) {
	status := models.MailOperationsAdminStatus{}
	status.Health.SMTPProfile = models.MailSMTPAdminProfile{
		Samples: 2, Successes: 1, Failures: 1, Connections: 2, Messages: 2,
		AvgConnectAuthMs: 1250, AvgDataMs: 240, AvgTotalMs: 1490, AvgQueueWaitMs: 3200,
	}

	var out bytes.Buffer
	if err := AdminMailOperationsPage(status).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminMailOperationsPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"Queue health", "SMTP baseline", "Process lifetime", "Avg connect + auth", "1.2 s", "240 ms"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin operations page missing %q: %s", want, html)
		}
	}
}
