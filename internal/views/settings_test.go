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

func TestAccountDialogsIncludeCalendarServiceSection(t *testing.T) {
	var addOut bytes.Buffer
	if err := AddAccountDialog().Render(context.Background(), &addOut); err != nil {
		t.Fatalf("AddAccountDialog.Render() error = %v", err)
	}
	addCalendarButton := renderedWizardButtonForStep(addOut.String(), `data-account-wizard-target-step="4"`)
	if addCalendarButton == "" {
		t.Fatalf("add account dialog did not render a Calendar navigation button")
	}
	if strings.Contains(addCalendarButton, " disabled") {
		t.Fatalf("add account Calendar navigation button must remain selectable: %s", addCalendarButton)
	}
	for _, want := range []string{
		"Calendar",
		"Provider access",
		`data-wizard-service-switch="calendar"`,
		`data-wizard-step="4"`,
		`data-wizard-step="5"`,
		`add-step-5-content`,
	} {
		if !strings.Contains(addOut.String(), want) {
			t.Fatalf("add account dialog missing Calendar service marker %q: %s", want, addOut.String())
		}
	}

	var postCreateOut bytes.Buffer
	if err := AddAccountPostCreateStep("created-account").Render(context.Background(), &postCreateOut); err != nil {
		t.Fatalf("AddAccountPostCreateStep.Render() error = %v", err)
	}
	for _, want := range []string{`add-wizard-footer-step-4`, `add-wizard-footer-step-5`, `add-step-4-content`, `add-step-5-content`, "Contact sync", "Calendar sync"} {
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
		`handleEditAccountSaveStart(event)`,
		`handleEditAccountSaveResult(event)`,
		`data-account-edit-submit-button`,
		`data-account-edit-submit-spinner`,
		`data-wizard-step="4"`,
		`data-wizard-step="5"`,
		`edit-step-5-content`,
		`data-wizard-footer-step="5"`,
		`onclick="editWizardGo(4)"`,
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
	for name, html := range map[string]string{"post-create": postCreateOut.String(), "edit": editOut.String()} {
		for _, want := range []string{`data-wizard-test-results`, `hx-trigger="wizard-test"`, "Testing connection..."} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s account wizard missing live test marker %q: %s", name, want, html)
			}
		}
		if strings.Contains(html, `hx-trigger="load"`) {
			t.Fatalf("%s account wizard must not test while the Finish step is hidden: %s", name, html)
		}
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
		`handleCalendarDiscoveryStart(event)`,
		`handleCalendarDiscoveryResult(event)`,
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
