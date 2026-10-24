package views

import (
	"bytes"
	"context"
	"os"
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

func TestSettingsAdvancedTabIncludesEmailLinkRegistration(t *testing.T) {
	var out bytes.Buffer
	if err := SettingsAdvancedTab(nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAdvancedTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Email links",
		`data-mailto-handler-button`,
		`data-mailto-handler-test-button`,
		`data-mailto-handler-status`,
		"Test email link",
		"Use Gofer",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered advanced settings missing %q: %s", want, html)
		}
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
				`id="test-account-` + tt.account.ID + `"`,
				`data-account-test-dialog-open`,
				`hx-post="/api/accounts/` + tt.account.ID + `/test"`,
				`hx-swap="none"`,
				`data-account-test-service`,
				`data-account-test-icon="testing"`,
				`data-account-test-icon="success"`,
				`data-account-test-retry`,
				`data-account-test-identity`,
				tt.account.Name,
				tt.account.Email,
				"Test Account Connection",
				"Test Again",
				"Account actions",
			} {
				if !strings.Contains(html, want) {
					t.Errorf("account card missing %q", want)
				}
			}
			if got := strings.Count(html, `data-account-primary-action=`); got != 2 {
				t.Errorf("primary action count = %d, want 2", got)
			}
			if got := strings.Count(html, `account-action-btn`); got != 2 {
				t.Errorf("account action style count = %d, want 2", got)
			}
			if strings.Contains(html, `data-test-btn`) {
				t.Errorf("account card should not render the removed inline test-button animation hook")
			}
			if strings.Contains(html, `data-account-test-summary`) {
				t.Errorf("account test dialog should render only provider service cards: %s", html)
			}
			if strings.Contains(html, "Gofer will check the configured mail services") {
				t.Errorf("account test dialog should not render the removed subtitle: %s", html)
			}
			switch tt.account.Provider {
			case "gmail":
				if got := strings.Count(html, `data-account-test-service=`); got != 1 {
					t.Errorf("Gmail account test service count = %d, want 1", got)
				}
				if !strings.Contains(html, `data-account-test-service="gmail"`) || !strings.Contains(html, "Gmail API") {
					t.Errorf("Gmail account card should render the Gmail API test section: %s", html)
				}
				if strings.Contains(html, `data-account-test-service="imap"`) || strings.Contains(html, `data-account-test-service="smtp"`) {
					t.Errorf("Gmail account card should not render IMAP/SMTP test sections: %s", html)
				}
			case "outlook":
				if got := strings.Count(html, `data-account-test-service=`); got != 1 {
					t.Errorf("Outlook account test service count = %d, want 1", got)
				}
				if !strings.Contains(html, `data-account-test-service="graph"`) {
					t.Errorf("Outlook account card should render the Graph test section: %s", html)
				}
				if strings.Contains(html, `data-account-test-service="imap"`) || strings.Contains(html, `data-account-test-service="smtp"`) {
					t.Errorf("Outlook account card should not render IMAP/SMTP test sections: %s", html)
				}
			default:
				if got := strings.Count(html, `data-account-test-service=`); got != 2 {
					t.Errorf("account test service count = %d, want 2", got)
				}
				for _, service := range []string{"imap", "smtp"} {
					if !strings.Contains(html, `data-account-test-service="`+service+`"`) {
						t.Errorf("account card missing %s test section: %s", service, html)
					}
				}
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

func TestSettingsAccountCardShowsCurrentMailSyncError(t *testing.T) {
	account := models.Account{
		ID:               "acc-error",
		Name:             "Work account",
		Email:            "person@example.com",
		Provider:         "imap",
		EmailSyncEnabled: true,
		EmailSyncError:   "authentication failed",
		EmailSyncErrorAt: "2026-07-16T08:30:00Z",
	}

	var out bytes.Buffer
	if err := SettingsAccountCard(account).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAccountCard.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-settings-account-sync-error="acc-error"`,
		"Mail sync failed",
		`aria-label="Show mail sync error for Work account"`,
		`data-tui-popover-type="click"`,
		`data-tui-popover-placement="bottom-start"`,
		`data-settings-account-sync-error-message`,
		"authentication failed",
		`data-account-sync-error-at="2026-07-16T08:30:00Z"`,
		"Last failure:",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("account card sync error missing %q: %s", want, html)
		}
	}

	marker := `data-settings-account-sync-error="acc-error"`
	start := strings.Index(html, marker)
	if start == -1 {
		t.Fatalf("account card sync error markup not found: %s", html)
	}
	end := strings.Index(html[start:], `data-tui-popover-root`)
	if end == -1 {
		t.Fatalf("account card sync error popover not found: %s", html)
	}
	if strings.Contains(html[start:start+end], " hidden") {
		t.Fatalf("current account sync error should be visible: %s", html[start:start+end])
	}
}

func TestSettingsAccountCardHidesMailSyncErrorWithoutCurrentIssue(t *testing.T) {
	account := models.Account{
		ID:               "acc-healthy",
		Name:             "Healthy account",
		Email:            "healthy@example.com",
		Provider:         "imap",
		EmailSyncEnabled: true,
	}

	var out bytes.Buffer
	if err := SettingsAccountCard(account).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAccountCard.Render() error = %v", err)
	}
	html := out.String()
	marker := `data-settings-account-sync-error="acc-healthy"`
	start := strings.Index(html, marker)
	if start == -1 {
		t.Fatalf("account card sync error live region not found: %s", html)
	}
	end := strings.Index(html[start:], `data-tui-popover-root`)
	if end == -1 {
		t.Fatalf("account card sync error popover region not found: %s", html)
	}
	if !strings.Contains(html[start:start+end], " hidden") {
		t.Fatalf("healthy account sync error should be hidden: %s", html[start:start+end])
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
		"Outlook mail is read through Microsoft Graph delta sync, and messages and drafts are sent through Microsoft Graph.",
		"data-wizard-skip-sending",
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
		"Outlook Sending",
		`data-account-wizard-step="2"`,
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

func TestAccountDialogsIncludeCalendarServiceSection(t *testing.T) {
	var addOut bytes.Buffer
	if err := AddAccountDialog().Render(context.Background(), &addOut); err != nil {
		t.Fatalf("AddAccountDialog.Render() error = %v", err)
	}
	addCalendarButton := renderedWizardButtonForStep(addOut.String(), `data-account-wizard-target-step="5"`)
	if addCalendarButton == "" {
		t.Fatalf("add account dialog did not render a Calendar navigation button")
	}
	// Contacts and calendar both wait for the mail account, and both start off.
	if !strings.Contains(addCalendarButton, " disabled") {
		t.Fatalf("add account Calendar navigation button must wait for the mail account: %s", addCalendarButton)
	}
	if addContactsButton := renderedWizardButtonForStep(addOut.String(), `data-account-wizard-target-step="4"`); !strings.Contains(addContactsButton, " disabled") {
		t.Fatalf("add account Contacts navigation button must wait for the mail account: %s", addContactsButton)
	}
	for _, service := range []string{"contacts", "calendar"} {
		if !strings.Contains(addOut.String(), `data-wizard-service-initial-enabled="false" data-wizard-service-switch="`+service+`"`) &&
			!strings.Contains(addOut.String(), `data-wizard-service-switch="`+service+`" data-wizard-service-initial-enabled="false"`) {
			t.Fatalf("add account %s switch must start off", service)
		}
	}
	for _, want := range []string{
		"Calendar",
		"Sync calendars",
		`data-wizard-service-switch="calendar"`,
		`data-wizard-step="4"`,
		`data-wizard-step="5"`,
		`add-contacts-content`,
		`add-calendar-content`,
	} {
		if !strings.Contains(addOut.String(), want) {
			t.Fatalf("add account dialog missing Calendar service marker %q: %s", want, addOut.String())
		}
	}

	var postCreateOut bytes.Buffer
	if err := AddAccountPostCreateStep(models.EditAccountData{AccountID: "created-account"}).Render(context.Background(), &postCreateOut); err != nil {
		t.Fatalf("AddAccountPostCreateStep.Render() error = %v", err)
	}
	for _, want := range []string{`add-wizard-footer-step-4`, `add-wizard-footer-step-5`, `add-calendar-content`, "Contact sync", "Calendar sync"} {
		if !strings.Contains(postCreateOut.String(), want) {
			t.Fatalf("post-create account wizard missing separate Calendar marker %q: %s", want, postCreateOut.String())
		}
	}

	var editOut bytes.Buffer
	if err := EditAccountDialog(models.EditAccountData{
		AccountID:    "calendar-account",
		Provider:     "outlook",
		EmailAddress: "calendar@outlook.com",
	}).Render(context.Background(), &editOut); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}
	editCalendarButton := renderedWizardButtonForStep(editOut.String(), `data-account-wizard-target-step="4"`)
	if editCalendarButton == "" {
		t.Fatalf("edit account dialog did not render a Calendar navigation button")
	}
	if strings.Contains(editCalendarButton, " disabled") {
		t.Fatalf("edit account Calendar navigation button must remain selectable: %s", editCalendarButton)
	}
	editCalendarSwitch := renderedWizardSwitchForService(editOut.String(), "calendar")
	if editCalendarSwitch == "" {
		t.Fatalf("edit account dialog did not render a Calendar switch")
	}
	if strings.Contains(editCalendarSwitch, "disabled") {
		t.Fatalf("edit account Calendar switch must remain interactive before discovery: %s", editCalendarSwitch)
	}
	if strings.Contains(editCalendarSwitch, `hx-post="/api/accounts/calendar-account/services"`) {
		t.Fatalf("Calendar switch should be saved by its Next action, not immediately on change: %s", editCalendarSwitch)
	}
	for _, want := range []string{
		"Calendar sync",
		"Microsoft Calendar access",
		"Discover calendars",
		`/api/accounts/calendar-account/calendar/discover`,
		`data-wizard-service-switch="calendar"`,
		`data-on-htmx-before-request='["handleEditAccountSaveStart","$event"]'`,
		`data-on-htmx-after-request='["handleEditAccountSaveResult","$event"]'`,
		`hx-target="#edit-account-save-status"`,
		`data-wizard-step="4"`,
		`data-wizard-step="5"`,
		`data-wizard-footer-step="5"`,
		`data-on-click='["editWizardGo",4]'`,
	} {
		if !strings.Contains(editOut.String(), want) {
			t.Fatalf("edit account dialog missing Calendar marker %q: %s", want, editOut.String())
		}
	}
	for name, html := range map[string]string{"add": addOut.String() + postCreateOut.String(), "edit": editOut.String()} {
		for _, want := range []string{"Next", "wizardServiceNext"} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s account dialog missing wizard navigation marker %q: %s", name, want, html)
			}
		}
		for _, forbidden := range []string{"Setup Contacts", "Save and continue"} {
			if strings.Contains(html, forbidden) {
				t.Fatalf("%s account dialog still renders obsolete wizard action %q: %s", name, forbidden, html)
			}
		}
	}
	if !strings.Contains(postCreateOut.String(), `data-wizard-contact-form`) {
		t.Fatalf("post-create account wizard is missing the contact Next form")
	}
	// Editing ends its mail steps with 1.d, which tests before saving; an
	// account without typed mail settings tests its saved connection there.
	for _, want := range []string{"Test and save", `id="edit-account-test-results"`, `data-test-url="/api/accounts/calendar-account/test?wizard=check"`, `data-test-typed="false"`, `data-account-test-anyway`, `data-account-test-retest`, `data-account-test-save`, `data-account-test-close`, "Continue to Contacts", `data-on-click='["wizardServiceNext","$this","calendar","done"]'`} {
		if !strings.Contains(editOut.String(), want) {
			t.Fatalf("edit account wizard missing test-before-save marker %q: %s", want, editOut.String())
		}
	}
	for _, gone := range []string{"Finish", "Test results", `data-wizard-test-results`} {
		if strings.Contains(editOut.String(), gone) {
			t.Fatalf("edit account wizard still has the old Finish step (%q): %s", gone, editOut.String())
		}
	}
	var typedOut bytes.Buffer
	if err := EditAccountDialog(models.EditAccountData{AccountID: "password-account", Provider: "imap", AuthMethod: "plain", EmailAddress: "ana@example.test"}).Render(context.Background(), &typedOut); err != nil {
		t.Fatalf("EditAccountDialog.Render() for a password account error = %v", err)
	}
	for _, want := range []string{`data-test-url="/api/accounts/password-account/test-changes"`, `data-test-typed="true"`} {
		if !strings.Contains(typedOut.String(), want) {
			t.Fatalf("password account edit wizard should test the typed settings (%q): %s", want, typedOut.String())
		}
	}
	// Adding tests the typed settings on their own Email step (1.d), before the
	// account exists, so the steps shown after creating it don't test again.
	for _, want := range []string{`data-add-account-test-results`, "Test and create", `data-account-test-anyway`, `data-account-test-retest`, `data-account-test-save`, `data-account-test-close`, "Continue to Contacts"} {
		if !strings.Contains(addOut.String(), want) {
			t.Fatalf("add account dialog missing test-before-create marker %q: %s", want, addOut.String())
		}
	}
	for _, gone := range []string{"Finish", "Ready to use"} {
		if strings.Contains(addOut.String(), gone) {
			t.Fatalf("add account dialog still has a Finish step (%q): %s", gone, addOut.String())
		}
	}
	for _, want := range []string{`data-add-account-created`, `data-on-click='["wizardServiceNext","$this","calendar","done"]'`} {
		if !strings.Contains(postCreateOut.String(), want) {
			t.Fatalf("post-create account wizard missing %q: %s", want, postCreateOut.String())
		}
	}
	if strings.Contains(postCreateOut.String(), `data-wizard-test-results`) {
		t.Fatalf("post-create account wizard tests the connection again: %s", postCreateOut.String())
	}

	var gmailOut bytes.Buffer
	if err := EditAccountDialog(models.EditAccountData{
		AccountID:    "gmail-calendar-account",
		Provider:     "gmail",
		EmailAddress: "calendar@gmail.com",
		DisplayName:  "Calendar Gmail",
	}).Render(context.Background(), &gmailOut); err != nil {
		t.Fatalf("EditAccountDialog.Render() for Gmail error = %v", err)
	}
	for _, want := range []string{
		"Google Calendar access",
		"Discover calendars",
		`/api/accounts/gmail-calendar-account/calendar/discover`,
		`data-calendar-discovery-form`,
		`data-calendar-discovery-label`,
		`["handleCalendarDiscoveryStart","$event"]`,
		`["handleCalendarDiscoveryResult","$event"]`,
		`data-wizard-service-always-active="true"`,
	} {
		if !strings.Contains(gmailOut.String(), want) {
			t.Fatalf("Gmail edit dialog missing Calendar setup marker %q: %s", want, gmailOut.String())
		}
	}
	if strings.Contains(gmailOut.String(), "Reconnect Google") {
		t.Fatal("Gmail Calendar setup should use the account actions menu for reconnecting")
	}
}

func TestCalendarSyncSettingsRenderSelectableSources(t *testing.T) {
	var out bytes.Buffer
	if err := CalendarSyncSettings(models.EditAccountData{
		AccountID:    "calendar-account",
		Provider:     "outlook",
		EmailAddress: "calendar@outlook.com",
		CalendarSync: models.CalendarSyncConfig{
			Enabled:             true,
			SourceCount:         2,
			SelectedSourceCount: 1,
			Sources: []models.CalendarSource{
				{ID: "primary-source", Name: "Primary", TimeZone: "Europe/Prague", AccessRole: "owner", IsPrimary: true, IsSelected: true, Color: "#4285f4"},
				{ID: "team-source", Name: "Team", AccessRole: "reader", IsSelected: false, Color: "#34a853"},
			},
		},
	}).Render(context.Background(), &out); err != nil {
		t.Fatalf("CalendarSyncSettings.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Selected calendars",
		"Primary",
		"Team",
		"Europe/Prague · Owner",
		"Read only",
		"1 of 2 selected",
		`data-calendar-source-form`,
		`name="source_id"`,
		`/api/accounts/calendar-account/calendar/sources`,
		"Your choices are saved when you click Next.",
		"max-h-56",
		"overflow-y-auto",
		"p-3 text-xs",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("calendar source selector missing %q: %s", want, html)
		}
	}
}

// A JMAP account chooses between JMAP and CalDAV calendars, starting on JMAP
// unless its calendars already come from CalDAV; an IMAP account has only
// CalDAV.
func TestCalendarSyncSettingsOfferJMAPToJMAPAccounts(t *testing.T) {
	render := func(data models.EditAccountData) string {
		t.Helper()
		var out bytes.Buffer
		if err := CalendarSyncSettings(data).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	fresh := render(models.EditAccountData{AccountID: "a1", Provider: "jmap", EmailAddress: "lena@example.test"})
	for _, want := range []string{`name="calendar_protocol" value="jmap"`, "Sync calendars over", `value="jmap" checked`, `data-calendar-protocol="caldav"`, "Calendars come from the same JMAP server"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("new JMAP account form missing %q", want)
		}
	}
	onCalDAV := render(models.EditAccountData{AccountID: "a1", Provider: "jmap", EmailAddress: "lena@example.test",
		CalendarSync: models.CalendarSyncConfig{Protocol: "caldav", CalDAVBaseURL: "https://dav.example.test/", SourceCount: 1, Sources: []models.CalendarSource{{ID: "s1", Name: "Home"}}}})
	if !strings.Contains(onCalDAV, `name="calendar_protocol" value="caldav"`) || !strings.Contains(onCalDAV, `value="caldav" checked`) {
		t.Errorf("JMAP account on CalDAV doesn't start on CalDAV")
	}
	imap := render(models.EditAccountData{AccountID: "a2", Provider: "imap", EmailAddress: "sam@example.test"})
	if strings.Contains(imap, "calendar_protocol") || !strings.Contains(imap, `name="caldav_url"`) {
		t.Errorf("IMAP account form offers JMAP or lacks CalDAV")
	}
}

func TestConnectionTestResultsUseProviderLayoutAndStatusColors(t *testing.T) {
	results := []models.ConnectionTestResult{{Service: "gmail", Message: "Gmail API mail access successful", Success: true}}
	var successOut bytes.Buffer
	if err := ConnectionTestResults(results, "gmail-account", "edit").Render(context.Background(), &successOut); err != nil {
		t.Fatalf("ConnectionTestResults() success render error = %v", err)
	}
	successHTML := successOut.String()
	for _, want := range []string{
		`class="grid grid-cols-1 gap-3"`,
		"bg-emerald-500/10",
		"text-emerald-700",
		"Gmail API mail access successful",
	} {
		if !strings.Contains(successHTML, want) {
			t.Fatalf("successful provider result missing %q: %s", want, successHTML)
		}
	}
	if strings.Contains(successHTML, "sm:grid-cols-2") {
		t.Fatalf("single provider result should use the full-width layout: %s", successHTML)
	}

	var failureOut bytes.Buffer
	if err := ConnectionTestResults([]models.ConnectionTestResult{{Service: "graph", Error: "token expired", Message: "Microsoft Graph mail access"}}, "outlook-account", "edit").Render(context.Background(), &failureOut); err != nil {
		t.Fatalf("ConnectionTestResults() failure render error = %v", err)
	}
	failureHTML := failureOut.String()
	for _, want := range []string{"bg-destructive/10", "text-destructive", "token expired"} {
		if !strings.Contains(failureHTML, want) {
			t.Fatalf("failed provider result missing %q: %s", want, failureHTML)
		}
	}
}

func renderedWizardButtonForStep(html, marker string) string {
	markerIndex := strings.Index(html, marker)
	if markerIndex < 0 {
		return ""
	}
	start := strings.LastIndex(html[:markerIndex], "<button")
	if start < 0 {
		return ""
	}
	endOffset := strings.Index(html[markerIndex:], "</button>")
	if endOffset < 0 {
		return ""
	}
	return html[start : markerIndex+endOffset+len("</button>")]
}

func renderedWizardSwitchForService(html, service string) string {
	marker := `data-wizard-service-switch="` + service + `"`
	markerIndex := strings.Index(html, marker)
	if markerIndex < 0 {
		return ""
	}
	start := strings.LastIndex(html[:markerIndex], "<input")
	if start < 0 {
		return ""
	}
	endOffset := strings.Index(html[markerIndex:], ">")
	if endOffset < 0 {
		return ""
	}
	return html[start : markerIndex+endOffset+1]
}

func TestAccountDiscoveryRequiresExplicitCandidateSelection(t *testing.T) {
	// The add account dialog's script is in settings-page.js.
	data, err := os.ReadFile("../../assets/js/settings-page.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if strings.Contains(script, "applyMailDiscoveryCandidate(0);") {
		t.Fatal("mail discovery still automatically applies the first candidate")
	}
	if !strings.Contains(script, "Choose a configuration to apply") {
		t.Fatal("mail discovery does not ask the user to choose a candidate")
	}
}

func TestThemeClassKeepsExpressLight(t *testing.T) {
	for _, tc := range []struct {
		settings map[string]string
		want     string
	}{
		{map[string]string{"theme": "dark", "theme_style": "classic"}, "dark"},
		{map[string]string{"theme": "dark", "theme_style": "express"}, ""},
		{map[string]string{"theme": "light", "theme_style": "express"}, ""},
		{map[string]string{"theme_style": "minimal"}, "dark"},
	} {
		if got := themeClass(tc.settings); got != tc.want {
			t.Errorf("themeClass(%v) = %q, want %q", tc.settings, got, tc.want)
		}
	}
}

func TestOutlookPushControlStates(t *testing.T) {
	render := func(push models.OutlookPushSettings) string {
		t.Helper()
		var out bytes.Buffer
		if err := OutlookPushControl(push).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	off := render(models.OutlookPushSettings{AccountID: "acc", Enabled: true})
	if !strings.Contains(off, `id="outlook-push-acc-notifications"`) || !strings.Contains(off, "disabled") || !strings.Contains(off, "GOFER_OUTLOOK_WEBHOOKS=true") || strings.Contains(off, "Test connection") {
		t.Fatalf("control with notifications off on the server = %s", off)
	}
	if !strings.Contains(off, "data-sync-autosave-ignore") {
		t.Fatal("the sync form would save the control's changes")
	}
	managed := render(models.OutlookPushSettings{AccountID: "acc", Enabled: true, Managed: true})
	if strings.Contains(managed, "GOFER_OUTLOOK_WEBHOOKS") || !strings.Contains(managed, "administrator") {
		t.Fatalf("managed control names server settings = %s", managed)
	}

	subscribed := render(models.OutlookPushSettings{AccountID: "acc", Enabled: true, Available: true, Subscribed: true, Expires: time.Now().Add(70 * time.Hour)})
	if !strings.Contains(subscribed, "Working") || strings.Contains(subscribed, "days") || !strings.Contains(subscribed, `hx-post="/api/settings/accounts/acc/outlook-push/check"`) {
		t.Fatalf("subscribed control = %s", subscribed)
	}
	refused := render(models.OutlookPushSettings{AccountID: "acc", Enabled: true, Available: true, Checked: true, CheckError: "Subscription validation request failed."})
	if !strings.Contains(refused, "Microsoft says: Subscription validation request failed.") {
		t.Fatalf("refused check = %s", refused)
	}
	failing := render(models.OutlookPushSettings{AccountID: "acc", Enabled: true, Available: true, Error: "Forbidden", RetryAt: time.Now().Add(10 * time.Minute)})
	if !strings.Contains(failing, "Not working") || !strings.Contains(failing, "Microsoft says: Forbidden") || strings.Contains(failing, "minutes") {
		t.Fatalf("failing subscription = %s", failing)
	}
	checks := render(models.OutlookPushSettings{AccountID: "acc", Available: true})
	if !strings.Contains(checks, "invisible") || strings.Contains(checks, "Working") || !strings.Contains(checks, `id="outlook-push-acc-checks"`) {
		t.Fatalf("control with scheduled checks = %s", checks)
	}
}

// An account card can be moved by its handle or its menu, except while the
// account is being deleted.
func TestSettingsAccountCardCanBeReordered(t *testing.T) {
	account := models.Account{ID: "acc-home", Name: "Home", Email: "home@example.com", Provider: "imap"}
	var out bytes.Buffer
	if err := SettingsAccountCard(account).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAccountCard.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-account-order-handle`,
		`aria-label="Reorder Home. Drag, or use the arrow keys"`,
		`data-account-move="up"`,
		`data-account-move="down"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("account card lacks %q:\n%s", want, html)
		}
	}

	account.IsDeleting = true
	out.Reset()
	if err := SettingsAccountCard(account).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAccountCard.Render() error = %v", err)
	}
	if html := out.String(); strings.Contains(html, `data-account-order-handle`) || strings.Contains(html, `data-account-move=`) {
		t.Fatalf("account being deleted can be moved:\n%s", html)
	}
}

// The Appearance page offers how message bodies look, showing the saved
// choice and following the theme until one is made.
func TestSettingsAppearanceShowsTheMessageStyle(t *testing.T) {
	for _, tt := range []struct{ saved, label, selected string }{
		{"", "Follow theme", "theme"},
		{"original", "Original", "original"},
		{"sepia", "Follow theme", "theme"},
	} {
		var out bytes.Buffer
		if err := SettingsAppearanceTab(map[string]string{"email_body_style": tt.saved}).Render(context.Background(), &out); err != nil {
			t.Fatalf("SettingsAppearanceTab.Render() error = %v", err)
		}
		html := out.String()
		picker := html[strings.Index(html, "data-email-body-style-picker"):]
		for _, want := range []string{`name="email_body_style"`, tt.label, `data-tui-selectbox-value="` + tt.selected + `" data-tui-selectbox-selected="true"`} {
			if !strings.Contains(picker, want) {
				t.Fatalf("saved %q: message style picker lacks %q:\n%.1500s", tt.saved, want, picker)
			}
		}
	}
}

// Mail settings offer grouping mail by conversation, on until turned off,
// and the thread order only matters while it is on.
func TestSettingsMailOffersConversationGrouping(t *testing.T) {
	for _, tt := range []struct {
		saved   string
		grouped bool
	}{{"", true}, {"true", true}, {"false", false}} {
		var out bytes.Buffer
		if err := SettingsComposeDisplayTab(map[string]string{"group_conversations": tt.saved}, nil).Render(context.Background(), &out); err != nil {
			t.Fatalf("SettingsComposeDisplayTab.Render() error = %v", err)
		}
		html := out.String()
		start := strings.Index(html, `id="group-conversations"`)
		if start < 0 {
			t.Fatalf("saved %q: no conversation grouping switch", tt.saved)
		}
		toggle := html[strings.LastIndex(html[:start], "<input"):]
		toggle = toggle[:strings.Index(toggle, ">")]
		if strings.Contains(toggle, "checked") != tt.grouped {
			t.Fatalf("saved %q: switch %s, want checked %v", tt.saved, toggle, tt.grouped)
		}
		row := html[strings.LastIndex(html[:strings.Index(html, `data-setting-depends-on="group-conversations"`)], "<div"):]
		row = row[:strings.Index(row, ">")]
		if faded := strings.Contains(row, "opacity-50"); faded == tt.grouped {
			t.Fatalf("saved %q: thread order row %s faded %v, want %v", tt.saved, row, faded, !tt.grouped)
		}
	}
}
