package handler

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newProfileTestHandler(t *testing.T) *Handler {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	return &Handler{db: db, accountStore: store}
}

// seedProfileSource builds a profile with an IMAP account using every
// service, an Outlook account, a signature, preferences and contacts.
func seedProfileSource(t *testing.T, h *Handler) (imapID, outlookID string) {
	t.Helper()
	ctx := t.Context()
	imap, err := h.accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "robin@mail.example.test", DisplayName: "Robin Example",
		IMAPHost: "imap.example.test", IMAPPort: 993, IMAPTLSMode: "tls",
		SMTPHost: "smtp.example.test", SMTPPort: 465, SMTPTLSMode: "tls",
		Username: "robin", Password: "mailbox-secret-value", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatalf("create IMAP account: %v", err)
	}
	if err := h.accountStore.UpdateAccountColor(ctx, "default", imap.ID, "#336699"); err != nil {
		t.Fatalf("set color: %v", err)
	}
	if err := h.accountStore.SaveContactSyncConfig(ctx, "default", imap.ID, models.ContactSyncConfig{
		Provider: "carddav", Enabled: true, BaseURL: "https://dav.example.test/", Username: "robin",
		AddressBooks: []models.ContactAddressBook{{URL: "https://dav.example.test/books/main/", Name: "Main", Default: true, Selected: true}},
	}, "carddav-secret-value"); err != nil {
		t.Fatalf("save contact sync: %v", err)
	}
	if err := h.accountStore.SaveCalDAVConfig(ctx, "default", imap.ID, "https://cal.example.test/", "", "", true); err != nil {
		t.Fatalf("save CalDAV: %v", err)
	}
	if err := h.db.ReplaceCalendarSources(ctx, "default", imap.ID, "caldav", []storage.CalendarSource{
		{RemoteID: "/cal/work/", Name: "Work", IsSelected: true},
		{RemoteID: "/cal/holidays/", Name: "Holidays", IsSelected: true},
		{RemoteID: "/cal/archive/", Name: "Archive"},
	}); err != nil {
		t.Fatalf("save calendars: %v", err)
	}
	sources, err := h.db.ListCalendarSourcesForAccount(ctx, "default", imap.ID)
	if err != nil {
		t.Fatalf("list calendars: %v", err)
	}
	for _, source := range sources {
		if source.RemoteID == "/cal/holidays/" {
			if err := h.db.SetCalendarSourceVisibility(ctx, "default", source.ID, false); err != nil {
				t.Fatalf("hide calendar: %v", err)
			}
		}
	}

	if _, err := h.db.Write().Exec(`
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('outlook-account', 'default', 'outlook', 'subject-id', 'sam@outlook.example.test', 'Sam Example', 'oauth2')`); err != nil {
		t.Fatalf("insert Outlook account: %v", err)
	}

	signature, err := h.db.SaveSignature(ctx, "default", models.Signature{Name: "Work", HTMLBody: "<p>Robin</p>", TextBody: "Robin"})
	if err != nil {
		t.Fatalf("save signature: %v", err)
	}
	if err := h.db.SaveAccountSignatureSettings(ctx, "default", models.AccountSignatureSettings{
		AccountID: imap.ID, NewSignatureID: signature.ID, NewEnabled: true, ReplyPlacement: "before", ForwardPlacement: "before",
	}); err != nil {
		t.Fatalf("save signature settings: %v", err)
	}
	if _, err := h.db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: imap.ID, Name: "Robin Sales", Email: "sales@example.test", ReplyTo: "team@example.test", SignatureID: signature.ID}, nil); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	shop, err := h.db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: imap.ID, Name: "Robin's Shop", Email: "shop@example.test", SMTPHost: "smtp.shop.example.test", SMTPPort: 587, SMTPTLSMode: "starttls", SMTPUsername: "robin-shop"}, nil)
	if err != nil {
		t.Fatalf("save identity with its own server: %v", err)
	}
	if err := h.accountStore.SetIdentitySMTPPassword(ctx, "default", imap.ID, shop.ID, "shop-smtp-secret", nil); err != nil {
		t.Fatalf("set identity password: %v", err)
	}
	if err := h.db.ReplaceProviderIdentities(ctx, imap.ID, []models.Identity{{ProviderIdentityID: "p1", Email: "provider-only@example.test"}}); err != nil {
		t.Fatalf("save provider identity: %v", err)
	}

	if err := h.db.SetUISettings(ctx, "default", map[string]string{
		"theme":                     "light",
		"theme_style":               "minimal",
		"sidebar_account_collapsed": `{"` + imap.ID + `":true,"outlook-account":true}`,
		"unified_folder_inbox_account_" + imap.ID + "_enabled": "false",
		"unified_folder_inbox_account_outlook-account_enabled": "false",
		"sidebar_folder_collapsed":                             `{"` + imap.ID + `:` + imap.ID + `_f_0011aabb":true}`,
	}); err != nil {
		t.Fatalf("save UI settings: %v", err)
	}
	if err := h.db.SetSettings(ctx, "default", map[string]string{
		"sync_interval_minutes": "15",
		"idle_folders":          `{"` + imap.ID + `":["inbox","` + imap.ID + `_f_0011aabb"],"outlook-account":["none"]}`,
		storage.RemoteContentSenderSettingPrefix + strings.Repeat("ab", 32): "1",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	if _, err := h.db.SaveContact(ctx, "default", models.Contact{Name: "Jordan Sample", Email: "jordan@example.test", Phone: "+1 555 0100", Organization: "Sample Co"}); err != nil {
		t.Fatalf("save contact: %v", err)
	}
	return imap.ID, "outlook-account"
}

func exportProfileForTest(t *testing.T, h *Handler, includeContacts bool) []byte {
	t.Helper()
	file, err := exportProfile(t.Context(), h.accountStore, h.db, "default", includeContacts, time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("exportProfile() error = %v", err)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return data
}

func TestProfileExportLeavesOutCredentialsAndAccountIDs(t *testing.T) {
	source := newProfileTestHandler(t)
	imapID, _ := seedProfileSource(t, source)
	data := string(exportProfileForTest(t, source, true))

	for _, secret := range []string{"mailbox-secret-value", "carddav-secret-value", "encrypted"} {
		if strings.Contains(data, secret) {
			t.Fatalf("export contains %q", secret)
		}
	}
	if strings.Contains(data, imapID) || strings.Contains(data, "outlook-account") {
		t.Fatalf("export names account IDs instead of placeholders: %s", data)
	}
	if !strings.Contains(data, "Jordan Sample") {
		t.Fatalf("export is missing the Gofer contact")
	}
	if !strings.Contains(data, "sales@example.test") || strings.Contains(data, "provider-only@example.test") {
		t.Fatalf("export should carry manual identities only: %s", data)
	}
	if !strings.Contains(data, "smtp.shop.example.test") || strings.Contains(data, "shop-smtp-secret") {
		t.Fatalf("export should carry an identity's server but not its password: %s", data)
	}
	if strings.Contains(string(exportProfileForTest(t, source, false)), "Jordan Sample") {
		t.Fatalf("export without contacts still has them")
	}
}

func TestProfileImportAddsAccountsWaitingForReconnect(t *testing.T) {
	source := newProfileTestHandler(t)
	seedProfileSource(t, source)
	data := exportProfileForTest(t, source, true)

	target := newProfileTestHandler(t)
	ctx := t.Context()
	file, err := parseProfile(data)
	if err != nil {
		t.Fatalf("parseProfile() error = %v", err)
	}
	options := profileImportOptions{Preferences: true, Signatures: true, Contacts: true, Accounts: map[string]bool{}}
	for _, account := range file.Accounts {
		options.Accounts[account.Ref] = true
	}
	result, err := target.importProfile(ctx, "default", file, options)
	if err != nil {
		t.Fatalf("importProfile() error = %v", err)
	}
	// Outlook sign-in is not set up on the target, so only the IMAP account is added.
	if !slices.Equal(result.Accounts, []string{"robin@mail.example.test"}) || len(result.AccountFailures) != 0 {
		t.Fatalf("imported accounts = %v, failures = %v", result.Accounts, result.AccountFailures)
	}
	if result.Signatures != 1 || result.Contacts != 1 || !result.Preferences {
		t.Fatalf("result = %+v", result)
	}

	accounts, err := target.db.GetAccounts(ctx, "default")
	if err != nil || len(accounts) != 1 {
		t.Fatalf("GetAccounts() = %v, %v", accounts, err)
	}
	account := accounts[0]
	if account.EmailSyncEnabled || account.ContactSyncEnabled || account.CalendarSyncEnabled {
		t.Fatalf("imported account has services on: %+v", account)
	}
	if !account.ReconnectPending || account.Color != "#336699" || account.Name != "Robin Example" {
		t.Fatalf("imported account = %+v", account)
	}
	cfg, err := target.accountStore.GetConfig(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if cfg.IMAPHost != "imap.example.test" || cfg.SMTPHost != "smtp.example.test" || cfg.Username != "robin" {
		t.Fatalf("imported server settings = %+v", cfg)
	}
	if password, err := target.accountStore.DecryptPassword(ctx, account.ID); err != nil || password != "" {
		t.Fatalf("imported password = %q, %v", password, err)
	}

	edit, err := target.accountStore.GetEditData(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetEditData() error = %v", err)
	}
	if edit.ContactSync.BaseURL != "https://dav.example.test/" || edit.ContactSync.Enabled || edit.ContactSync.HasPassword || len(edit.ContactSync.AddressBooks) != 1 {
		t.Fatalf("imported contact sync = %+v", edit.ContactSync)
	}
	if edit.CalendarSync.CalDAVBaseURL != "https://cal.example.test/" || !edit.CalendarSync.CalDAVUseAccountCredentials || len(edit.CalendarSync.Sources) != 3 {
		t.Fatalf("imported calendar sync = %+v", edit.CalendarSync)
	}
	imported, err := target.db.ListCalendarSourcesForAccount(ctx, "default", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	hidden := map[string]bool{}
	for _, source := range imported {
		hidden[source.Name] = source.IsHidden
	}
	if !hidden["Holidays"] || hidden["Work"] {
		t.Fatalf("calendar visibility = %v", hidden)
	}
	if len(edit.Signatures) != 1 || edit.SignatureSettings.NewSignatureID != edit.Signatures[0].ID || !edit.SignatureSettings.NewEnabled {
		t.Fatalf("signatures = %+v, settings = %+v", edit.Signatures, edit.SignatureSettings)
	}
	identities, err := target.db.ListAccountIdentities(ctx, "default", account.ID)
	if err != nil || len(identities) != 2 || identities[0].Name != "Robin Sales" || identities[0].Email != "sales@example.test" || identities[0].ReplyTo != "<team@example.test>" || identities[0].SignatureID != edit.Signatures[0].ID || identities[0].Source != models.IdentitySourceManual {
		t.Fatalf("imported identities = %+v, %v", identities, err)
	}
	// The identity's server comes along; its password is entered again.
	if shop := identities[1]; shop.Email != "shop@example.test" || shop.SMTPHost != "smtp.shop.example.test" || shop.SMTPPort != 587 || shop.SMTPTLSMode != "starttls" || shop.SMTPUsername != "robin-shop" || shop.HasSMTPPassword {
		t.Fatalf("imported identity with its own server = %+v", shop)
	}

	ui := target.db.GetUISettings(ctx, "default")
	if ui["theme"] != "light" || ui["theme_style"] != "minimal" {
		t.Fatalf("theme = %q/%q", ui["theme"], ui["theme_style"])
	}
	if ui["unified_folder_inbox_account_"+account.ID+"_enabled"] != "false" {
		t.Fatalf("unified folder setting was not moved to the new account: %v", ui)
	}
	if ui["sidebar_account_collapsed"] != `{"`+account.ID+`":true}` {
		t.Fatalf("sidebar_account_collapsed = %s", ui["sidebar_account_collapsed"])
	}
	if ui["sidebar_folder_collapsed"] != `{"`+account.ID+`:`+account.ID+`_f_0011aabb":true}` {
		t.Fatalf("sidebar_folder_collapsed = %s", ui["sidebar_folder_collapsed"])
	}
	for key := range ui {
		if strings.Contains(key, "@account:") {
			t.Fatalf("unresolved setting %q", key)
		}
	}
	if idle, _ := target.db.GetSetting(ctx, "default", "idle_folders"); idle != `{"`+account.ID+`":["inbox","`+account.ID+`_f_0011aabb"]}` {
		t.Fatalf("idle_folders = %s", idle)
	}
	if interval := target.db.GetSyncInterval(ctx, "default"); interval != 15 {
		t.Fatalf("sync interval = %d", interval)
	}
	if allowed, _ := target.db.GetSetting(ctx, "default", storage.RemoteContentSenderSettingPrefix+strings.Repeat("ab", 32)); allowed != "1" {
		t.Fatalf("allowed sender was not imported")
	}
	contacts, err := target.db.ListContactsForExport(ctx, "default")
	if err != nil || len(contacts) != 1 || contacts[0].Email != "jordan@example.test" {
		t.Fatalf("contacts = %+v, %v", contacts, err)
	}

	// Entering the password turns the services back on.
	if !slices.Equal(account.ReconnectNeeds, []string{"mail"}) {
		t.Fatalf("an account whose services share the mail login needs %v", account.ReconnectNeeds)
	}
	if remaining, waiting, err := target.resolveImportedLogin(ctx, "default", account.ID, storage.ImportedLoginMail); err != nil || !waiting || len(remaining) != 0 {
		t.Fatalf("resolveImportedLogin() = %v, %t, %v", remaining, waiting, err)
	}
	accounts, err = target.db.GetAccounts(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccounts() error = %v", err)
	}
	account = accounts[0]
	if !account.EmailSyncEnabled || !account.ContactSyncEnabled || !account.CalendarSyncEnabled || account.ReconnectPending {
		t.Fatalf("reconnected account = %+v", account)
	}
	sources, err := target.db.ListCalendarSourcesForAccount(ctx, "default", account.ID)
	if err != nil {
		t.Fatalf("ListCalendarSourcesForAccount() error = %v", err)
	}
	for _, source := range sources {
		if want := source.RemoteID != "/cal/archive/"; source.IsSelected != want {
			t.Fatalf("calendar %s selected = %t, want %t", source.Name, source.IsSelected, want)
		}
	}
	// A second reconnect changes nothing.
	if err := target.accountStore.SetEmailSyncEnabled(ctx, "default", account.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, waiting, err := target.resolveImportedLogin(ctx, "default", account.ID, storage.ImportedLoginMail); err != nil || waiting {
		t.Fatalf("resolveImportedLogin() again = %t, %v", waiting, err)
	}
	if accounts, _ := target.db.GetAccounts(ctx, "default"); accounts[0].EmailSyncEnabled {
		t.Fatalf("a finished account was changed again")
	}
}

func TestProfileImportTwiceAddsNothingNew(t *testing.T) {
	source := newProfileTestHandler(t)
	seedProfileSource(t, source)
	file, err := parseProfile(exportProfileForTest(t, source, true))
	if err != nil {
		t.Fatal(err)
	}
	target := newProfileTestHandler(t)
	options := profileImportOptions{Preferences: true, Signatures: true, Contacts: true, Accounts: map[string]bool{}}
	for _, account := range file.Accounts {
		options.Accounts[account.Ref] = true
	}
	if _, err := target.importProfile(t.Context(), "default", file, options); err != nil {
		t.Fatal(err)
	}
	result, err := target.importProfile(t.Context(), "default", file, options)
	if err != nil {
		t.Fatalf("second importProfile() error = %v", err)
	}
	if len(result.Accounts) != 0 || result.Signatures != 0 {
		t.Fatalf("second import added %+v", result)
	}
	accounts, _ := target.db.GetAccounts(t.Context(), "default")
	signatures, _ := target.db.ListSignatures(t.Context(), "default")
	contacts, _ := target.db.ListContactsForExport(t.Context(), "default")
	if len(accounts) != 1 || len(signatures) != 1 || len(contacts) != 1 {
		t.Fatalf("after two imports: %d accounts, %d signatures, %d contacts", len(accounts), len(signatures), len(contacts))
	}
	if identities, _ := target.db.ListAccountIdentities(t.Context(), "default", accounts[0].ID); len(identities) != 2 {
		t.Fatalf("after two imports: identities = %+v", identities)
	}
}

func TestProfileImportIntoInstallationWithTheAccount(t *testing.T) {
	h := newProfileTestHandler(t)
	imapID, _ := seedProfileSource(t, h)
	file, err := parseProfile(exportProfileForTest(t, h, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUISettings(t.Context(), "default", map[string]string{"theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	existing, err := h.profileExistingAccounts(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	for ref, plan := range h.profileAccountPlans(file, existing) {
		if plan.Status != profileImportExists || !plan.CanReplace || plan.ExistingID == "" {
			t.Fatalf("account %s plan = %+v", ref, plan)
		}
	}
	if _, err := h.importProfile(t.Context(), "default", file, profileImportOptions{Preferences: true, Accounts: map[string]bool{}}); err != nil {
		t.Fatal(err)
	}
	ui := h.db.GetUISettings(t.Context(), "default")
	if ui["theme"] != "light" || ui["unified_folder_inbox_account_"+imapID+"_enabled"] != "false" {
		t.Fatalf("settings for existing accounts were not restored: %v", ui)
	}
}

func TestParseProfileRejectsOtherFiles(t *testing.T) {
	for name, data := range map[string]string{
		"not json":     "BEGIN:VCARD",
		"other format": `{"format":"something-else","version":1}`,
		"newer":        `{"format":"gofer-profile","version":99}`,
		"bad ref":      `{"format":"gofer-profile","version":1,"accounts":[{"ref":"acc_1","email_address":"a@example.test"}]}`,
		"duplicate":    `{"format":"gofer-profile","version":1,"accounts":[{"ref":"@account:1@","email_address":"a@example.test"},{"ref":"@account:1@","email_address":"b@example.test"}]}`,
	} {
		if _, err := parseProfile([]byte(data)); err == nil {
			t.Errorf("%s: parseProfile() accepted the file", name)
		}
	}
	if _, err := parseProfile([]byte(`{"format":"gofer-profile","version":1}`)); err != nil {
		t.Fatalf("parseProfile() rejected an empty profile: %v", err)
	}
}

func TestMergeProfileSettingDropsAccountsThatWereNotImported(t *testing.T) {
	ids := map[string]string{"@account:1@": "new-id"}
	merged, ok := mergeProfileSetting(`{"kept":true}`, `{"@account:1@":true,"@account:2@":true}`, ids)
	if !ok || merged != `{"kept":true,"new-id":true}` {
		t.Fatalf("merged = %s, %t", merged, ok)
	}
	if _, ok := mergeProfileSetting("", "@account:2@", ids); ok {
		t.Fatalf("a setting naming a missing account was kept")
	}
	if value, ok := mergeProfileSetting("", "plain", ids); !ok || value != "plain" {
		t.Fatalf("plain value = %q, %t", value, ok)
	}
}

func TestProfileImportContactConflicts(t *testing.T) {
	target := newProfileTestHandler(t)
	ctx := t.Context()
	for _, contact := range []models.Contact{
		{Name: "Jordan Sample", Email: "jordan@example.test", Phone: "+1 555 0100"},
		{Name: "Avery Sample", Email: "avery@example.test", Notes: "kept"},
		{Name: "Morgan Sample", Phone: "+1 555 0102"},
		{Name: "Riley Sample", Phone: "+1 555 0103"},
	} {
		if _, err := target.db.SaveContact(ctx, "default", contact); err != nil {
			t.Fatal(err)
		}
	}
	vcards := []string{
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Jordan Changed\r\nEMAIL:jordan@example.test\r\nORG:Sample Co\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Avery Changed\r\nEMAIL:avery@example.test\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Morgan Sample\r\nEMAIL:morgan@example.test\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Riley Sample\r\nTEL:+1 555 0199\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Quinn Sample\r\nEMAIL:quinn@example.test\r\nEND:VCARD\r\n",
	}
	file, err := parseProfile([]byte(`{"format":"gofer-profile","version":1,"contacts":` + strconv.Quote(strings.Join(vcards, "")) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	contacts, err := profileContacts(file)
	if err != nil {
		t.Fatal(err)
	}
	conflicts, err := findProfileContactConflicts(ctx, target.db, "default", contacts)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, conflict := range conflicts {
		got[conflict.Index] = fmt.Sprintf("%t/%s", conflict.ByEmail, conflict.DefaultAction())
	}
	want := map[int]string{0: "true/skip", 1: "true/skip", 2: "false/add", 3: "false/skip"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("conflicts = %v, want %v", got, want)
	}

	// Jordan is skipped by default, Avery replaced, Morgan kept beside the
	// existing one, Riley kept beside it on request, and Quinn added.
	result, err := target.importProfile(ctx, "default", file, profileImportOptions{
		Contacts:       true,
		ContactActions: map[int]string{1: profileConflictReplace, 3: profileConflictKeepBoth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Contacts != 3 || result.ContactsReplaced != 1 || result.ContactsSkipped != 1 {
		t.Fatalf("result = %+v", result)
	}
	listed, err := target.db.ListContactsForExport(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, contact := range listed {
		names[contact.Name]++
	}
	if names["Jordan Sample"] != 1 || names["Jordan Changed"] != 0 || names["Avery Changed"] != 1 || names["Avery Sample"] != 0 ||
		names["Morgan Sample"] != 2 || names["Riley Sample"] != 2 || names["Quinn Sample"] != 1 {
		t.Fatalf("contacts after import = %v", names)
	}
}

func TestProfileImportReplacesConnectedAccountSettings(t *testing.T) {
	source := newProfileTestHandler(t)
	seedProfileSource(t, source)
	file, err := parseProfile(exportProfileForTest(t, source, false))
	if err != nil {
		t.Fatal(err)
	}

	target := newProfileTestHandler(t)
	ctx := t.Context()
	current, err := target.accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "robin@mail.example.test", DisplayName: "Old Name",
		IMAPHost: "old-imap.example.test", IMAPPort: 993, IMAPTLSMode: "tls",
		SMTPHost: "old-smtp.example.test", SMTPPort: 465, SMTPTLSMode: "tls",
		Username: "robin", Password: "kept-password", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	options := profileImportOptions{Signatures: true, Accounts: map[string]bool{}, AccountActions: map[string]string{}}
	for _, account := range file.Accounts {
		options.AccountActions[account.Ref] = profileConflictReplace
	}
	result, err := target.importProfile(ctx, "default", file, options)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.AccountsReplaced, []string{"robin@mail.example.test"}) || len(result.Accounts) != 0 {
		t.Fatalf("result = %+v", result)
	}
	cfg, err := target.accountStore.GetConfig(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAPHost != "imap.example.test" || cfg.SMTPHost != "smtp.example.test" {
		t.Fatalf("servers were not replaced: %+v", cfg)
	}
	if password, err := target.accountStore.DecryptPassword(ctx, current.ID); err != nil || password != "kept-password" {
		t.Fatalf("password = %q, %v", password, err)
	}
	accounts, err := target.db.GetAccounts(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	account := accounts[0]
	if account.Name != "Robin Example" || account.Color != "#336699" || !account.EmailSyncEnabled || !account.ContactSyncEnabled || account.ReconnectPending {
		t.Fatalf("replaced account = %+v", account)
	}
	sources, err := target.db.ListCalendarSourcesForAccount(ctx, "default", current.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, source := range sources {
		state[source.RemoteID] = fmt.Sprintf("%t/%t", source.IsSelected, source.IsHidden)
	}
	if state["/cal/work/"] != "true/false" || state["/cal/holidays/"] != "true/true" || state["/cal/archive/"] != "false/false" {
		t.Fatalf("calendars = %v", state)
	}
	edit, err := target.accountStore.GetEditData(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if edit.ContactSync.BaseURL != "https://dav.example.test/" || !edit.SignatureSettings.NewEnabled {
		t.Fatalf("contacts = %+v, signatures = %+v", edit.ContactSync, edit.SignatureSettings)
	}
}

func TestProfileImportWaitsForEachSeparateLogin(t *testing.T) {
	file, err := parseProfile([]byte(`{"format":"gofer-profile","version":1,"accounts":[{
		"ref":"@account:1@","provider":"imap","email_address":"pat@mail.example.test","display_name":"Pat Example","auth_method":"plain",
		"imap_host":"imap.example.test","imap_port":993,"imap_tls_mode":"tls","smtp_host":"smtp.example.test","smtp_port":465,"smtp_tls_mode":"tls",
		"username":"pat","smtp_username":"pat-sender","email_sync":true,
		"contacts":{"enabled":true,"base_url":"https://dav.example.test/","username":"pat-contacts","address_books":[{"url":"https://dav.example.test/books/pat/","name":"Pat","default":true}]},
		"calendar":{"caldav_base_url":"https://cal.example.test/","caldav_username":"pat-calendar","calendars":[{"remote_id":"/cal/pat/","name":"Pat","selected":true},{"remote_id":"/cal/other/","name":"Other"}]}
	}]}`))
	if err != nil {
		t.Fatal(err)
	}
	target := newProfileTestHandler(t)
	ctx := t.Context()
	if _, err := target.importProfile(ctx, "default", file, profileImportOptions{Accounts: map[string]bool{"@account:1@": true}}); err != nil {
		t.Fatal(err)
	}
	account := func() models.Account {
		t.Helper()
		accounts, err := target.db.GetAccounts(ctx, "default")
		if err != nil || len(accounts) != 1 {
			t.Fatalf("GetAccounts() = %v, %v", accounts, err)
		}
		return accounts[0]
	}
	id := account().ID
	if needs := account().ReconnectNeeds; !slices.Equal(needs, []string{"mail", "contacts", "calendar"}) {
		t.Fatalf("needs = %v", needs)
	}
	edit, err := target.accountStore.GetEditData(ctx, id)
	if err != nil || !edit.ReconnectSMTP || !edit.ReconnectContacts || !edit.ReconnectCalendar {
		t.Fatalf("edit data = %+v, %v", edit, err)
	}

	// The mail save needs the sending password too, since it is separate.
	recorder := httptest.NewRecorder()
	if err := target.resolveImportedMailLogin(recorder, ctx, "default", id, &models.CreateAccountRequest{Password: "invented"}); err != nil {
		t.Fatal(err)
	}
	if needs := account().ReconnectNeeds; !slices.Contains(needs, "mail") || recorder.Header().Get("X-Gofer-Reconnect-Resolved") != "" {
		t.Fatalf("mail resolved without the sending password: %v", needs)
	}

	// Contacts first: only contacts turn on.
	if remaining, _, err := target.resolveImportedLogin(ctx, "default", id, storage.ImportedLoginContacts); err != nil || !slices.Equal(remaining, []string{"mail", "calendar"}) {
		t.Fatalf("after contacts: %v, %v", remaining, err)
	}
	if current := account(); !current.ContactSyncEnabled || current.EmailSyncEnabled || current.CalendarSyncEnabled || !current.ReconnectPending {
		t.Fatalf("after contacts: %+v", current)
	}

	recorder = httptest.NewRecorder()
	if err := target.resolveImportedMailLogin(recorder, ctx, "default", id, &models.CreateAccountRequest{Password: "invented", SmtpPassword: "invented"}); err != nil {
		t.Fatal(err)
	}
	if recorder.Header().Get("X-Gofer-Reconnect-Resolved") != "mail" || recorder.Header().Get("X-Gofer-Reconnect-Remaining") != "calendar" {
		t.Fatalf("headers = %v", recorder.Header())
	}
	if current := account(); !current.EmailSyncEnabled || current.CalendarSyncEnabled || !slices.Equal(current.ReconnectNeeds, []string{"calendar"}) {
		t.Fatalf("after mail: %+v", current)
	}

	if remaining, _, err := target.resolveImportedLogin(ctx, "default", id, storage.ImportedLoginCalendar); err != nil || len(remaining) != 0 {
		t.Fatalf("after calendar: %v, %v", remaining, err)
	}
	current := account()
	if !current.CalendarSyncEnabled || current.ReconnectPending {
		t.Fatalf("after calendar: %+v", current)
	}
	sources, err := target.db.ListCalendarSourcesForAccount(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source.IsSelected != (source.RemoteID == "/cal/pat/") {
			t.Fatalf("calendar %s selected = %t", source.RemoteID, source.IsSelected)
		}
	}
}

func TestProfileImportGmailWaitsForSignIn(t *testing.T) {
	pending := profileImportPending(profileAccount{Provider: "gmail", EmailSync: true, Contacts: &profileContactSync{Enabled: true},
		Calendar: &profileCalendarSync{Calendars: []profileCalendar{{RemoteID: "primary", Selected: true}}}})
	if !slices.Equal(pending.Missing(), []string{"signin"}) {
		t.Fatalf("needs = %v", pending.Missing())
	}
}

// A JMAP account travels with its session URL and CalDAV setup, and comes
// back as a JMAP account waiting for its password. A profile from before
// JMAP support, without the session URL, neither adds nor replaces one.
func TestProfileJMAPAccountRoundTrip(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	ctx := t.Context()
	const sessionURL = "https://jmap.example.test/.well-known/jmap"
	source := newProfileTestHandler(t)
	created, err := source.accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "sam@jmap.example.test", DisplayName: "Sam Rivera",
		JMAPSessionURL: sessionURL, Username: "sam@jmap.example.test", Password: "jmap-secret-value", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatalf("create JMAP account: %v", err)
	}
	if err := source.accountStore.SaveCalDAVConfig(ctx, "default", created.ID, "https://jmap.example.test/dav/cal/", "", "", true); err != nil {
		t.Fatalf("save CalDAV: %v", err)
	}
	if err := source.accountStore.SaveContactSyncConfig(ctx, "default", created.ID, models.ContactSyncConfig{Provider: "jmap", Enabled: true,
		AddressBooks: []models.ContactAddressBook{{URL: "B1", Name: "Personal", Default: true}}}, ""); err != nil {
		t.Fatalf("save JMAP contacts: %v", err)
	}
	data := exportProfileForTest(t, source, false)
	if strings.Contains(string(data), "jmap-secret-value") || !strings.Contains(string(data), `"jmap_session_url":"`+sessionURL+`"`) {
		t.Fatalf("exported profile = %s", data)
	}

	target := newProfileTestHandler(t)
	file, err := parseProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	options := profileImportOptions{Accounts: map[string]bool{file.Accounts[0].Ref: true}}
	result, err := target.importProfile(ctx, "default", file, options)
	if err != nil || !slices.Equal(result.Accounts, []string{"sam@jmap.example.test"}) || len(result.AccountFailures) != 0 {
		t.Fatalf("import = %+v, %v", result, err)
	}
	accounts, err := target.db.GetAccounts(ctx, "default")
	if err != nil || len(accounts) != 1 || !accounts[0].ReconnectPending || accounts[0].EmailSyncEnabled {
		t.Fatalf("imported accounts = %+v, %v", accounts, err)
	}
	id := accounts[0].ID
	cfg, err := target.accountStore.GetConfig(ctx, id)
	if err != nil || cfg.Provider != "jmap" || cfg.AuthMethod != "plain" || cfg.JMAPSessionURL != sessionURL || cfg.Username != "sam@jmap.example.test" {
		t.Fatalf("imported account config = %+v, %v", cfg, err)
	}
	if push, err := target.db.ProviderPushEnabled(ctx, id); err != nil || !push {
		t.Fatalf("imported account notifications = %t, %v", push, err)
	}
	if calendar, err := target.accountStore.GetCalendarSyncConfig(ctx, "default", id); err != nil || calendar.CalDAVBaseURL != "https://jmap.example.test/dav/cal/" {
		t.Fatalf("imported CalDAV = %+v, %v", calendar, err)
	}
	// Contacts over JMAP come back off, and start with the mailbox login.
	if contacts, err := target.accountStore.GetContactSyncConfig(ctx, "default", id); err != nil || contacts.Provider != "jmap" || contacts.Enabled ||
		len(contacts.AddressBooks) != 1 || contacts.AddressBooks[0].URL != "B1" || !contacts.AddressBooks[0].Default {
		t.Fatalf("imported contacts = %+v, %v", contacts, err)
	}
	if _, _, err := target.resolveImportedLogin(ctx, "default", id, storage.ImportedLoginMail); err != nil {
		t.Fatal(err)
	}
	if contacts, err := target.accountStore.GetContactSyncConfig(ctx, "default", id); err != nil || !contacts.Enabled {
		t.Fatalf("contacts after the mailbox login = %+v, %v", contacts, err)
	}

	// Replacing the account from the profile keeps it a JMAP account, and
	// brings the choice of scheduled checks.
	if err := source.db.SetProviderPushEnabled(ctx, "default", created.ID, false); err != nil {
		t.Fatal(err)
	}
	checks, err := parseProfile(exportProfileForTest(t, source, false))
	if err != nil || !checks.Accounts[0].ScheduledChecks {
		t.Fatalf("exported scheduled checks = %+v, %v", checks.Accounts, err)
	}
	if err := target.replaceProfileAccount(ctx, "default", id, checks.Accounts[0], nil); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if push, err := target.db.ProviderPushEnabled(ctx, id); err != nil || push {
		t.Fatalf("replaced account notifications = %t, %v", push, err)
	}
	if cfg, err := target.accountStore.GetConfig(ctx, id); err != nil || cfg.Provider != "jmap" || cfg.JMAPSessionURL != sessionURL {
		t.Fatalf("replaced account config = %+v, %v", cfg, err)
	}

	old := *file
	old.Accounts = []profileAccount{file.Accounts[0]}
	old.Accounts[0].JMAPSessionURL = ""
	existing, err := target.profileExistingAccounts(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if plan := target.profileAccountPlans(&old, existing)[old.Accounts[0].Ref]; plan.Status != profileImportExists || plan.CanReplace {
		t.Fatalf("old profile plan for a connected account = %+v", plan)
	}
	if plan := target.profileAccountPlans(&old, nil)[old.Accounts[0].Ref]; plan.Status != profileImportUnsupported {
		t.Fatalf("old profile plan for a new account = %+v", plan)
	}
}

// Contacts that sign in like the account itself, as an imported account's
// do, use the mailbox password; a password of their own still wins.
func TestCardDAVConfigFallsBackToTheMailboxPassword(t *testing.T) {
	h := newProfileTestHandler(t)
	ctx := t.Context()
	account, err := h.accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "robin@mail.example.test", IMAPHost: "imap.example.test", IMAPPort: 993, IMAPTLSMode: "tls",
		SMTPHost: "smtp.example.test", SMTPPort: 465, SMTPTLSMode: "tls", Username: "robin", Password: "mailbox-secret", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := models.ContactSyncConfig{Provider: "carddav", Enabled: true, BaseURL: "https://dav.example.test/", Username: "robin",
		AddressBooks: []models.ContactAddressBook{{URL: "https://dav.example.test/books/main/", Default: true, Selected: true}}}
	if err := h.accountStore.SaveContactSyncConfig(ctx, "default", account.ID, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if _, password, err := h.cardDAVConfig(ctx, "default", account.ID); err != nil || password != "mailbox-secret" {
		t.Fatalf("CardDAV password = %q, %v; want the mailbox password", password, err)
	}
	if err := h.accountStore.SaveContactSyncConfig(ctx, "default", account.ID, cfg, "contacts-secret"); err != nil {
		t.Fatal(err)
	}
	if _, password, err := h.cardDAVConfig(ctx, "default", account.ID); err != nil || password != "contacts-secret" {
		t.Fatalf("CardDAV password = %q, %v; want the contacts password", password, err)
	}
}

// Calendars read over JMAP come back as JMAP calendars, selected once the
// mailbox login is entered, with no CalDAV setup of their own.
func TestProfileJMAPCalendarsRoundTrip(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	ctx := t.Context()
	source := newProfileTestHandler(t)
	created, err := source.accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "sam@jmap.example.test", DisplayName: "Sam Rivera",
		JMAPSessionURL: "https://jmap.example.test/.well-known/jmap", Username: "sam@jmap.example.test", Password: "jmap-secret-value", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	home, team := jmapCalendarDAVCollection("home"), jmapCalendarDAVCollection("team")
	if err := source.db.ReplaceCalendarSources(ctx, "default", created.ID, storage.CalendarSourceProviderJMAP, []storage.CalendarSource{
		{RemoteID: home, Name: "Home", IsPrimary: true, IsSelected: true, AccessRole: "writer"},
		{RemoteID: team, Name: "Team", AccessRole: "reader"},
	}); err != nil {
		t.Fatal(err)
	}
	data := exportProfileForTest(t, source, false)
	if !strings.Contains(string(data), `"protocol":"jmap"`) || strings.Contains(string(data), "caldav_base_url") {
		t.Fatalf("exported profile = %s", data)
	}

	target := newProfileTestHandler(t)
	file, err := parseProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.importProfile(ctx, "default", file, profileImportOptions{Accounts: map[string]bool{file.Accounts[0].Ref: true}}); err != nil {
		t.Fatal(err)
	}
	accounts, err := target.db.GetAccounts(ctx, "default")
	if err != nil || len(accounts) != 1 {
		t.Fatalf("imported accounts = %+v, %v", accounts, err)
	}
	id := accounts[0].ID
	if _, _, err := target.resolveImportedLogin(ctx, "default", id, storage.ImportedLoginMail); err != nil {
		t.Fatal(err)
	}
	sources, err := target.db.ListCalendarSourcesForAccount(ctx, "default", id)
	if err != nil || len(sources) != 2 {
		t.Fatalf("imported calendars = %+v, %v", sources, err)
	}
	for _, s := range sources {
		if s.Provider != "jmap" || s.IsSelected != (s.RemoteID == home) {
			t.Fatalf("imported calendar = %+v", s)
		}
	}
	if cfg, err := target.accountStore.GetCalendarSyncConfig(ctx, "default", id); err != nil || cfg.Protocol != "jmap" || cfg.CalDAVBaseURL != "" {
		t.Fatalf("imported calendar setup = %+v, %v", cfg, err)
	}
}
