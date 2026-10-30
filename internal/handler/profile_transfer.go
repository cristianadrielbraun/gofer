package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A profile file carries a user's preferences, signatures, accounts and
// Gofer-only contacts to another installation. It never holds passwords or
// sign-in tokens: imported accounts wait until they are reconnected.
const (
	profileFormat = "gofer-profile"
	// profileVersion is the newest profile this Gofer reads. Version 2 added
	// accounts with no mail; a profile without one is still written as
	// version 1, so older Gofers keep reading it.
	profileVersion        = 2
	profileImportMaxBytes = 16 << 20
)

// profileAccountRefPattern matches the placeholders that stand for account IDs
// in an exported file, such as "@account:1@".
var profileAccountRefPattern = regexp.MustCompile(`@account:[0-9]+@`)

type profileFile struct {
	Format      string             `json:"format"`
	Version     int                `json:"version"`
	ExportedAt  string             `json:"exported_at"`
	Preferences profilePreferences `json:"preferences"`
	Signatures  []profileSignature `json:"signatures,omitempty"`
	Accounts    []profileAccount   `json:"accounts,omitempty"`
	// Contacts are the contacts kept only in Gofer, as vCard 4.0.
	Contacts string `json:"contacts,omitempty"`
}

// profilePreferences keep settings as stored, with account IDs replaced by
// the account's placeholder.
type profilePreferences struct {
	UI                  map[string]string `json:"ui,omitempty"`
	SyncIntervalMinutes string            `json:"sync_interval_minutes,omitempty"`
	IdleFolders         string            `json:"idle_folders,omitempty"`
	// AllowedSenders are the hashed addresses of senders whose remote content
	// is allowed.
	AllowedSenders []string `json:"allowed_senders,omitempty"`
}

type profileSignature struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	HTMLBody string `json:"html_body,omitempty"`
	TextBody string `json:"text_body,omitempty"`
}

type profileAccount struct {
	Ref               string `json:"ref"`
	Provider          string `json:"provider"`
	ProviderAccountID string `json:"provider_account_id,omitempty"`
	EmailAddress      string `json:"email_address"`
	DisplayName       string `json:"display_name,omitempty"`
	Color             string `json:"color,omitempty"`
	AuthMethod        string `json:"auth_method,omitempty"`
	IMAPHost          string `json:"imap_host,omitempty"`
	IMAPPort          int    `json:"imap_port,omitempty"`
	IMAPTLSMode       string `json:"imap_tls_mode,omitempty"`
	SMTPHost          string `json:"smtp_host,omitempty"`
	SMTPPort          int    `json:"smtp_port,omitempty"`
	SMTPTLSMode       string `json:"smtp_tls_mode,omitempty"`
	Username          string `json:"username,omitempty"`
	SMTPUsername      string `json:"smtp_username,omitempty"`
	JMAPSessionURL    string `json:"jmap_session_url,omitempty"`
	EmailSync         bool   `json:"email_sync"`
	// ScheduledChecks is set when the account chose scheduled checks over
	// its provider's notifications; older exports leave notifications on.
	ScheduledChecks bool                      `json:"scheduled_checks,omitempty"`
	Contacts        *profileContactSync       `json:"contacts,omitempty"`
	Calendar        *profileCalendarSync      `json:"calendar,omitempty"`
	Signatures      *profileAccountSignatures `json:"signatures,omitempty"`
	// Identities are the account's manual identities. Provider identities
	// come back from the provider once the account is reconnected.
	Identities []profileIdentity `json:"identities,omitempty"`
}

type profileIdentity struct {
	Name      string `json:"name,omitempty"`
	Email     string `json:"email"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Signature string `json:"signature,omitempty"`
	// SMTP is the identity's own server, without its password: an imported
	// identity needs the password entered again.
	SMTP *profileIdentitySMTP `json:"smtp,omitempty"`
}

type profileIdentitySMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLSMode  string `json:"tls_mode"`
	Username string `json:"username,omitempty"`
}

type profileContactSync struct {
	Enabled bool `json:"enabled"`
	// Protocol is "jmap" for contacts synced over a JMAP account's own
	// connection; otherwise they use CardDAV.
	Protocol     string               `json:"protocol,omitempty"`
	BaseURL      string               `json:"base_url,omitempty"`
	Username     string               `json:"username,omitempty"`
	AddressBooks []profileAddressBook `json:"address_books,omitempty"`
}

type profileAddressBook struct {
	URL     string `json:"url"`
	Name    string `json:"name,omitempty"`
	Default bool   `json:"default,omitempty"`
}

type profileCalendarSync struct {
	// Protocol is "jmap" for calendars read over a JMAP account's own
	// connection; otherwise they come from CalDAV or the provider.
	Protocol                    string            `json:"protocol,omitempty"`
	CalDAVBaseURL               string            `json:"caldav_base_url,omitempty"`
	CalDAVUsername              string            `json:"caldav_username,omitempty"`
	CalDAVUseAccountCredentials bool              `json:"caldav_use_account_credentials,omitempty"`
	Calendars                   []profileCalendar `json:"calendars,omitempty"`
}

type profileCalendar struct {
	RemoteID    string `json:"remote_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	TimeZone    string `json:"time_zone,omitempty"`
	Color       string `json:"color,omitempty"`
	AccessRole  string `json:"access_role,omitempty"`
	Primary     bool   `json:"primary,omitempty"`
	Selected    bool   `json:"selected,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
}

type profileAccountSignatures struct {
	New              profileSignatureUse `json:"new"`
	Reply            profileSignatureUse `json:"reply"`
	Forward          profileSignatureUse `json:"forward"`
	ReplyPlacement   string              `json:"reply_placement,omitempty"`
	ForwardPlacement string              `json:"forward_placement,omitempty"`
}

type profileSignatureUse struct {
	Signature string `json:"signature,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
}

// withProfileStore runs fn with the user's account store and database.
func (h *Handler) withProfileStore(ctx context.Context, owner string, fn func(*config.AccountStore, *storage.DB) error) error {
	if h.userAccounts != nil {
		return h.userAccounts.WithUser(ctx, owner, fn)
	}
	return fn(h.accountStore, h.db)
}

func (h *Handler) withProfileAccount(ctx context.Context, owner, accountID string, fn func(*config.AccountStore, *storage.DB) error) error {
	if h.userAccounts != nil {
		return h.userAccounts.WithAccountForUser(ctx, owner, accountID, fn)
	}
	return fn(h.accountStore, h.db)
}

func exportProfile(ctx context.Context, store *config.AccountStore, db *storage.DB, owner string, includeContacts bool, now time.Time) (*profileFile, error) {
	file := &profileFile{Format: profileFormat, Version: 1, ExportedAt: now.UTC().Format(time.RFC3339)}

	accounts, err := db.GetAccounts(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	signatures, err := db.ListSignatures(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("list signatures: %w", err)
	}
	signatureRefs := make(map[string]string, len(signatures))
	for i, signature := range signatures {
		ref := "signature-" + strconv.Itoa(i+1)
		signatureRefs[signature.ID] = ref
		file.Signatures = append(file.Signatures, profileSignature{Ref: ref, Name: signature.Name, HTMLBody: signature.HTMLBody, TextBody: signature.TextBody})
	}

	accountRefs := make(map[string]string, len(accounts))
	for i, account := range accounts {
		if account.IsDeleting {
			continue
		}
		ref := "@account:" + strconv.Itoa(i+1) + "@"
		accountRefs[account.ID] = ref
		exported, err := exportProfileAccount(ctx, store, db, owner, account, ref, signatureRefs)
		if err != nil {
			return nil, fmt.Errorf("export account %s: %w", account.ID, err)
		}
		if exported.Provider == providers.ProviderDAV {
			file.Version = 2
		}
		file.Accounts = append(file.Accounts, exported)
	}
	refs := profileAccountReplacer(accountRefs)

	if raw, err := db.GetSetting(ctx, owner, "ui_settings"); err != nil {
		return nil, err
	} else if strings.TrimSpace(raw) != "" {
		var ui map[string]string
		if err := json.Unmarshal([]byte(raw), &ui); err == nil && len(ui) > 0 {
			file.Preferences.UI = make(map[string]string, len(ui))
			for key, value := range ui {
				file.Preferences.UI[refs.Replace(key)] = refs.Replace(value)
			}
		}
	}
	if file.Preferences.SyncIntervalMinutes, err = db.GetSetting(ctx, owner, "sync_interval_minutes"); err != nil {
		return nil, err
	}
	idle, err := db.GetSetting(ctx, owner, "idle_folders")
	if err != nil {
		return nil, err
	}
	file.Preferences.IdleFolders = refs.Replace(idle)
	senders, err := db.ListSettingsWithPrefix(ctx, owner, storage.RemoteContentSenderSettingPrefix)
	if err != nil {
		return nil, err
	}
	for key, value := range senders {
		if value == "1" {
			file.Preferences.AllowedSenders = append(file.Preferences.AllowedSenders, strings.TrimPrefix(key, storage.RemoteContentSenderSettingPrefix))
		}
	}
	sort.Strings(file.Preferences.AllowedSenders)

	if includeContacts {
		contacts, err := profileLocalContacts(ctx, db, owner)
		if err != nil {
			return nil, err
		}
		if len(contacts) > 0 {
			data, err := renderVCard4(contacts)
			if err != nil {
				return nil, fmt.Errorf("render contacts: %w", err)
			}
			file.Contacts = string(data)
		}
	}
	return file, nil
}

// profileAccountReplacer swaps account IDs for their placeholders. Longer IDs
// go first so an ID that starts another one cannot match inside it.
func profileAccountReplacer(refs map[string]string) *strings.Replacer {
	ids := make([]string, 0, len(refs))
	for id := range refs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	pairs := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		pairs = append(pairs, id, refs[id])
	}
	return strings.NewReplacer(pairs...)
}

func exportProfileAccount(ctx context.Context, store *config.AccountStore, db *storage.DB, owner string, account models.Account, ref string, signatureRefs map[string]string) (profileAccount, error) {
	data, err := store.GetEditData(ctx, account.ID)
	if err != nil {
		return profileAccount{}, err
	}
	exported := profileAccount{
		Ref:               ref,
		Provider:          data.Provider,
		ProviderAccountID: data.ProviderAccountID,
		EmailAddress:      data.EmailAddress,
		DisplayName:       data.DisplayName,
		Color:             account.Color,
		AuthMethod:        data.AuthMethod,
		IMAPHost:          data.IMAPHost,
		IMAPPort:          data.IMAPPort,
		IMAPTLSMode:       data.IMAPTLSMode,
		SMTPHost:          data.SMTPHost,
		SMTPPort:          data.SMTPPort,
		SMTPTLSMode:       data.SMTPTLSMode,
		Username:          data.Username,
		SMTPUsername:      data.SmtpUsername,
		JMAPSessionURL:    data.JMAPSessionURL,
		EmailSync:         data.EmailSyncEnabled,
	}
	push, err := db.ProviderPushEnabled(ctx, account.ID)
	if err != nil {
		return profileAccount{}, err
	}
	exported.ScheduledChecks = !push

	contacts := data.ContactSync
	if contacts.Enabled || strings.TrimSpace(contacts.BaseURL) != "" {
		exported.Contacts = &profileContactSync{Enabled: contacts.Enabled, BaseURL: contacts.BaseURL, Username: contacts.Username}
		if contacts.Provider == providers.ProviderJMAP {
			exported.Contacts = &profileContactSync{Enabled: contacts.Enabled, Protocol: providers.ProviderJMAP}
		}
		for _, book := range contacts.AddressBooks {
			exported.Contacts.AddressBooks = append(exported.Contacts.AddressBooks, profileAddressBook{URL: book.URL, Name: book.Name, Default: book.Default})
		}
	}

	calendar := data.CalendarSync
	sources, err := db.ListCalendarSourcesForAccount(ctx, owner, account.ID)
	if err != nil {
		return profileAccount{}, err
	}
	if strings.TrimSpace(calendar.CalDAVBaseURL) != "" || len(sources) > 0 {
		exported.Calendar = &profileCalendarSync{CalDAVBaseURL: calendar.CalDAVBaseURL, CalDAVUsername: calendar.CalDAVUsername, CalDAVUseAccountCredentials: calendar.CalDAVUseAccountCredentials}
		if calendar.Protocol == storage.CalendarSourceProviderJMAP {
			exported.Calendar = &profileCalendarSync{Protocol: storage.CalendarSourceProviderJMAP}
		}
		for _, source := range sources {
			exported.Calendar.Calendars = append(exported.Calendar.Calendars, profileCalendar{
				RemoteID: source.RemoteID, Name: source.Name, Description: source.Description, TimeZone: source.TimeZone,
				Color: source.Color, AccessRole: source.AccessRole, Primary: source.IsPrimary, Selected: source.IsSelected, Hidden: source.IsHidden,
			})
		}
	}

	identities, err := db.ListAccountIdentities(ctx, owner, account.ID)
	if err != nil {
		return profileAccount{}, fmt.Errorf("list identities: %w", err)
	}
	for _, identity := range identities {
		if identity.Source == models.IdentitySourceManual {
			item := profileIdentity{Name: identity.Name, Email: identity.Email, ReplyTo: identity.ReplyTo, Signature: signatureRefs[identity.SignatureID]}
			if identity.HasOwnSMTP() {
				item.SMTP = &profileIdentitySMTP{Host: identity.SMTPHost, Port: identity.SMTPPort, TLSMode: identity.SMTPTLSMode, Username: identity.SMTPUsername}
			}
			exported.Identities = append(exported.Identities, item)
		}
	}

	settings := data.SignatureSettings
	uses := profileAccountSignatures{
		New:              profileSignatureUse{Signature: signatureRefs[settings.NewSignatureID], Enabled: settings.NewEnabled},
		Reply:            profileSignatureUse{Signature: signatureRefs[settings.ReplySignatureID], Enabled: settings.ReplyEnabled},
		Forward:          profileSignatureUse{Signature: signatureRefs[settings.ForwardSignatureID], Enabled: settings.ForwardEnabled},
		ReplyPlacement:   settings.ReplyPlacement,
		ForwardPlacement: settings.ForwardPlacement,
	}
	if uses.New != (profileSignatureUse{}) || uses.Reply != (profileSignatureUse{}) || uses.Forward != (profileSignatureUse{}) {
		exported.Signatures = &uses
	}
	return exported, nil
}

// profileLocalContacts returns the contacts that exist only in Gofer. Synced
// contacts come back from their provider, and contacts collected from mail
// are collected again.
func profileLocalContacts(ctx context.Context, db *storage.DB, owner string) ([]models.Contact, error) {
	listed, err := db.ListContactsForExport(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	var contacts []models.Contact
	for _, row := range listed {
		if !row.IsManual || row.IsDeleted || len(row.SourceBooks) > 0 {
			continue
		}
		contact, err := db.GetContact(ctx, owner, row.ID)
		if err != nil {
			return nil, fmt.Errorf("load contact: %w", err)
		}
		if contact != nil {
			contacts = append(contacts, *contact)
		}
	}
	return contacts, nil
}

// parseProfile reads and checks an exported profile.
func parseProfile(data []byte) (*profileFile, error) {
	var file profileFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, errors.New("This file is not a Gofer profile.")
	}
	if file.Format != profileFormat {
		return nil, errors.New("This file is not a Gofer profile.")
	}
	if file.Version < 1 {
		return nil, errors.New("This profile file is damaged.")
	}
	if file.Version > profileVersion {
		return nil, errors.New("This profile was exported by a newer version of Gofer. Update Gofer to import it.")
	}
	refs := make(map[string]bool, len(file.Accounts))
	for _, account := range file.Accounts {
		if !profileAccountRefPattern.MatchString(account.Ref) || profileAccountRefPattern.FindString(account.Ref) != account.Ref || refs[account.Ref] {
			return nil, errors.New("This profile file is damaged.")
		}
		refs[account.Ref] = true
		// An account with no mail goes by its login instead of an address.
		if strings.TrimSpace(account.EmailAddress) == "" && (account.Provider != providers.ProviderDAV || strings.TrimSpace(account.Username) == "") {
			return nil, errors.New("This profile file is damaged.")
		}
	}
	return &file, nil
}

// profileImportStatus says what importing an account would do.
type profileImportStatus string

const (
	profileImportNew         profileImportStatus = "new"
	profileImportExists      profileImportStatus = "exists"
	profileImportUnsupported profileImportStatus = "unsupported"
)

func (h *Handler) profileProviderAvailable(provider string) bool {
	switch provider {
	case providers.ProviderIMAP, providers.ProviderDAV:
		return true
	case providers.ProviderJMAP:
		return providers.JMAPEnabled()
	case providers.ProviderGmail:
		if h.userAccounts != nil {
			return h.userCredentials != nil && h.userCredentials.HasGoogleOAuth()
		}
		return h.auth != nil && h.mailCredentials() != nil && h.mailCredentials().HasGoogleOAuth()
	case providers.ProviderOutlook:
		if h.userAccounts != nil {
			return h.userCredentials != nil && h.userCredentials.HasMicrosoftOAuth()
		}
		return h.auth != nil && h.mailCredentials() != nil && h.mailCredentials().HasMicrosoftOAuth()
	}
	return false
}

type profileExistingAccount struct {
	ID       string
	Provider string
}

// profileExistingAccounts maps the email addresses of the user's accounts,
// in lower case, to the accounts; an account with no mail goes by its login
// on its server.
func (h *Handler) profileExistingAccounts(ctx context.Context, owner string) (map[string]profileExistingAccount, error) {
	existing := map[string]profileExistingAccount{}
	err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		accounts, err := db.GetAccountsIncludingDeleting(ctx, owner)
		if err != nil {
			return err
		}
		for _, account := range accounts {
			key := strings.ToLower(strings.TrimSpace(account.Email))
			if account.Provider == providers.ProviderDAV {
				key = profileDAVKey(account.DAVLogin, account.DAVServer)
			}
			if !account.IsDeleting && key != "" {
				existing[key] = profileExistingAccount{ID: account.ID, Provider: account.Provider}
			}
		}
		return nil
	})
	return existing, err
}

// profileAccountKey is what an account of a profile is matched by: its email
// address, or for an account with no mail its login on its server.
func profileAccountKey(account profileAccount) string {
	if account.Provider != providers.ProviderDAV {
		return strings.ToLower(strings.TrimSpace(account.EmailAddress))
	}
	server := ""
	if account.Calendar != nil {
		server = account.Calendar.CalDAVBaseURL
	}
	if strings.TrimSpace(server) == "" && account.Contacts != nil {
		server = account.Contacts.BaseURL
	}
	return profileDAVKey(account.Username, server)
}

func profileDAVKey(username, server string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	parsed, err := url.Parse(strings.TrimSpace(server))
	if username == "" || err != nil || parsed.Hostname() == "" {
		return ""
	}
	return "dav:" + username + "@" + strings.ToLower(parsed.Hostname())
}

// profileAccountName names an account of a profile in what an import says:
// its address, or the name of an account with no mail.
func profileAccountName(account profileAccount) string {
	if address := strings.TrimSpace(account.EmailAddress); address != "" {
		return address
	}
	return strings.TrimSpace(account.DisplayName)
}

// profileAccountPlan is what importing one account of a profile can do.
type profileAccountPlan struct {
	Status     profileImportStatus
	ExistingID string
	// CanReplace is set when the mailbox is already connected the same way,
	// so the profile's settings can replace its settings.
	CanReplace bool
}

func (h *Handler) profileAccountPlans(file *profileFile, existing map[string]profileExistingAccount) map[string]profileAccountPlan {
	plans := make(map[string]profileAccountPlan, len(file.Accounts))
	for _, account := range file.Accounts {
		current, connected := existing[profileAccountKey(account)]
		switch {
		case connected:
			canReplace := current.Provider == account.Provider && !(account.Provider == providers.ProviderJMAP && strings.TrimSpace(account.JMAPSessionURL) == "")
			plans[account.Ref] = profileAccountPlan{Status: profileImportExists, ExistingID: current.ID, CanReplace: canReplace}
		case !h.profileProviderAvailable(account.Provider),
			// Profiles exported before JMAP support left out the server.
			account.Provider == providers.ProviderJMAP && strings.TrimSpace(account.JMAPSessionURL) == "":
			plans[account.Ref] = profileAccountPlan{Status: profileImportUnsupported}
		default:
			plans[account.Ref] = profileAccountPlan{Status: profileImportNew}
		}
	}
	return plans
}

// What to do with something in the profile that is already in Gofer.
const (
	profileConflictSkip     = "skip"
	profileConflictReplace  = "replace"
	profileConflictKeepBoth = "add"
)

// profileContactConflict is a contact in the profile that looks like one the
// user already has: it shares an email address with it, or has the same name.
type profileContactConflict struct {
	Index    int
	Imported models.Contact
	Existing models.Contact
	ByEmail  bool
}

// DefaultAction keeps both contacts when only their names match and the
// profile's contact has an email address of its own; otherwise it skips the
// profile's contact.
func (c profileContactConflict) DefaultAction() string {
	if !c.ByEmail && strings.TrimSpace(c.Imported.Email) != "" {
		return profileConflictKeepBoth
	}
	return profileConflictSkip
}

func profileContacts(file *profileFile) ([]models.Contact, error) {
	if strings.TrimSpace(file.Contacts) == "" {
		return nil, nil
	}
	return parseVCardContacts(strings.NewReader(file.Contacts), []string{"local"})
}

func findProfileContactConflicts(ctx context.Context, db *storage.DB, owner string, contacts []models.Contact) ([]profileContactConflict, error) {
	if len(contacts) == 0 {
		return nil, nil
	}
	listed, err := db.ListContactsForExport(ctx, owner)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]string, len(listed))
	for _, contact := range listed {
		if name := strings.ToLower(strings.TrimSpace(contact.Name)); name != "" {
			if _, seen := byName[name]; !seen {
				byName[name] = contact.ID
			}
		}
	}
	var conflicts []profileContactConflict
	for i, contact := range contacts {
		existingID, byEmail := "", false
		for _, email := range append([]string{contact.Email}, contact.AdditionalEmails...) {
			if strings.TrimSpace(email) == "" {
				continue
			}
			profile, err := db.FindContactProfileByIdentity(ctx, owner, "email", email)
			if err != nil {
				return nil, err
			}
			if profile != nil {
				existingID, byEmail = profile.ID, true
				break
			}
		}
		if existingID == "" {
			existingID = byName[strings.ToLower(strings.TrimSpace(contact.Name))]
		}
		if existingID == "" {
			continue
		}
		existing, err := db.GetContact(ctx, owner, existingID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			continue
		}
		conflicts = append(conflicts, profileContactConflict{Index: i, Imported: contact, Existing: *existing, ByEmail: byEmail})
	}
	return conflicts, nil
}

type profileImportOptions struct {
	Preferences bool
	Signatures  bool
	Contacts    bool
	// Accounts are the new accounts to add.
	Accounts map[string]bool
	// AccountActions say what to do with accounts already connected, by
	// reference. They are skipped unless the action is to replace them.
	AccountActions map[string]string
	// ContactActions say what to do with contacts that look like existing
	// ones, by their position in the profile.
	ContactActions map[int]string
}

type profileImportResult struct {
	Accounts         []string
	AccountsReplaced []string
	AccountFailures  []string
	Signatures       int
	Contacts         int
	ContactsReplaced int
	ContactsSkipped  int
	Preferences      bool
}

func (h *Handler) importProfile(ctx context.Context, owner string, file *profileFile, options profileImportOptions) (profileImportResult, error) {
	var result profileImportResult
	existing, err := h.profileExistingAccounts(ctx, owner)
	if err != nil {
		return result, err
	}
	plans := h.profileAccountPlans(file, existing)

	// Settings that name an account apply to the new account, or to the one
	// already connected with the same address.
	accountIDs := map[string]string{}
	for _, account := range file.Accounts {
		if plan := plans[account.Ref]; plan.ExistingID != "" {
			accountIDs[account.Ref] = plan.ExistingID
		}
	}

	signatureIDs := map[string]string{}
	if options.Signatures {
		if err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			added, err := importProfileSignatures(ctx, db, owner, file.Signatures, signatureIDs)
			result.Signatures = added
			return err
		}); err != nil {
			return result, fmt.Errorf("import signatures: %w", err)
		}
	}

	for _, account := range file.Accounts {
		plan := plans[account.Ref]
		switch {
		case plan.Status == profileImportNew && options.Accounts[account.Ref]:
			id, err := h.importProfileAccount(ctx, owner, account, signatureIDs)
			if err != nil {
				log.Printf("profile import: account %s: %v", profileAccountName(account), err)
				result.AccountFailures = append(result.AccountFailures, profileAccountName(account))
				continue
			}
			accountIDs[account.Ref] = id
			result.Accounts = append(result.Accounts, profileAccountName(account))
		case plan.CanReplace && options.AccountActions[account.Ref] == profileConflictReplace:
			if err := h.replaceProfileAccount(ctx, owner, plan.ExistingID, account, signatureIDs); err != nil {
				log.Printf("profile import: replace account %s: %v", profileAccountName(account), err)
				result.AccountFailures = append(result.AccountFailures, profileAccountName(account))
				continue
			}
			result.AccountsReplaced = append(result.AccountsReplaced, profileAccountName(account))
		}
	}

	if options.Preferences {
		if err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			return importProfilePreferences(ctx, db, owner, file.Preferences, accountIDs)
		}); err != nil {
			return result, fmt.Errorf("import preferences: %w", err)
		}
		result.Preferences = true
	}

	if options.Contacts {
		contacts, err := profileContacts(file)
		if err != nil {
			return result, fmt.Errorf("read contacts: %w", err)
		}
		if err := h.importProfileContacts(ctx, owner, contacts, options.ContactActions, &result); err != nil {
			return result, fmt.Errorf("import contacts: %w", err)
		}
	}
	return result, nil
}

// importProfileSignatures adds the profile's signatures. A signature that
// already exists with the same name and text is reused; another one with the
// same name gets a number after its name.
func importProfileSignatures(ctx context.Context, db *storage.DB, owner string, signatures []profileSignature, ids map[string]string) (int, error) {
	existing, err := db.ListSignatures(ctx, owner)
	if err != nil {
		return 0, err
	}
	names := make(map[string]bool, len(existing))
	for _, signature := range existing {
		names[strings.ToLower(strings.TrimSpace(signature.Name))] = true
	}
	added := 0
	for _, signature := range signatures {
		if strings.TrimSpace(signature.HTMLBody) == "" && strings.TrimSpace(signature.TextBody) == "" {
			continue
		}
		reused := false
		for _, current := range existing {
			if strings.EqualFold(strings.TrimSpace(current.Name), strings.TrimSpace(signature.Name)) && current.HTMLBody == signature.HTMLBody && current.TextBody == signature.TextBody {
				ids[signature.Ref], reused = current.ID, true
				break
			}
		}
		if reused {
			continue
		}
		name := strings.TrimSpace(signature.Name)
		if name == "" {
			name = "Signature"
		}
		for n := 2; names[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s (%d)", strings.TrimSpace(signature.Name), n)
		}
		saved, err := db.SaveSignature(ctx, owner, models.Signature{Name: name, HTMLBody: signature.HTMLBody, TextBody: signature.TextBody})
		if err != nil {
			return added, err
		}
		names[strings.ToLower(name)] = true
		ids[signature.Ref] = saved.ID
		added++
	}
	return added, nil
}

// profileCalendarProvider is the provider an account's calendars from a
// profile are read with: a mail-server account's come from CalDAV, or for a
// JMAP account from JMAP when the profile says so.
func profileCalendarProvider(account profileAccount) string {
	if profileCalendarsOverJMAP(account) {
		return storage.CalendarSourceProviderJMAP
	}
	if account.Provider == providers.ProviderIMAP || account.Provider == providers.ProviderJMAP || account.Provider == providers.ProviderDAV {
		return storage.CalendarSourceProviderCalDAV
	}
	return account.Provider
}

func profileCalendarsOverJMAP(account profileAccount) bool {
	return account.Provider == providers.ProviderJMAP && account.Calendar != nil && account.Calendar.Protocol == storage.CalendarSourceProviderJMAP
}

// importProfileAccount creates an account from the profile with every service
// off, and records what to turn on once it is reconnected.
func (h *Handler) importProfileAccount(ctx context.Context, owner string, account profileAccount, signatureIDs map[string]string) (string, error) {
	req := models.CreateAccountRequest{
		Provider:          account.Provider,
		ProviderAccountID: account.ProviderAccountID,
		EmailAddress:      strings.TrimSpace(account.EmailAddress),
		DisplayName:       strings.TrimSpace(account.DisplayName),
		IMAPHost:          account.IMAPHost,
		IMAPPort:          account.IMAPPort,
		IMAPTLSMode:       account.IMAPTLSMode,
		SMTPHost:          account.SMTPHost,
		SMTPPort:          account.SMTPPort,
		SMTPTLSMode:       account.SMTPTLSMode,
		Username:          account.Username,
		AuthMethod:        account.AuthMethod,
		SmtpUsername:      account.SMTPUsername,
		JMAPSessionURL:    account.JMAPSessionURL,
		EmailSyncDisabled: true,
	}
	if req.Provider == providers.ProviderIMAP || req.Provider == providers.ProviderJMAP || req.Provider == providers.ProviderDAV {
		req.AuthMethod = "plain"
	} else {
		req.AuthMethod = "oauth2"
	}

	var id string
	if h.userAccounts != nil {
		created, err := h.userAccounts.CreateAccount(ctx, owner, &req)
		if err != nil {
			return "", err
		}
		id = created.ID
	} else {
		if err := h.cleanupDeletingAccountForCreate(ctx, owner, req.EmailAddress); err != nil {
			return "", err
		}
		created, err := h.accountStore.CreateAccount(ctx, owner, &req)
		if err != nil {
			return "", err
		}
		id = created.ID
	}

	pending := profileImportPending(account)
	err := h.withProfileAccount(ctx, owner, id, func(store *config.AccountStore, db *storage.DB) error {
		if color, ok := normalizeAccountColor(account.Color); ok {
			if err := store.UpdateAccountColor(ctx, owner, id, color); err != nil {
				return err
			}
		}
		if err := db.SetProviderPushEnabled(ctx, owner, id, !account.ScheduledChecks); err != nil {
			return err
		}
		if err := importProfileAccountServices(ctx, store, db, owner, id, account); err != nil {
			return err
		}
		if account.Signatures != nil {
			if err := db.SaveAccountSignatureSettings(ctx, owner, profileSignatureSettings(id, account.Signatures, signatureIDs)); err != nil {
				return err
			}
		}
		if err := importProfileIdentities(ctx, db, owner, id, account.Identities, signatureIDs); err != nil {
			return err
		}
		return db.SetImportedAccountPending(ctx, owner, id, pending)
	})
	if err != nil {
		return id, err
	}
	if h.userAccounts != nil {
		if err := h.userStorage.WithAccountActivityForUser(ctx, owner, id, func() error {
			return h.userAccountHooks.Created(ctx, id)
		}); err != nil {
			log.Printf("profile import: account %s created but its worker did not start: %v", id, err)
		}
	}
	return id, nil
}

// profileImportPending works out which logins an imported account needs
// before each of its services can sync again. Gmail and Outlook need a new
// sign-in for everything. Other accounts need their mail password, and a
// contacts or calendar password of their own when those services had one.
func profileImportPending(account profileAccount) storage.ImportedAccountPending {
	pending := storage.ImportedAccountPending{Email: account.EmailSync, Contacts: account.Contacts != nil && account.Contacts.Enabled}
	if calendar := account.Calendar; calendar != nil {
		for _, source := range calendar.Calendars {
			if source.Selected {
				pending.Calendars = append(pending.Calendars, source.RemoteID)
			}
		}
	}
	if account.Provider == providers.ProviderGmail || account.Provider == providers.ProviderOutlook {
		pending.EmailLogin = storage.ImportedLoginSignIn
		pending.Needs = []string{storage.ImportedLoginSignIn}
		return pending
	}
	pending.Needs = []string{storage.ImportedLoginMail}
	username := strings.TrimSpace(account.Username)
	if smtp := strings.TrimSpace(account.SMTPUsername); smtp != "" && smtp != username {
		pending.SMTPSeparate = true
	}
	if pending.Contacts {
		if contactsUser := strings.TrimSpace(account.Contacts.Username); contactsUser != "" && contactsUser != username {
			pending.ContactsLogin = storage.ImportedLoginContacts
			pending.Needs = append(pending.Needs, storage.ImportedLoginContacts)
		}
	}
	if len(pending.Calendars) > 0 && !profileCalendarsOverJMAP(account) && !account.Calendar.CalDAVUseAccountCredentials && strings.TrimSpace(account.Calendar.CalDAVBaseURL) != "" {
		pending.CalendarLogin = storage.ImportedLoginCalendar
		pending.Needs = append(pending.Needs, storage.ImportedLoginCalendar)
	}
	return pending
}

// profileContactConfig is the contact setup a profile gives a mail-server
// account: over its JMAP connection, or CardDAV. ok is false when it has none.
func profileContactConfig(account profileAccount, enabled bool) (cfg models.ContactSyncConfig, ok bool) {
	contacts := account.Contacts
	if contacts == nil {
		return cfg, false
	}
	switch {
	case contacts.Protocol == providers.ProviderJMAP:
		if account.Provider != providers.ProviderJMAP || len(contacts.AddressBooks) == 0 {
			return cfg, false
		}
		cfg = models.ContactSyncConfig{Provider: providers.ProviderJMAP, Enabled: enabled}
	case strings.TrimSpace(contacts.BaseURL) != "":
		cfg = models.ContactSyncConfig{Provider: providers.ProviderCardDAV, Enabled: enabled, BaseURL: contacts.BaseURL, Username: contacts.Username}
	default:
		return cfg, false
	}
	for _, book := range contacts.AddressBooks {
		cfg.AddressBooks = append(cfg.AddressBooks, models.ContactAddressBook{URL: book.URL, Name: book.Name, Default: book.Default, Selected: true})
	}
	return cfg, true
}

func importProfileAccountServices(ctx context.Context, store *config.AccountStore, db *storage.DB, owner, id string, account profileAccount) error {
	builtin := account.Provider == providers.ProviderGmail || account.Provider == providers.ProviderOutlook
	if contacts := account.Contacts; contacts != nil {
		if builtin {
			if err := store.SetContactSyncEnabled(ctx, owner, id, false); err != nil {
				return err
			}
		} else if cfg, ok := profileContactConfig(account, false); ok {
			if err := store.SaveContactSyncConfig(ctx, owner, id, cfg, ""); err != nil {
				return err
			}
		}
	} else if builtin {
		// Gmail and Outlook sync contacts unless told not to.
		if err := store.SetContactSyncEnabled(ctx, owner, id, false); err != nil {
			return err
		}
	}

	calendar := account.Calendar
	if calendar == nil {
		return nil
	}
	if (account.Provider == providers.ProviderIMAP || account.Provider == providers.ProviderJMAP || account.Provider == providers.ProviderDAV) && !profileCalendarsOverJMAP(account) && strings.TrimSpace(calendar.CalDAVBaseURL) != "" {
		if err := store.SaveCalDAVConfig(ctx, owner, id, calendar.CalDAVBaseURL, calendar.CalDAVUsername, "", calendar.CalDAVUseAccountCredentials); err != nil {
			return err
		}
	}
	sources := make([]storage.CalendarSource, 0, len(calendar.Calendars))
	for _, source := range calendar.Calendars {
		sources = append(sources, storage.CalendarSource{
			RemoteID: source.RemoteID, Name: source.Name, Description: source.Description, TimeZone: source.TimeZone,
			Color: source.Color, AccessRole: source.AccessRole, IsPrimary: source.Primary, IsHidden: source.Hidden,
		})
	}
	return db.AddImportedCalendarSources(ctx, owner, id, profileCalendarProvider(account), sources)
}

// resolveProfileRefs puts account IDs back in place of their placeholders.
// ok is false when the text names an account that was not imported.
func resolveProfileRefs(text string, accountIDs map[string]string) (string, bool) {
	ok := true
	resolved := profileAccountRefPattern.ReplaceAllStringFunc(text, func(ref string) string {
		if id := accountIDs[ref]; id != "" {
			return id
		}
		ok = false
		return ref
	})
	return resolved, ok
}

// resolveProfileJSONObject resolves the placeholders in a setting that holds
// a JSON object, dropping entries for accounts that were not imported. ok is
// false when value is not a JSON object.
func resolveProfileJSONObject(value string, accountIDs map[string]string) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &object); err != nil || object == nil {
		return nil, false
	}
	resolved := make(map[string]json.RawMessage, len(object))
	for key, entry := range object {
		resolvedKey, keyOK := resolveProfileRefs(key, accountIDs)
		resolvedEntry, entryOK := resolveProfileRefs(string(entry), accountIDs)
		if keyOK && entryOK {
			resolved[resolvedKey] = json.RawMessage(resolvedEntry)
		}
	}
	return resolved, true
}

// mergeProfileSetting combines an imported setting with the current one. JSON
// objects, such as which sidebar groups are collapsed, keep the current
// entries the profile does not mention. ok is false when the setting names an
// account that was not imported.
func mergeProfileSetting(current, imported string, accountIDs map[string]string) (string, bool) {
	if object, isObject := resolveProfileJSONObject(imported, accountIDs); isObject {
		merged := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(current), &merged); err != nil || merged == nil {
			merged = map[string]json.RawMessage{}
		}
		for key, entry := range object {
			merged[key] = entry
		}
		encoded, err := json.Marshal(merged)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
	return resolveProfileRefs(imported, accountIDs)
}

func importProfilePreferences(ctx context.Context, db *storage.DB, owner string, preferences profilePreferences, accountIDs map[string]string) error {
	if len(preferences.UI) > 0 {
		current := map[string]string{}
		if raw, err := db.GetSetting(ctx, owner, "ui_settings"); err != nil {
			return err
		} else if strings.TrimSpace(raw) != "" {
			_ = json.Unmarshal([]byte(raw), &current)
		}
		updates := make(map[string]string, len(preferences.UI))
		for key, value := range preferences.UI {
			resolvedKey, ok := resolveProfileRefs(key, accountIDs)
			if !ok {
				continue
			}
			if merged, ok := mergeProfileSetting(current[resolvedKey], value, accountIDs); ok {
				updates[resolvedKey] = merged
			}
		}
		if err := db.MergeUISettings(ctx, owner, updates); err != nil {
			return err
		}
	}

	settings := map[string]string{}
	if interval, err := strconv.Atoi(strings.TrimSpace(preferences.SyncIntervalMinutes)); err == nil && interval >= 1 {
		settings["sync_interval_minutes"] = strconv.Itoa(interval)
	}
	if idle := strings.TrimSpace(preferences.IdleFolders); idle != "" {
		current, err := db.GetSetting(ctx, owner, "idle_folders")
		if err != nil {
			return err
		}
		if _, isObject := resolveProfileJSONObject(idle, accountIDs); isObject {
			if merged, ok := mergeProfileSetting(current, idle, accountIDs); ok {
				settings["idle_folders"] = merged
			}
		} else if strings.TrimSpace(current) == "" {
			// Older installations keep one list of folder roles for every
			// account.
			if resolved, ok := resolveProfileRefs(idle, accountIDs); ok {
				settings["idle_folders"] = resolved
			}
		}
	}
	for _, sender := range preferences.AllowedSenders {
		if isHexDigest(sender) {
			settings[storage.RemoteContentSenderSettingPrefix+sender] = "1"
		}
	}
	return db.SetSettings(ctx, owner, settings)
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// importProfileContacts adds the profile's contacts to Gofer's local
// contacts. A contact that looks like an existing one is handled as its
// action says: skipped, replacing the existing contact's details while
// keeping where it syncs, or added beside it.
func (h *Handler) importProfileContacts(ctx context.Context, owner string, contacts []models.Contact, actions map[int]string, result *profileImportResult) error {
	if len(contacts) == 0 {
		return nil
	}
	var conflicts []profileContactConflict
	if err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		conflicts, err = findProfileContactConflicts(ctx, db, owner, contacts)
		return err
	}); err != nil {
		return err
	}
	byIndex := make(map[int]profileContactConflict, len(conflicts))
	for _, conflict := range conflicts {
		byIndex[conflict.Index] = conflict
	}

	type pendingContact struct {
		contact  models.Contact
		previous *models.Contact
	}
	var pending []pendingContact
	for i, contact := range contacts {
		conflict, found := byIndex[i]
		if !found {
			pending = append(pending, pendingContact{contact: contact})
			continue
		}
		action := actions[i]
		if action == "" {
			action = conflict.DefaultAction()
		}
		switch {
		case action == profileConflictReplace:
			previous := conflict.Existing
			contact.ID = previous.ID
			if len(previous.SaveTargets) > 0 {
				contact.SaveTargets = previous.SaveTargets
			}
			contact.GoferSyncEnabled = previous.GoferSyncEnabled
			pending = append(pending, pendingContact{contact: contact, previous: &previous})
		case action == profileConflictKeepBoth && !conflict.ByEmail:
			pending = append(pending, pendingContact{contact: contact})
		default:
			result.ContactsSkipped++
		}
	}

	count := func(item pendingContact) {
		if item.previous != nil {
			result.ContactsReplaced++
		} else {
			result.Contacts++
		}
	}
	if h.userAccounts != nil {
		return h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
			queued := false
			for _, item := range pending {
				snapshot, err := h.userAccounts.SnapshotContactEdit(ctx, owner, item.contact)
				if err != nil {
					return err
				}
				saved, err := h.userAccounts.SaveContactEdit(ctx, snapshot, false)
				if err != nil {
					return err
				}
				queued = queued || saved.OperationID != ""
				count(item)
			}
			if queued {
				if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
					log.Printf("profile import: wake contact sync: %v", err)
				}
			}
			return nil
		})
	}
	for _, item := range pending {
		saved, err := h.db.SaveContact(ctx, owner, item.contact)
		if err != nil {
			return err
		}
		if item.previous != nil {
			h.scheduleContactAccountSync(ctx, owner, saved, item.previous)
		}
		count(item)
	}
	return nil
}

// resolveImportedLogin records that an account imported from a profile has
// one of the logins it was waiting for, and turns on the services that login
// lets sync. It returns the logins still missing; waiting is false when the
// account was not waiting for that login. The caller restarts the account's
// workers.
func (h *Handler) resolveImportedLogin(ctx context.Context, owner, accountID, login string) (remaining []string, waiting bool, err error) {
	var services storage.ImportedServices
	var sourceIDs []string
	err = h.withProfileAccount(ctx, owner, accountID, func(store *config.AccountStore, db *storage.DB) error {
		var err error
		services, remaining, waiting, err = db.ResolveImportedAccountLogin(ctx, owner, accountID, login)
		if err != nil || !waiting {
			return err
		}
		if services.Email {
			if err := store.SetEmailSyncEnabled(ctx, owner, accountID, true); err != nil {
				return err
			}
		}
		if services.Contacts {
			if err := store.SetContactSyncEnabled(ctx, owner, accountID, true); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if len(services.Calendars) == 0 {
			return nil
		}
		sources, err := db.ListCalendarSourcesForAccount(ctx, owner, accountID)
		if err != nil {
			return err
		}
		selected := make(map[string]bool, len(services.Calendars))
		for _, remoteID := range services.Calendars {
			selected[remoteID] = true
		}
		for _, source := range sources {
			if selected[source.RemoteID] {
				sourceIDs = append(sourceIDs, source.ID)
			}
		}
		if len(sourceIDs) == 0 || h.userAccounts != nil {
			return nil
		}
		return db.SetCalendarSourceSelection(ctx, owner, accountID, sourceIDs)
	})
	if err != nil || !waiting {
		return remaining, waiting, err
	}
	if h.userAccounts != nil && len(sourceIDs) > 0 {
		err = h.userAccounts.SetCalendarSourcesSelected(ctx, owner, accountID, sourceIDs)
	}
	return remaining, waiting, err
}

// resolveImportedMailLogin clears an imported account's mail mark when a save
// brings its mail password, and its sending password too when sending had a
// login of its own.
func (h *Handler) resolveImportedMailLogin(w http.ResponseWriter, ctx context.Context, owner, accountID string, req *models.CreateAccountRequest) error {
	if req.Password == "" {
		return nil
	}
	var pending storage.ImportedAccountPending
	var found bool
	if err := h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		all, err := db.ImportedAccountsPending(ctx, owner)
		pending, found = all[accountID]
		return err
	}); err != nil || !found {
		return err
	}
	if pending.SMTPSeparate && req.SmtpPassword == "" {
		return nil
	}
	remaining, waiting, err := h.resolveImportedLogin(ctx, owner, accountID, storage.ImportedLoginMail)
	if err == nil && waiting {
		markImportedLoginResolved(w, storage.ImportedLoginMail, remaining)
	}
	return err
}

// markImportedLoginResolved tells the edit dialog which of its marks a save
// cleared, and which logins are still missing.
func markImportedLoginResolved(w http.ResponseWriter, login string, remaining []string) {
	w.Header().Set("X-Gofer-Reconnect-Resolved", login)
	w.Header().Set("X-Gofer-Reconnect-Remaining", strings.Join(remaining, ","))
}

// replaceProfileAccount gives an account that is already connected the
// profile's settings: name, color, servers, contacts, calendars, signatures
// and which services are on. It keeps the account's password or sign-in, its
// mail and its identity.
func (h *Handler) replaceProfileAccount(ctx context.Context, owner, id string, account profileAccount, signatureIDs map[string]string) error {
	builtin := account.Provider == providers.ProviderGmail || account.Provider == providers.ProviderOutlook
	name := strings.TrimSpace(account.DisplayName)
	if builtin {
		if name != "" {
			if h.userAccounts != nil {
				data, err := h.userEditData(ctx, owner, id)
				if err != nil {
					return err
				}
				req := models.CreateAccountRequest{Provider: data.Provider, ProviderAccountID: data.ProviderAccountID, EmailAddress: data.EmailAddress, DisplayName: name, AuthMethod: "oauth2"}
				if err := h.userAccounts.UpdateOAuthMetadata(ctx, owner, id, &req); err != nil {
					return err
				}
			} else if err := h.accountStore.UpdateAccount(ctx, id, &models.CreateAccountRequest{DisplayName: name}); err != nil {
				return err
			}
		}
	} else {
		req := models.CreateAccountRequest{
			Provider: providers.ProviderIMAP, DisplayName: name, AuthMethod: "plain",
			IMAPHost: account.IMAPHost, IMAPPort: account.IMAPPort, IMAPTLSMode: account.IMAPTLSMode,
			SMTPHost: account.SMTPHost, SMTPPort: account.SMTPPort, SMTPTLSMode: account.SMTPTLSMode,
			Username: account.Username, SmtpUsername: account.SMTPUsername,
		}
		if account.Provider == providers.ProviderJMAP {
			req = models.CreateAccountRequest{Provider: providers.ProviderJMAP, DisplayName: name, AuthMethod: "plain",
				JMAPSessionURL: account.JMAPSessionURL, Username: account.Username}
		}
		if account.Provider == providers.ProviderDAV {
			req = models.CreateAccountRequest{Provider: providers.ProviderDAV, DisplayName: name, AuthMethod: "plain", Username: account.Username}
		}
		var err error
		if h.userAccounts != nil {
			err = h.userAccounts.UpdateAccount(ctx, owner, id, &req)
		} else {
			err = h.accountStore.UpdateAccount(ctx, id, &req)
		}
		if err != nil {
			return err
		}
	}

	// An account still waiting to be reconnected keeps its services off and
	// remembers the profile's choices for when it is.
	waiting := false
	if err := h.withProfileAccount(ctx, owner, id, func(store *config.AccountStore, db *storage.DB) error {
		pending, err := db.ImportedAccountsPending(ctx, owner)
		if err != nil {
			return err
		}
		_, waiting = pending[id]
		if color, ok := normalizeAccountColor(account.Color); ok {
			if err := store.UpdateAccountColor(ctx, owner, id, color); err != nil {
				return err
			}
		}
		if err := db.SaveAccountSignatureSettings(ctx, owner, profileSignatureSettings(id, account.Signatures, signatureIDs)); err != nil {
			return err
		}
		if err := importProfileIdentities(ctx, db, owner, id, account.Identities, signatureIDs); err != nil {
			return err
		}
		if err := db.SetProviderPushEnabled(ctx, owner, id, !account.ScheduledChecks); err != nil {
			return err
		}
		if waiting {
			if err := importProfileAccountServices(ctx, store, db, owner, id, account); err != nil {
				return err
			}
			return db.SetImportedAccountPending(ctx, owner, id, profileImportPending(account))
		}
		return store.SetEmailSyncEnabled(ctx, owner, id, account.EmailSync)
	}); err != nil {
		return err
	}
	if !waiting {
		if err := h.replaceProfileAccountServices(ctx, owner, id, account); err != nil {
			return err
		}
	}

	if h.userAccounts != nil {
		return h.userStorage.WithAccountActivityForUser(ctx, owner, id, func() error {
			return h.userAccountHooks.Updated(ctx, id)
		})
	}
	h.closeBodyClient(id)
	if h.syncer == nil {
		return nil
	}
	if account.EmailSync && !waiting {
		h.syncer.RestartAccount(id)
	} else {
		h.syncer.StopAccount(id)
	}
	return nil
}

// importProfileIdentities adds a profile's identities to an account. Ones the
// account already has with the same name and address are left as they are,
// and ones that are not valid addresses, or that the account's provider
// would not send as, are skipped.
func importProfileIdentities(ctx context.Context, db *storage.DB, owner, accountID string, identities []profileIdentity, signatureIDs map[string]string) error {
	if len(identities) == 0 {
		return nil
	}
	existing, err := db.ListAccountIdentities(ctx, owner, accountID)
	if err != nil {
		return err
	}
	for _, imported := range identities {
		if slices.ContainsFunc(existing, func(identity models.Identity) bool {
			return strings.EqualFold(identity.Email, strings.TrimSpace(imported.Email)) && identity.Name == strings.TrimSpace(imported.Name)
		}) {
			continue
		}
		if _, _, err := storage.NormalizeIdentityAddresses(imported.Email, imported.ReplyTo); err != nil {
			log.Printf("profile import: skipping identity of account %s: %v", accountID, err)
			continue
		}
		identity := models.Identity{AccountID: accountID, Name: imported.Name, Email: imported.Email, ReplyTo: imported.ReplyTo, SignatureID: signatureIDs[imported.Signature]}
		if imported.SMTP != nil {
			identity.SMTPHost, identity.SMTPPort, identity.SMTPTLSMode, identity.SMTPUsername = imported.SMTP.Host, imported.SMTP.Port, imported.SMTP.TLSMode, imported.SMTP.Username
			if err := storage.NormalizeIdentitySMTP(&identity); err != nil {
				log.Printf("profile import: skipping identity of account %s: %v", accountID, err)
				continue
			}
		}
		saved, err := db.SaveAccountIdentityGuarded(ctx, owner, identity, nil)
		if errors.Is(err, storage.ErrIdentityAddressNotAllowed) || errors.Is(err, storage.ErrIdentitySMTPNotSupported) {
			log.Printf("profile import: skipping identity of account %s: it can't send as %s", accountID, imported.Email)
			continue
		}
		if err != nil {
			return fmt.Errorf("import identity: %w", err)
		}
		existing = append(existing, saved)
	}
	return nil
}

func profileSignatureSettings(id string, uses *profileAccountSignatures, signatureIDs map[string]string) models.AccountSignatureSettings {
	settings := models.AccountSignatureSettings{AccountID: id, ReplyPlacement: "before", ForwardPlacement: "before"}
	if uses == nil {
		return settings
	}
	settings.NewSignatureID = signatureIDs[uses.New.Signature]
	settings.ReplySignatureID = signatureIDs[uses.Reply.Signature]
	settings.ForwardSignatureID = signatureIDs[uses.Forward.Signature]
	settings.NewEnabled = uses.New.Enabled && settings.NewSignatureID != ""
	settings.ReplyEnabled = uses.Reply.Enabled && settings.ReplySignatureID != ""
	settings.ForwardEnabled = uses.Forward.Enabled && settings.ForwardSignatureID != ""
	if uses.ReplyPlacement != "" {
		settings.ReplyPlacement = uses.ReplyPlacement
	}
	if uses.ForwardPlacement != "" {
		settings.ForwardPlacement = uses.ForwardPlacement
	}
	return settings
}

// replaceProfileAccountServices applies the profile's contact and calendar
// setup to a connected account, keeping any passwords it already has.
func (h *Handler) replaceProfileAccountServices(ctx context.Context, owner, id string, account profileAccount) error {
	builtin := account.Provider == providers.ProviderGmail || account.Provider == providers.ProviderOutlook
	contacts := account.Contacts
	contactsOn := contacts != nil && contacts.Enabled
	if cfg, ok := profileContactConfig(account, contactsOn); !builtin && ok {
		if h.userAccounts != nil {
			snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
			if err != nil {
				return err
			}
			if err := h.userAccounts.SaveContactServices(ctx, snapshot, cfg, ""); err != nil {
				return err
			}
		} else if err := h.accountStore.SaveContactSyncConfig(ctx, owner, id, cfg, ""); err != nil {
			return err
		}
	} else if h.userAccounts != nil {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		if err := h.userAccounts.SetContactServicesEnabled(ctx, snapshot, contactsOn); err != nil && !errors.Is(err, config.ErrContactServicesNotConfigured) {
			return err
		}
	} else if err := h.accountStore.SetContactSyncEnabled(ctx, owner, id, contactsOn); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	calendar := account.Calendar
	if calendar == nil {
		calendar = &profileCalendarSync{}
	}
	if !builtin && !profileCalendarsOverJMAP(account) && strings.TrimSpace(calendar.CalDAVBaseURL) != "" {
		if h.userAccounts != nil {
			snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
			if err != nil {
				return err
			}
			if err := h.userAccounts.SaveCalDAVServices(ctx, snapshot, calendar.CalDAVBaseURL, calendar.CalDAVUsername, "", calendar.CalDAVUseAccountCredentials); err != nil {
				return err
			}
		} else if err := h.accountStore.SaveCalDAVConfig(ctx, owner, id, calendar.CalDAVBaseURL, calendar.CalDAVUsername, "", calendar.CalDAVUseAccountCredentials); err != nil {
			return err
		}
	}

	wanted := make(map[string]profileCalendar, len(calendar.Calendars))
	for _, source := range calendar.Calendars {
		wanted[source.RemoteID] = source
	}
	var selected []string
	visibility := map[string]bool{}
	err := h.withProfileAccount(ctx, owner, id, func(_ *config.AccountStore, db *storage.DB) error {
		// Calendars the account has not found yet are added, so the
		// profile's choices apply to them.
		var missing []storage.CalendarSource
		sources, err := db.ListCalendarSourcesForAccount(ctx, owner, id)
		if err != nil {
			return err
		}
		known := make(map[string]bool, len(sources))
		for _, source := range sources {
			known[source.RemoteID] = true
		}
		for _, source := range calendar.Calendars {
			if !known[source.RemoteID] {
				missing = append(missing, storage.CalendarSource{RemoteID: source.RemoteID, Name: source.Name, Description: source.Description, TimeZone: source.TimeZone, Color: source.Color, AccessRole: source.AccessRole, IsPrimary: source.Primary, IsHidden: source.Hidden})
			}
		}
		if err := db.AddImportedCalendarSources(ctx, owner, id, profileCalendarProvider(account), missing); err != nil {
			return err
		}
		if sources, err = db.ListCalendarSourcesForAccount(ctx, owner, id); err != nil {
			return err
		}
		for _, source := range sources {
			profile, listed := wanted[source.RemoteID]
			if listed && profile.Selected {
				selected = append(selected, source.ID)
			}
			if listed && profile.Hidden != source.IsHidden {
				visibility[source.ID] = !profile.Hidden
			}
		}
		if h.userAccounts != nil {
			return nil
		}
		for sourceID, visible := range visibility {
			if err := db.SetCalendarSourceVisibility(ctx, owner, sourceID, visible); err != nil {
				return err
			}
		}
		return db.SetCalendarSourceSelection(ctx, owner, id, selected)
	})
	if err != nil || h.userAccounts == nil {
		return err
	}
	for sourceID, visible := range visibility {
		if err := h.userAccounts.SetCalendarSourceVisible(ctx, owner, sourceID, visible); err != nil {
			return err
		}
	}
	return h.userAccounts.SetCalendarSourcesSelected(ctx, owner, id, selected)
}
