package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func iCloudEditData() models.EditAccountData {
	return models.EditAccountData{
		AccountID:    "acc-icloud",
		Provider:     "imap",
		AuthMethod:   "plain",
		EmailAddress: "robin@icloud.com",
		DisplayName:  "Robin",
		IMAPHost:     "imap.mail.me.com",
		IMAPPort:     993,
		IMAPTLSMode:  "tls",
		SMTPHost:     "smtp.mail.me.com",
		SMTPPort:     587,
		SMTPTLSMode:  "starttls",
		Username:     "robin@icloud.com",
		SameSmtpAuth: true,
	}
}

func TestEditAccountDialogHidesICloudServerSettings(t *testing.T) {
	var out bytes.Buffer
	if err := EditAccountDialog(iCloudEditData()).Render(context.Background(), &out); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`name="provider" value="imap"`,
		`name="mail_service" value="icloud"`,
		`name="username"`,
		`value="robin@icloud.com"`,
		"App-specific password",
		"iCloud Mail syncs and sends with an app-specific password",
		"data-wizard-skip-sending",
		"Apple Account email and app-specific password. If contact sync reports a sign-in problem",
		`name="base_url" value="https://contacts.icloud.com"`,
		"iCloud Calendar access",
		`name="caldav_url" value="https://caldav.icloud.com"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("iCloud edit dialog missing %q", want)
		}
	}
	for _, notWant := range []string{
		`name="imap_host"`,
		`name="imap_port"`,
		`name="smtp_host"`,
		`name="smtp_username"`,
		`name="auth_method"`,
		"IMAP Host",
		"SMTP Host",
		">Base URL</label>",
		"data-carddav-autodiscover",
		"data-calendar-autodiscover",
		"Enter address book URL manually",
		`data-account-wizard-step="2"`,
	} {
		if strings.Contains(html, notWant) {
			t.Fatalf("iCloud edit dialog should not render %q", notWant)
		}
	}
}

func TestEditAccountDialogKeepsServerSettingsForOtherIMAPAccounts(t *testing.T) {
	data := iCloudEditData()
	data.IMAPHost, data.SMTPHost = "imap.example.org", "smtp.example.org"
	var out bytes.Buffer
	if err := EditAccountDialog(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("EditAccountDialog.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, `name="imap_host"`) || !strings.Contains(html, `name="smtp_host"`) || strings.Contains(html, `name="mail_service"`) || strings.Contains(html, "data-wizard-skip-sending") {
		t.Fatalf("generic IMAP edit dialog should keep server settings and no iCloud marker")
	}
}

func TestAddAccountPostCreateStepUsesICloudContacts(t *testing.T) {
	var out bytes.Buffer
	if err := AddAccountPostCreateStep(iCloudEditData()).Render(context.Background(), &out); err != nil {
		t.Fatalf("AddAccountPostCreateStep.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, `name="base_url" value="https://contacts.icloud.com"`) || strings.Contains(html, ">Base URL</label>") {
		t.Fatalf("post-create contacts step should point at iCloud without a Base URL field")
	}
}

func TestAddAccountDialogsOfferICloud(t *testing.T) {
	var chooser bytes.Buffer
	if err := ChooseAccountTypeDialog().Render(context.Background(), &chooser); err != nil {
		t.Fatalf("ChooseAccountTypeDialog.Render() error = %v", err)
	}
	if !strings.Contains(chooser.String(), `["openAddAccountWizard","icloud"]`) {
		t.Fatalf("account type chooser should offer iCloud")
	}
	var add bytes.Buffer
	if err := AddAccountDialog().Render(context.Background(), &add); err != nil {
		t.Fatalf("AddAccountDialog.Render() error = %v", err)
	}
	for _, want := range []string{`name="mail_service" value=""`, `data-add-mode="icloud"`, "Add iCloud Account"} {
		if !strings.Contains(add.String(), want) {
			t.Fatalf("add account dialog missing %q", want)
		}
	}
}

func TestSettingsAccountCardLabelsICloudAccounts(t *testing.T) {
	var out bytes.Buffer
	account := models.Account{ID: "acc-icloud", Name: "Robin", Email: "robin@icloud.com", Provider: "imap", MailService: "icloud", EmailSyncEnabled: true}
	if err := SettingsAccountCard(account).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsAccountCard.Render() error = %v", err)
	}
	if !strings.Contains(out.String(), "iCloud") {
		t.Fatalf("account card should label the account iCloud")
	}
}
