package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestEditAccountDialogShowsJMAPServerAndLogin(t *testing.T) {
	var out bytes.Buffer
	data := models.EditAccountData{
		AccountID: "acc-jmap", Provider: "jmap", AuthMethod: "plain",
		EmailAddress: "robin@example.test", DisplayName: "Robin", Username: "robin@example.test",
		JMAPSessionURL: "https://mail.example.test/.well-known/jmap", EmailSyncEnabled: true,
	}
	if err := EditAccountDialog(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`name="provider" value="jmap"`,
		`name="jmap_session_url"`,
		`value="https://mail.example.test/.well-known/jmap"`,
		`name="username"`,
		`name="password"`,
		"Server and login",
		"data-wizard-skip-sending",
		`/api/accounts/acc-jmap/test-changes`,
		"Gofer finds your JMAP server and signs in",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("JMAP edit dialog missing %q", want)
		}
	}
	for _, notWant := range []string{
		`name="imap_host"`,
		`name="smtp_host"`,
		`name="smtp_username"`,
		`name="auth_method"`,
		`data-account-wizard-step="2"`,
	} {
		if strings.Contains(html, notWant) {
			t.Fatalf("JMAP edit dialog should not render %q", notWant)
		}
	}
}

func TestJMAPConnectionDialogUsesTheJMAPResult(t *testing.T) {
	var out bytes.Buffer
	if err := SettingsAccountConnectionTestDialog(models.Account{ID: "acc-jmap", Provider: "jmap", Name: "JMAP", Email: "robin@example.test"}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Count(html, `data-account-test-service=`) != 1 || !strings.Contains(html, `data-account-test-service="jmap"`) || !strings.Contains(html, "Checking JMAP mail access...") {
		t.Fatalf("connection dialog does not match the JMAP result: %s", html)
	}
	for _, unwanted := range []string{`data-account-test-service="imap"`, `data-account-test-service="smtp"`} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("JMAP connection dialog contains %s", unwanted)
		}
	}
}

func TestSettingsJMAPSyncUsesAccountWideUpdates(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "JMAP only"
		if mixed {
			name = "JMAP and IMAP"
		}
		t.Run(name, func(t *testing.T) {
			settings := models.SyncSettings{SyncIntervalMinutes: 5, Accounts: []models.AccountSyncStatus{{
				AccountID: "acc-jmap", AccountName: "JMAP", AccountEmail: "robin@example.test", Provider: "jmap",
				Folders: []models.FolderSyncStatus{{ID: "jmap-inbox", Name: "Inbox", Role: "inbox", Icon: "inbox", IsIDLE: true}},
			}}}
			if mixed {
				settings.Accounts = append(settings.Accounts, models.AccountSyncStatus{AccountID: "acc-imap", AccountName: "IMAP", Provider: "imap",
					Folders: []models.FolderSyncStatus{{ID: "imap-inbox", Name: "Inbox", Role: "inbox", Icon: "inbox", IsIDLE: true}},
				})
			}
			var out bytes.Buffer
			if err := SettingsSyncTab(settings, nil).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			for _, want := range []string{"JMAP sync", "JMAP uses real-time updates when the server supports them, with scheduled checks for the whole account."} {
				if !strings.Contains(html, want) {
					t.Fatalf("missing JMAP sync explanation %q", want)
				}
			}
			for _, unwanted := range []string{`data-idle-zone="acc-jmap"`, `data-poll-zone="acc-jmap"`, `data-folder-id="jmap-inbox"`} {
				if strings.Contains(html, unwanted) {
					t.Fatalf("JMAP renders an IMAP control: %s", unwanted)
				}
			}
			if strings.Contains(html, `data-idle-zone="acc-imap"`) != mixed || strings.Contains(html, "Real-time note") != mixed {
				t.Fatal("IMAP folder controls do not match the connected account types")
			}
		})
	}
}

func TestConnectionTestResultsShowWarning(t *testing.T) {
	var out bytes.Buffer
	results := []models.ConnectionTestResult{{Success: true, Service: "jmap", Message: "Connected", Warning: "Sending won't work: this login can't send mail."}}
	if err := ConnectionTestResults(results, "", "add").Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	// The result keeps its success box; the warning gets its own box below.
	success, warning := strings.Index(html, "bg-emerald-500/10"), strings.Index(html, "data-connection-test-warning")
	if success < 0 || warning < success || !strings.Contains(html[warning:], "this login can&#39;t send mail") {
		t.Fatalf("warning not shown below the success box: %s", html)
	}
}
