package views

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSettingsSyncTabIncludesUnifiedFoldersPanel(t *testing.T) {
	var out bytes.Buffer
	if err := SettingsSyncTab(models.SyncSettings{SyncIntervalMinutes: 5}, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Sync settings",
		"Unified folders",
		`name="unified_folders_enabled"`,
		`name="unified_folder_inbox_enabled"`,
		`name="unified_folder_starred_enabled"`,
		`name="unified_folder_spam_enabled"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered sync tab missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "Spam and junk folders across accounts.") {
		t.Fatalf("unified folder rows should not render descriptions: %s", html)
	}
	if strings.Contains(html, `name="unified_folder_scheduled_enabled"`) {
		t.Fatalf("scheduled should not render as a unified folder setting: %s", html)
	}
}

func TestSettingsAccountCardKeepsPrimaryActionsVisibleAndMovesSecondaryActionsToMenu(t *testing.T) {
	tests := []struct {
		name             string
		account          models.Account
		secondaryActions []string
	}{
		{
			name: "gmail",
			account: models.Account{
				ID:               "acc-gmail",
				Name:             "Personal Gmail",
				Email:            "person@gmail.com",
				Provider:         "gmail",
				EmailSyncEnabled: true,
			},
			secondaryActions: []string{"reconnect", "repair", "delete"},
		},
		{
			name: "outlook",
			account: models.Account{
				ID:       "acc-outlook",
				Name:     "Work Outlook",
				Email:    "person@outlook.com",
				Provider: "outlook",
			},
			secondaryActions: []string{"reconnect", "delete"},
		},
		{
			name: "imap",
			account: models.Account{
				ID:       "acc-imap",
				Name:     "Other mail",
				Email:    "person@example.com",
				Provider: "imap",
			},
			secondaryActions: []string{"delete"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := SettingsAccountCard(tt.account).Render(context.Background(), &out); err != nil {
				t.Fatalf("SettingsAccountCard.Render() error = %v", err)
			}
			html := out.String()
			for _, want := range []string{
				`data-account-primary-action="edit"`,
				`data-account-primary-action="test"`,
				`id="account-actions-menu-` + tt.account.ID + `"`,
				`aria-label="More actions for ` + tt.account.Name + `"`,
				`data-account-actions-menu="` + tt.account.ID + `"`,
				"Account actions",
			} {
				if !strings.Contains(html, want) {
					t.Errorf("account card missing %q", want)
				}
			}
			if got := strings.Count(html, `data-account-primary-action=`); got != 2 {
				t.Errorf("primary action count = %d, want 2", got)
			}
			for _, action := range tt.secondaryActions {
				if !strings.Contains(html, `data-account-secondary-action="`+action+`"`) {
					t.Errorf("account actions menu missing %q", action)
				}
			}
			if got := strings.Count(html, `data-account-secondary-action=`); got != len(tt.secondaryActions) {
				t.Errorf("secondary action count = %d, want %d", got, len(tt.secondaryActions))
			}
		})
	}
}

func TestSettingsSyncTabRendersUnifiedFolderAccountSwitches(t *testing.T) {
	settings := models.SyncSettings{
		SyncIntervalMinutes: 5,
		Accounts: []models.AccountSyncStatus{
			{AccountID: "acc-a", AccountName: "Primary", AccountEmail: "primary@example.com", Color: "#d2802d"},
			{AccountID: "acc-b", AccountName: "Archive", AccountEmail: "archive@example.com", Color: "#4f8f6b"},
		},
	}
	uiSettings := map[string]string{
		"unified_folder_inbox_account_acc-b_enabled": "false",
	}

	var out bytes.Buffer
	if err := SettingsSyncTab(settings, uiSettings).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`aria-label="Choose accounts for Inbox"`,
		`name="unified_folder_inbox_account_acc-a_enabled"`,
		`name="unified_folder_inbox_account_acc-b_enabled"`,
		`data-unified-folder-account-switch="inbox"`,
		"primary@example.com",
		"archive@example.com",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered unified folder account controls missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, `name="unified_folder_scheduled_account_acc-a_enabled"`) {
		t.Fatalf("scheduled should not render account-level unified settings: %s", html)
	}
}

func TestSettingsSyncTabRendersFolderRemotePaths(t *testing.T) {
	settings := models.SyncSettings{
		SyncIntervalMinutes: 1,
		Accounts: []models.AccountSyncStatus{
			{
				AccountID:    "acc-gmail",
				AccountName:  "Gmail",
				AccountEmail: "user@gmail.com",
				Folders: []models.FolderSyncStatus{
					{ID: "acc_gmail_sent", Name: "sent", RemoteID: "[Gmail]/Sent Mail", Role: "sent", Icon: "send", IsIDLE: true},
					{ID: "acc_sent", Name: "sent", RemoteID: "Sent", Role: "sent", Icon: "send", IsIDLE: false},
				},
			},
		},
	}

	var out bytes.Buffer
	if err := SettingsSyncTab(settings, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-folder-id="acc_gmail_sent"`,
		`title="[Gmail]/Sent Mail"`,
		"[Gmail]/Sent Mail",
		`data-folder-id="acc_sent"`,
		">Sent</span>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered sync folder paths missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, ">sent ") {
		t.Fatalf("rendered sync folder should prefer remote path over role/name label: %s", html)
	}
}

func TestSettingsSyncTabShowsConfiguredIDLEFolderInPollingDuringFallback(t *testing.T) {
	settings := models.SyncSettings{
		SyncIntervalMinutes: 5,
		Accounts: []models.AccountSyncStatus{{
			AccountID:    "acc-imap",
			AccountName:  "IMAP",
			AccountEmail: "user@example.com",
			Provider:     "imap",
			Folders: []models.FolderSyncStatus{{
				ID:                 "acc_inbox",
				Name:               "INBOX",
				RemoteID:           "INBOX",
				Role:               "inbox",
				Icon:               "inbox",
				IsIDLE:             true,
				EffectiveIDLE:      false,
				IDLEFallbackReason: "the server does not advertise IMAP IDLE",
			}},
		}},
	}

	var out bytes.Buffer
	if err := SettingsSyncTab(settings, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-poll-zone="acc-imap"`,
		`data-folder-id="acc_inbox"`,
		`data-configured-idle="true"`,
		`data-effective-idle="false"`,
		`data-idle-fallback-warning`,
		`aria-label="Show IDLE fallback reason"`,
		"INBOX was moved to polling because the server does not advertise IMAP IDLE.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered fallback folder missing %q: %s", want, html)
		}
	}
	if strings.Index(html, `data-folder-id="acc_inbox"`) < strings.Index(html, `data-poll-zone="acc-imap"`) {
		t.Fatalf("fallback folder rendered outside polling zone: %s", html)
	}
}

func TestSyncFolderPillHidesWarningWithoutFallbackReason(t *testing.T) {
	var out bytes.Buffer
	folder := models.FolderSyncStatus{
		ID:            "acc_inbox",
		Name:          "INBOX",
		RemoteID:      "INBOX",
		Role:          "inbox",
		Icon:          "inbox",
		IsIDLE:        true,
		EffectiveIDLE: true,
	}
	if err := syncFolderPill(folder, 5, "acc-imap").Render(context.Background(), &out); err != nil {
		t.Fatalf("syncFolderPill.Render() error = %v", err)
	}
	html := out.String()
	marker := `data-idle-fallback-warning class="`
	start := strings.Index(html, marker)
	if start == -1 {
		t.Fatalf("rendered pill missing fallback warning wrapper: %s", html)
	}
	start += len(marker)
	end := strings.Index(html[start:], `"`)
	if end == -1 {
		t.Fatalf("rendered pill has malformed fallback warning class: %s", html)
	}
	classes := strings.Fields(html[start : start+end])
	if !slices.Contains(classes, "hidden") || slices.Contains(classes, "inline-flex") {
		t.Fatalf("healthy pill warning classes = %q, want hidden without inline-flex", classes)
	}
}

func TestIdleFallbackTooltipIncludesTemporaryRetry(t *testing.T) {
	text := idleFallbackTooltip(models.FolderSyncStatus{
		Name:               "INBOX",
		RemoteID:           "INBOX",
		IDLEFallbackReason: "the IDLE connection was closed",
		IDLERetryAt:        time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
	})
	if !strings.Contains(text, "INBOX was moved to polling because the IDLE connection was closed.") || !strings.Contains(text, "Gofer will try again in") {
		t.Fatalf("idleFallbackTooltip() = %q", text)
	}
}

func TestSettingsSyncTabRendersGmailAPIInfoInsteadOfFolderModes(t *testing.T) {
	settings := models.SyncSettings{
		SyncIntervalMinutes: 5,
		Accounts: []models.AccountSyncStatus{
			{
				AccountID:    "acc-gmail",
				AccountName:  "Gmail",
				AccountEmail: "user@gmail.com",
				Provider:     "gmail",
				Folders: []models.FolderSyncStatus{
					{ID: "acc_gmail_inbox", Name: "INBOX", RemoteID: "INBOX", Role: "inbox", Icon: "inbox", IsIDLE: true},
				},
			},
		},
	}

	var out bytes.Buffer
	if err := SettingsSyncTab(settings, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Gmail API sync",
		"Gmail sync uses Gmail API history changes instead of per-folder IMAP IDLE.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered Gmail sync card missing %q: %s", want, html)
		}
	}
	for _, notWant := range []string{
		`data-idle-zone="acc-gmail"`,
		`data-poll-zone="acc-gmail"`,
		`data-folder-id="acc_gmail_inbox"`,
		"Real-time note",
		"Each real-time folder holds open a persistent IMAP connection.",
	} {
		if strings.Contains(html, notWant) {
			t.Fatalf("Gmail sync card should not render folder mode control %q: %s", notWant, html)
		}
	}
}

func TestSettingsSyncTabRendersOutlookGraphInfoInsteadOfFolderModes(t *testing.T) {
	settings := models.SyncSettings{
		SyncIntervalMinutes: 5,
		Accounts: []models.AccountSyncStatus{
			{
				AccountID:    "acc-outlook",
				AccountName:  "Outlook",
				AccountEmail: "user@outlook.com",
				Provider:     "outlook",
				Folders: []models.FolderSyncStatus{
					{ID: "acc_outlook_inbox", Name: "Inbox", RemoteID: "Inbox", Role: "inbox", Icon: "inbox", IsIDLE: true},
				},
			},
		},
	}

	var out bytes.Buffer
	if err := SettingsSyncTab(settings, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Outlook Graph sync",
		"Outlook sync uses Microsoft Graph delta changes instead of per-folder IMAP IDLE.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered Outlook sync card missing %q: %s", want, html)
		}
	}
	for _, notWant := range []string{
		`data-idle-zone="acc-outlook"`,
		`data-poll-zone="acc-outlook"`,
		`data-folder-id="acc_outlook_inbox"`,
		"Real-time note",
		"Each real-time folder holds open a persistent IMAP connection.",
	} {
		if strings.Contains(html, notWant) {
			t.Fatalf("Outlook sync card should not render folder mode control %q: %s", notWant, html)
		}
	}
}

func TestEditAccountDialogRendersOutlookGraphMailPanels(t *testing.T) {
	data := models.EditAccountData{
		AccountID:    "acc-outlook",
		Provider:     "outlook",
		EmailAddress: "user@outlook.com",
		DisplayName:  "Outlook",
	}

	var out bytes.Buffer
	if err := EditAccountDialog(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`name="provider" value="outlook"`,
		"Microsoft Graph",
		"Outlook mail is read through Microsoft Graph delta sync.",
		"Outlook messages and drafts are sent through Microsoft Graph.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered Outlook edit dialog missing %q: %s", want, html)
		}
	}
	for _, notWant := range []string{
		`name="imap_host"`,
		`name="smtp_host"`,
		"IMAP Host",
		"SMTP Host",
		"(IMAP)",
		"(SMTP)",
	} {
		if strings.Contains(html, notWant) {
			t.Fatalf("Outlook edit dialog should not render mail transport field %q: %s", notWant, html)
		}
	}
}

func TestAccountDialogsOfferOnlyExplicitMailTransportModes(t *testing.T) {
	var addOut bytes.Buffer
	if err := AddAccountDialog().Render(context.Background(), &addOut); err != nil {
		t.Fatalf("AddAccountDialog.Render() error = %v", err)
	}
	var editOut bytes.Buffer
	if err := EditAccountDialog(models.EditAccountData{
		AccountID:    "acc-imap",
		Provider:     "imap",
		EmailAddress: "user@example.com",
		IMAPTLSMode:  "tls",
		SMTPTLSMode:  "starttls",
	}).Render(context.Background(), &editOut); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}

	for name, html := range map[string]string{"add": addOut.String(), "edit": editOut.String()} {
		if strings.Contains(html, `value="none"`) {
			t.Fatalf("%s account dialog still offers an unencrypted mail transport: %s", name, html)
		}
		for _, want := range []string{`value="tls"`, `value="starttls"`, `value="plaintext"`, "admin exception"} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s account dialog missing transport option %q: %s", name, want, html)
			}
		}
	}
}

func TestAccountDiscoveryRequiresExplicitCandidateSelection(t *testing.T) {
	var out bytes.Buffer
	if err := AddAccountDialog().Render(context.Background(), &out); err != nil {
		t.Fatalf("AddAccountDialog.Render() error = %v", err)
	}
	html := out.String()
	if strings.Contains(html, "applyMailDiscoveryCandidate(0);") {
		t.Fatal("mail discovery still automatically applies the first candidate")
	}
	if !strings.Contains(html, "Choose a configuration to apply") {
		t.Fatal("mail discovery does not ask the user to choose a candidate")
	}
}
