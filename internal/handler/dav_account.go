package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// The one service a calendar-only or contacts-only account is for.
const (
	davServiceCalendar = "calendar"
	davServiceContacts = "contacts"
)

// davAccountForm is what the dialog of a calendar-only or contacts-only
// account sends: its server and login, and the calendars or address books
// ticked to sync.
type davAccountForm struct {
	service, name, serverURL, username, password string
	items                                        []string
	defaultItem                                  string
}

func parseDAVAccountForm(w http.ResponseWriter, r *http.Request, saved *models.EditAccountData) (davAccountForm, string) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || len(r.PostForm["item"]) > calendarDiscoveryMaxSources {
		return davAccountForm{}, "The account settings could not be read."
	}
	form := davAccountForm{
		service:     strings.TrimSpace(r.PostForm.Get("service")),
		name:        strings.TrimSpace(r.PostForm.Get("display_name")),
		serverURL:   strings.TrimSpace(r.PostForm.Get("server_url")),
		username:    strings.TrimSpace(r.PostForm.Get("username")),
		password:    r.PostForm.Get("password"),
		items:       r.PostForm["item"],
		defaultItem: strings.TrimSpace(r.PostForm.Get("default_item")),
	}
	if saved != nil {
		// An account keeps the service it was added for.
		form.service = davAccountService(*saved)
	}
	if form.service != davServiceCalendar && form.service != davServiceContacts {
		return form, "Choose calendars or contacts."
	}
	// An account imported from a profile has no password yet.
	needsPassword := saved == nil || saved.ReconnectPending
	if form.serverURL == "" || form.username == "" || (needsPassword && strings.TrimSpace(form.password) == "") {
		return form, "Enter the server URL, username and password."
	}
	if form.name == "" {
		form.name = davServerName(form.serverURL)
	}
	return form, ""
}

// davAccountService is the service an account with no mail is for: its
// calendars, once it has a CalDAV server, or else its contacts.
func davAccountService(data models.EditAccountData) string {
	if strings.TrimSpace(data.CalendarSync.CalDAVBaseURL) != "" {
		return davServiceCalendar
	}
	return davServiceContacts
}

// davServerName is the host of a server URL, to name an account nobody
// named.
func davServerName(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return "Calendar and contacts"
}

// davAccountDiscovery is what signing in to the server found: the calendars
// with the server they are on, or the address books.
type davAccountDiscovery struct {
	serverURL    string
	calendars    []storage.CalendarSource
	addressBooks []models.ContactAddressBook
}

// discoverDAVAccount signs in to the server and lists its calendars or
// address books, first making the one create asks for, when it isn't nil.
func discoverDAVAccount(ctx context.Context, form davAccountForm, create *davCreate) (davAccountDiscovery, error) {
	ctx, cancel := context.WithTimeout(ctx, cardDAVDiscoveryOverallTimeout)
	defer cancel()
	var found davAccountDiscovery
	var err error
	if form.service == davServiceCalendar {
		found.serverURL, err = normalizeCalDAVBaseURL(form.serverURL)
		if err == nil {
			found.calendars, err = discoverCalDAVCalendars(ctx, found.serverURL, form.username, form.password, "", "", create)
		}
		if err == nil && len(found.calendars) > calendarDiscoveryMaxSources {
			err = errors.New("CalDAV discovery exceeded its source limit")
		}
		if err != nil {
			return found, fmt.Errorf("Could not sign in to the CalDAV server: %w", err)
		}
		return found, nil
	}
	found.serverURL, err = normalizeCardDAVBaseURL(form.serverURL)
	if err == nil {
		found.addressBooks, err = discoverCardDAVAddressBooksCandidates(ctx, contactSyncDiscoveryCandidates(found.serverURL), form.username, form.password, nil, false, create)
	}
	if err != nil {
		return found, fmt.Errorf("Could not sign in to the CardDAV server: %w", err)
	}
	return found, nil
}

// davItems lists what signing in found, ticked as checked says: whether an
// item syncs, and whether it is the default address book.
func davItems(found davAccountDiscovery, checked func(id string) (bool, bool)) []views.DAVItem {
	var items []views.DAVItem
	for _, source := range found.calendars {
		item := views.DAVItem{ID: source.RemoteID, Name: source.Name, Detail: source.Description, Color: source.Color, Primary: source.IsPrimary}
		item.Checked, _ = checked(source.RemoteID)
		items = append(items, item)
	}
	for _, book := range found.addressBooks {
		item := views.DAVItem{ID: book.URL, Name: contactAddressBookLabel(book)}
		if item.Name != book.URL {
			item.Detail = book.URL
		}
		item.Checked, item.Default = checked(book.URL)
		items = append(items, item)
	}
	return items
}

// davSavedChoices ticks what an account already syncs, or for an account
// imported from a profile what the profile synced; anything else the server
// has starts unticked.
func davSavedChoices(data models.EditAccountData) func(string) (bool, bool) {
	return func(id string) (bool, bool) {
		for _, source := range data.CalendarSync.Sources {
			if source.RemoteID == id {
				return source.IsSelected || (data.ReconnectPending && slices.Contains(data.ReconnectCalendars, id)), false
			}
		}
		for _, book := range data.ContactSync.AddressBooks {
			if book.URL == id {
				return true, book.Default
			}
		}
		return false, false
	}
}

// davNewChoices ticks, for a new account, the primary calendar, or every
// address book.
func davNewChoices(found davAccountDiscovery) func(string) (bool, bool) {
	return func(id string) (bool, bool) {
		for _, source := range found.calendars {
			if source.RemoteID == id {
				return source.IsSelected, false
			}
		}
		return true, false
	}
}

// handleUserFindDAVItems signs in to the server in the dialog of a
// calendar-only or contacts-only account and lists its calendars or address
// books to choose from. Nothing is saved.
func (h *Handler) handleUserFindDAVItems(w http.ResponseWriter, r *http.Request) {
	var saved *models.EditAccountData
	owner, id := h.userID(r.Context()), r.PathValue("id")
	if id != "" {
		var err error
		if saved, err = h.davAccountEditData(r.Context(), owner, id); err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	form, problem := parseDAVAccountForm(w, r, saved)
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	if saved != nil && strings.TrimSpace(form.password) == "" {
		var err error
		if form.password, err = h.davAccountPassword(r.Context(), owner, id); err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	create, err := davCreateFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.checkDAVLogin(w, r, form.username) {
		return
	}
	found, err := discoverDAVAccount(r.Context(), form, create)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	checked := davNewChoices(found)
	if create != nil {
		checked = davCreatedChoices(form, create.Created)
	} else if saved != nil {
		checked = davSavedChoices(*saved)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.DAVItems(form.service, davItems(found, checked)).Render(r.Context(), w)
}

// davCreatedChoices keeps, after a calendar or address book was made, what
// the dialog had ticked, and ticks the new one too. A new address book is the
// default when none was.
func davCreatedChoices(form davAccountForm, created string) func(string) (bool, bool) {
	return func(id string) (bool, bool) {
		isNew := created != "" && davSameCollection(id, created)
		if form.defaultItem != "" {
			return isNew || slices.Contains(form.items, id), id == form.defaultItem
		}
		return isNew || slices.Contains(form.items, id), isNew
	}
}

// handleUserCreateDAVAccount adds an account with no mail, only a CalDAV or
// CardDAV server. It signs in first, so nothing is saved for a server or login
// that doesn't work, then saves the calendars or address books ticked.
func (h *Handler) handleUserCreateDAVAccount(w http.ResponseWriter, r *http.Request) {
	form, problem := parseDAVAccountForm(w, r, nil)
	if problem == "" && len(form.items) == 0 {
		problem = davChooseOne(form.service)
	}
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	if !h.checkDAVLogin(w, r, form.username) {
		return
	}
	found, err := discoverDAVAccount(r.Context(), form, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner := h.userID(r.Context())
	account, err := h.userAccounts.CreateAccount(r.Context(), owner, &models.CreateAccountRequest{
		Provider: providers.ProviderDAV, DisplayName: form.name, Username: form.username, Password: form.password,
	})
	if err != nil {
		var pending *config.PendingAccountCreationError
		if errors.As(err, &pending) {
			w.Header().Set("X-Gofer-Account-ID", pending.AccountID)
		}
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("X-Gofer-Account-ID", account.ID)
	if err := h.saveDAVAccountServices(r.Context(), owner, account.ID, form, found); err != nil {
		log.Printf("routed account %s: save %s after creating it: %v", account.ID, form.service, err)
		http.Error(w, "The account was created, but its "+form.service+" could not be saved. Open Edit to choose them again.", http.StatusServiceUnavailable)
		return
	}
	if err := h.userStorage.WithAccountActivityForUser(r.Context(), owner, account.ID, func() error {
		return h.userAccountHooks.Created(r.Context(), account.ID)
	}); err != nil {
		log.Printf("routed account %s created but its sync could not be started: %v", account.ID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUserUpdateDAVAccount saves the dialog of a calendar-only or
// contacts-only account: its name, server and login, signing in with them
// first, and the calendars or address books ticked.
func (h *Handler) handleUserUpdateDAVAccount(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), r.PathValue("id")
	saved, err := h.davAccountEditData(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	form, problem := parseDAVAccountForm(w, r, saved)
	if problem == "" && len(form.items) == 0 {
		problem = davChooseOne(form.service)
	}
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	login := form
	if strings.TrimSpace(login.password) == "" {
		if login.password, err = h.davAccountPassword(r.Context(), owner, id); err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	if !h.checkDAVLogin(w, r, login.username) {
		return
	}
	found, err := discoverDAVAccount(r.Context(), login, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.userAccounts.UpdateAccount(r.Context(), owner, id, &models.CreateAccountRequest{
		Provider: providers.ProviderDAV, DisplayName: form.name, Username: form.username, Password: form.password,
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	if saved.ReconnectPending {
		// It came from a profile; with its password it syncs again, with the
		// calendars or address books ticked below.
		if _, _, err := h.resolveImportedLogin(r.Context(), owner, id, storage.ImportedLoginMail); err != nil {
			log.Printf("routed account %s: turn on imported services: %v", id, err)
		}
	}
	if err := h.saveDAVAccountServices(r.Context(), owner, id, form, found); err != nil {
		log.Printf("routed account %s: save %s: %v", id, form.service, err)
		userContactSetupError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkDAVLogin refuses, before signing in, a login the allowed mail lists
// don't let the person use, and reports whether it may go on.
func (h *Handler) checkDAVLogin(w http.ResponseWriter, r *http.Request, username string) bool {
	list, err := h.mailAllowlist(r.Context())
	if err == nil {
		err = list.CheckLogin(username)
	}
	if err != nil {
		userAccountError(w, r, err)
		return false
	}
	return true
}

func davChooseOne(service string) string {
	if service == davServiceCalendar {
		return "Find the calendars, then choose at least one."
	}
	return "Find the address books, then choose at least one."
}

// davAccountEditData is the saved settings of an account with no mail.
func (h *Handler) davAccountEditData(ctx context.Context, owner, id string) (*models.EditAccountData, error) {
	data, err := h.userEditData(ctx, owner, id)
	if err == nil && data.Provider != providers.ProviderDAV {
		return nil, storage.ErrAccountRoute
	}
	return data, err
}

func (h *Handler) davAccountPassword(ctx context.Context, owner, id string) (string, error) {
	snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
	if err != nil {
		return "", err
	}
	return snapshot.MailboxPassword()
}

// saveDAVAccountServices saves what signing in found, using the account's
// own login: the calendars, with the ones ticked selected, or the address
// books ticked.
func (h *Handler) saveDAVAccountServices(ctx context.Context, owner, id string, form davAccountForm, found davAccountDiscovery) error {
	if form.service == davServiceCalendar {
		err := h.userIMAP.RunAccountService(ctx, owner, id, mail.AccountServiceCalendar, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
			snapshot, err := h.userAccounts.SnapshotCalendarDiscovery(ctx, owner, id)
			if err != nil {
				return err
			}
			_, err = h.userAccounts.PublishCalendarDiscovery(ctx, snapshot, found.calendars,
				&config.DiscoveredCalDAVSettings{BaseURL: found.serverURL, UseAccountCredentials: true})
			return err
		})
		if err != nil {
			return err
		}
		data, err := h.userEditData(ctx, owner, id)
		if err != nil {
			return err
		}
		var selected []string
		for _, source := range data.CalendarSync.Sources {
			if slices.Contains(form.items, source.RemoteID) {
				selected = append(selected, source.ID)
			}
		}
		if err := h.userAccounts.SetCalendarSourcesSelected(ctx, owner, id, selected); err != nil {
			return err
		}
		h.wakeUserCalendarAccount(ctx, owner, id)
		return nil
	}
	var books []models.ContactAddressBook
	for _, book := range found.addressBooks {
		if slices.Contains(form.items, book.URL) {
			book.Default = book.URL == form.defaultItem
			books = append(books, book)
		}
	}
	err := h.userIMAP.RunAccountService(ctx, owner, id, mail.AccountServiceContacts, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		cfg := models.ContactSyncConfig{Provider: providers.ProviderCardDAV, Enabled: true,
			BaseURL: found.serverURL, Username: form.username, AddressBooks: books}
		return h.userAccounts.SaveContactServices(ctx, snapshot, cfg, "")
	})
	if err != nil {
		return err
	}
	return h.WakeUserContactAccount(ctx, owner, id)
}

// davAccountDialogData fills the dialog of an account with no mail from its
// saved settings.
func davAccountDialogData(data models.EditAccountData) views.DAVAccountDialogData {
	dialog := views.DAVAccountDialogData{AccountID: data.AccountID, Service: davAccountService(data), Name: data.DisplayName, Username: data.Username, NeedsPassword: data.ReconnectPending}
	checked := davSavedChoices(data)
	if dialog.Service == davServiceCalendar {
		dialog.ServerURL = data.CalendarSync.CalDAVBaseURL
		for _, source := range data.CalendarSync.Sources {
			item := views.DAVItem{ID: source.RemoteID, Name: calendarSourceLabel(source), Color: source.Color, Primary: source.IsPrimary}
			item.Checked, _ = checked(source.RemoteID)
			dialog.Items = append(dialog.Items, item)
		}
		return dialog
	}
	dialog.ServerURL = data.ContactSync.BaseURL
	for _, book := range data.ContactSync.AddressBooks {
		item := views.DAVItem{ID: book.URL, Name: contactAddressBookLabel(book), Checked: true, Default: book.Default}
		if item.Name != book.URL {
			item.Detail = book.URL
		}
		dialog.Items = append(dialog.Items, item)
	}
	return dialog
}

func calendarSourceLabel(source models.CalendarSource) string {
	if name := strings.TrimSpace(source.Name); name != "" {
		return name
	}
	return source.RemoteID
}

// handleUserAddAccountMail adds a mailbox to an account with no mail, which
// the add-account wizard does in place of creating one. It answers with the
// wizard's contacts and calendar steps, as creating does.
func (h *Handler) handleUserAddAccountMail(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), r.PathValue("id")
	existing, err := h.userEditData(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if existing.Provider != providers.ProviderDAV {
		http.Error(w, config.ErrAccountHasMail.Error(), http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	req := accountRequestFromForm(r)
	if req.Provider == providers.ProviderJMAP {
		if err := h.prepareJMAPAccount(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" || req.Password == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return
	}
	if (req.Provider != "" && req.Provider != providers.ProviderIMAP && req.Provider != providers.ProviderJMAP) || (req.AuthMethod != "" && req.AuthMethod != "plain") {
		http.Error(w, "this account connection method is not available yet", http.StatusNotImplemented)
		return
	}
	if err := h.userAccounts.AddMail(r.Context(), owner, id, &req); err != nil {
		if errors.Is(err, config.ErrAccountHasMail) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("X-Gofer-Account-ID", id)
	if err := h.userStorage.WithAccountActivityForUser(r.Context(), owner, id, func() error {
		return h.userAccountHooks.Updated(r.Context(), id)
	}); err != nil {
		log.Printf("routed account %s got mail but its sync could not be started: %v", id, err)
		http.Error(w, "email added; synchronization could not be started", http.StatusServiceUnavailable)
		return
	}
	data, err := h.userEditData(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.AddAccountPostCreateStep(*data).Render(r.Context(), w)
}
