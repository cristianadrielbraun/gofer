package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

type davMultiStatus struct {
	Responses []davResponse `xml:"response"`
	SyncToken string        `xml:"sync-token"`
}

type cardDAVDiscoveryProgress func(completed, total int, endpoint string)

const (
	cardDAVDiscoveryOverallTimeout = 25 * time.Second
	cardDAVDiscoveryHTTPTimeout    = 5 * time.Second
	cardDAVDiscoveryDNSTimeout     = 2 * time.Second
)

type davResponse struct {
	Href      string        `xml:"href"`
	Status    string        `xml:"status"`
	PropStats []davPropStat `xml:"propstat"`
}

type davPropStat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	GetETag              string          `xml:"getetag"`
	AddressData          string          `xml:"address-data"`
	DisplayName          string          `xml:"displayname"`
	CTag                 string          `xml:"getctag"`
	CurrentUserPrincipal davHrefProp     `xml:"current-user-principal"`
	AddressBookHomeSet   davHrefProp     `xml:"addressbook-home-set"`
	ResourceType         davResourceType `xml:"resourcetype"`
}

type davHrefProp struct {
	Href string `xml:"href"`
}

type davResourceType struct {
	Collection  bool
	AddressBook bool
}

type cardDAVSyncResult struct {
	Responses []davResponse
	SyncToken string
	Fallback  bool
}

type cardDAVHTTPError struct {
	Status int
	Body   string
}

func (e cardDAVHTTPError) Error() string {
	return fmt.Sprintf("CardDAV returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (r *davResourceType) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "collection":
				r.Collection = true
			case "addressbook":
				r.AddressBook = true
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return nil
			}
		}
	}
}

func (h *Handler) handleSaveAccountContactSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := r.PathValue("id")
	if accountID == "" {
		htmlStatus(w, http.StatusBadRequest, "Account is required.")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid contact sync settings.")
		return
	}

	cfg := models.ContactSyncConfig{
		AccountID:      accountID,
		UserID:         userID,
		Provider:       providers.ProviderCardDAV,
		BaseURL:        strings.TrimSpace(r.FormValue("base_url")),
		AddressBookURL: strings.TrimSpace(r.FormValue("addressbook_url")),
		Username:       strings.TrimSpace(r.FormValue("username")),
	}
	cfg.AddressBooks = contactSyncAddressBooksFromForm(r, cfg.AddressBookURL)
	password := r.FormValue("password")
	cfg.Enabled = cfg.BaseURL != "" || len(cfg.AddressBooks) > 0 || cfg.Username != "" || password != ""
	if cfg.Enabled {
		if cfg.Username == "" || r.FormValue("use_account_credentials") == "1" {
			accountEmail, accountUsername, _, _, err := h.contactSyncAccountIdentity(ctx, userID, accountID)
			if err != nil {
				htmlStatus(w, http.StatusBadRequest, "Account is required.")
				return
			}
			cfg.Username = accountUsername
			if cfg.Username == "" {
				cfg.Username = accountEmail
			}
		}
		if cfg.AddressBookURL == "" && len(cfg.AddressBooks) == 0 {
			cfg.AddressBookURL = cfg.BaseURL
			cfg.AddressBooks = contactSyncAddressBooksFromURL(cfg.AddressBookURL)
		}
		if len(cfg.AddressBooks) == 0 || cfg.Username == "" {
			htmlStatus(w, http.StatusBadRequest, "Choose at least one CardDAV address book and enter a username.")
			return
		}
		if password == "" {
			var err error
			password, err = h.contactSyncPasswordOrAccount(ctx, userID, accountID, "")
			if err != nil || password == "" {
				htmlStatus(w, http.StatusBadRequest, "CardDAV password is required.")
				return
			}
		}
		for _, book := range cfg.AddressBooks {
			bookCfg := cfg
			bookCfg.AddressBookURL = book.URL
			if err := testCardDAVAddressBook(ctx, bookCfg, password); err != nil {
				htmlStatus(w, http.StatusBadRequest, "CardDAV save failed: could not connect to "+contactAddressBookLabel(book)+": "+err.Error())
				return
			}
		}
	}
	if err := h.accountStore.SaveContactSyncConfig(ctx, userID, accountID, cfg, password); err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not save contact sync settings.")
		return
	}
	if !cfg.Enabled {
		htmlStatus(w, http.StatusOK, "Contact sync is disabled for this account.")
		return
	}
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := h.SyncContactAccount(bg, accountID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
			log.Printf("contacts sync %s after save: %v", accountID, err)
		}
	}()
	htmlContactSyncSaved(w, accountID, cfg)
}

func htmlContactSyncSaved(w http.ResponseWriter, accountID string, cfg models.ContactSyncConfig) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Gofer-Status", "ok")
	w.WriteHeader(http.StatusOK)
	books := contactSyncSelectedBooks(cfg.AddressBooks)
	defaultBook := contactSyncDefaultBook(books)
	message := `<div class="rounded-md border border-border bg-background px-3 py-2 text-xs text-muted-foreground"><div class="flex items-center gap-2"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3.5 text-muted-foreground"><path d="M20 6 9 17l-5-5"></path></svg><span>CardDAV contact sync settings saved and verified.</span></div></div>`
	_, _ = w.Write([]byte(message))
	summary := contactSyncSavedSummaryHTML(books, defaultBook)
	_, _ = w.Write([]byte(`<div id="account-contact-sync-fields-` + html.EscapeString(accountID) + `" hx-swap-oob="outerHTML" class="grid gap-3 sm:grid-cols-2">` + summary + `</div>`))
	_, _ = w.Write([]byte(`<div id="add-contact-sync-fields-` + html.EscapeString(accountID) + `" hx-swap-oob="outerHTML" class="space-y-3">` + summary + `</div>`))
}

func contactSyncSavedSummaryHTML(books []models.ContactAddressBook, defaultBook models.ContactAddressBook) string {
	bookNames := make([]string, 0, len(books))
	for _, book := range books {
		bookNames = append(bookNames, contactAddressBookLabel(book))
	}
	defaultName := contactAddressBookLabel(defaultBook)
	return `<div class="sm:col-span-2 rounded-lg border border-border bg-muted/30 p-3 text-xs text-muted-foreground"><div class="flex items-start gap-2"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="mt-0.5 size-4 shrink-0 text-muted-foreground"><path d="M20 6 9 17l-5-5"></path></svg><div class="min-w-0"><div class="font-semibold text-foreground">Contact sync saved</div><p class="mt-1 leading-relaxed">Synced address books: ` + html.EscapeString(contactSyncHumanList(bookNames)) + `.</p><p class="mt-1 leading-relaxed">Default for new contacts: ` + html.EscapeString(defaultName) + `.</p></div></div></div>`
}

func contactSyncSelectedBooks(books []models.ContactAddressBook) []models.ContactAddressBook {
	selected := make([]models.ContactAddressBook, 0, len(books))
	for _, book := range books {
		if book.URL != "" {
			selected = append(selected, book)
		}
	}
	return selected
}

func contactSyncDefaultBook(books []models.ContactAddressBook) models.ContactAddressBook {
	for _, book := range books {
		if book.Default {
			return book
		}
	}
	if len(books) > 0 {
		return books[0]
	}
	return models.ContactAddressBook{Name: "None"}
}

func contactSyncHumanList(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	if len(items) == 1 {
		return items[0]
	}
	if len(items) == 2 {
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

func contactAddressBookLabel(book models.ContactAddressBook) string {
	if strings.TrimSpace(book.Name) != "" {
		return strings.TrimSpace(book.Name)
	}
	if strings.TrimSpace(book.URL) != "" {
		return strings.TrimSpace(book.URL)
	}
	return "address book"
}

func (h *Handler) handleTestAccountContactSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := r.PathValue("id")
	if accountID == "" {
		htmlStatus(w, http.StatusBadRequest, "Account is required.")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid contact sync test.")
		return
	}

	cfg := models.ContactSyncConfig{
		AccountID:      accountID,
		UserID:         userID,
		Provider:       providers.ProviderCardDAV,
		Enabled:        true,
		BaseURL:        strings.TrimSpace(r.FormValue("base_url")),
		AddressBookURL: strings.TrimSpace(r.FormValue("addressbook_url")),
		Username:       strings.TrimSpace(r.FormValue("username")),
	}
	cfg.AddressBooks = contactSyncAddressBooksFromForm(r, cfg.AddressBookURL)
	if cfg.AddressBookURL == "" && len(cfg.AddressBooks) == 0 {
		cfg.AddressBookURL = cfg.BaseURL
		cfg.AddressBooks = contactSyncAddressBooksFromURL(cfg.AddressBookURL)
	}
	password := r.FormValue("password")
	if password == "" {
		var err error
		password, err = h.contactSyncPasswordOrAccount(ctx, userID, accountID, "")
		if err != nil || password == "" {
			htmlStatus(w, http.StatusBadRequest, "Enter the CardDAV password before testing.")
			return
		}
	}
	if len(cfg.AddressBooks) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Choose at least one CardDAV address book before testing.")
		return
	}
	for _, book := range cfg.AddressBooks {
		bookCfg := cfg
		bookCfg.AddressBookURL = book.URL
		if err := testCardDAVAddressBook(ctx, bookCfg, password); err != nil {
			htmlStatus(w, http.StatusOK, "CardDAV test failed: "+err.Error())
			return
		}
	}
	htmlStatus(w, http.StatusOK, "CardDAV connection succeeded.")
}

func contactSyncAddressBooksFromForm(r *http.Request, fallbackURL string) []models.ContactAddressBook {
	urls := r.Form["addressbook_url"]
	ids := r.Form["addressbook_id"]
	names := r.Form["addressbook_name"]
	defaultURL := strings.TrimSpace(r.FormValue("default_addressbook_url"))
	books := make([]models.ContactAddressBook, 0, len(urls))
	seen := make(map[string]bool)
	for i, rawURL := range urls {
		bookURL := strings.TrimSpace(rawURL)
		if bookURL == "" || seen[bookURL] {
			continue
		}
		seen[bookURL] = true
		name := ""
		if len(names) == len(urls) && i < len(names) {
			name = strings.TrimSpace(names[i])
		}
		bookID := ""
		if len(ids) == len(urls) && i < len(ids) {
			bookID = strings.TrimSpace(ids[i])
		}
		books = append(books, models.ContactAddressBook{ID: bookID, Name: name, URL: bookURL, Selected: true, Default: defaultURL != "" && defaultURL == bookURL})
	}
	if len(books) == 0 {
		return contactSyncAddressBooksFromURL(fallbackURL)
	}
	defaultSet := false
	for _, book := range books {
		if book.Default {
			defaultSet = true
			break
		}
	}
	if !defaultSet {
		books[0].Default = true
	}
	return books
}

func contactSyncAddressBooksFromURL(bookURL string) []models.ContactAddressBook {
	bookURL = strings.TrimSpace(bookURL)
	if bookURL == "" {
		return nil
	}
	return []models.ContactAddressBook{{URL: bookURL, Selected: true, Default: true}}
}

func (h *Handler) handleDiscoverAccountContactSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), cardDAVDiscoveryOverallTimeout)
	defer cancel()
	userID := h.userID(ctx)
	accountID := r.PathValue("id")
	if accountID == "" {
		writeJSONError(w, http.StatusBadRequest, "account is required")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid discovery request")
		return
	}

	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(r.FormValue("addressbook_url"))
	}
	username := strings.TrimSpace(r.FormValue("username"))
	accountEmail, accountUsername, imapHost, smtpHost, err := h.contactSyncAccountIdentity(ctx, userID, accountID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "account is required")
		return
	}
	if username == "" {
		username = accountUsername
		if username == "" {
			username = accountEmail
		}
	}
	autodiscover := r.FormValue("autodiscover") == "1"
	candidates := contactSyncDiscoveryCandidates(baseURL)
	if autodiscover {
		candidates = contactSyncDiscoveryCandidates(baseURL, imapHost, smtpHost, username, accountEmail)
	} else if len(candidates) == 0 {
		writeJSONError(w, http.StatusBadRequest, "enter a CardDAV base URL, or use Attempt URL autodiscover")
		return
	}
	password := r.FormValue("password")
	if password == "" {
		password, err = h.contactSyncPasswordOrAccount(ctx, userID, accountID, "")
		if err != nil || password == "" {
			writeJSONError(w, http.StatusBadRequest, "enter the CardDAV password before discovery")
			return
		}
	}

	log.Printf("contacts carddav discover %s: trying %d candidate(s): %s", accountID, len(candidates), strings.Join(candidates, ", "))
	if strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		h.streamCardDAVDiscovery(w, ctx, accountID, candidates, username, password, autodiscover)
		return
	}

	books, err := discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, nil, autodiscover)
	if err != nil {
		log.Printf("contacts carddav discover %s: failed: %v", accountID, err)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("contacts carddav discover %s: found %d address book(s)", accountID, len(books))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"address_books": books})
}

func (h *Handler) streamCardDAVDiscovery(w http.ResponseWriter, ctx context.Context, accountID string, candidates []string, username, password string, autodiscover bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	encoder := json.NewEncoder(w)
	writeEvent := func(event map[string]any) {
		_ = encoder.Encode(event)
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent(map[string]any{"type": "start", "message": "Preparing CardDAV discovery..."})
	books, err := discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, func(completed, total int, endpoint string) {
		writeEvent(map[string]any{"type": "progress", "completed": completed, "total": total, "endpoint": endpoint})
	}, autodiscover)
	if err != nil {
		log.Printf("contacts carddav discover %s: failed: %v", accountID, err)
		writeEvent(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	log.Printf("contacts carddav discover %s: found %d address book(s)", accountID, len(books))
	writeEvent(map[string]any{"type": "done", "address_books": books})
}

func (h *Handler) contactSyncAccountIdentity(ctx context.Context, userID, accountID string) (string, string, string, string, error) {
	var email, username, imapHost, smtpHost string
	err := h.db.Read().QueryRowContext(ctx, `
		SELECT email_address, username, imap_host, smtp_host
		FROM accounts
		WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID).Scan(&email, &username, &imapHost, &smtpHost)
	return strings.TrimSpace(email), strings.TrimSpace(username), strings.TrimSpace(imapHost), strings.TrimSpace(smtpHost), err
}

func contactSyncDiscoveryCandidates(values ...string) []string {
	seen := make(map[string]bool)
	candidates := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, value)
	}
	return candidates
}

func (h *Handler) contactSyncPasswordOrAccount(ctx context.Context, userID, accountID, submitted string) (string, error) {
	if strings.TrimSpace(submitted) != "" {
		return submitted, nil
	}
	password, err := h.accountStore.DecryptContactSyncPassword(ctx, userID, accountID)
	if err == nil && password != "" {
		return password, nil
	}
	password, err = h.accountStore.DecryptPassword(ctx, accountID)
	if err == nil && password != "" {
		return password, nil
	}
	return "", err
}

func (h *Handler) syncCardDAVContacts(ctx context.Context, userID string, account contactSyncAccount) (int, error) {
	cfg, password, err := h.cardDAVConfig(ctx, userID, account.ID)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, book := range cfg.AddressBooks {
		bookCfg := cfg
		bookCfg.AddressBookURL = book.URL
		bookCfg.LastSyncToken = book.LastSyncToken
		result, err := cardDAVSync(ctx, bookCfg, password)
		if err != nil {
			_ = h.accountStore.MarkContactSyncError(ctx, userID, account.ID, err.Error())
			return imported, err
		}

		seenRemoteIDs := make(map[string]bool)
		for _, response := range result.Responses {
			remoteID := absoluteDAVHref(bookCfg.AddressBookURL, response.Href)
			if response.deleted() {
				if remoteID != "" {
					if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderCardDAV, account.ID, remoteID); err != nil {
						return imported, err
					}
				}
				continue
			}
			if remoteID != "" {
				seenRemoteIDs[remoteID] = true
			}
			addressData := strings.TrimSpace(response.addressData())
			if addressData == "" {
				continue
			}
			contacts, err := parseVCardContacts(strings.NewReader(addressData), []string{"account:" + account.ID})
			if err != nil {
				return imported, err
			}
			for _, contact := range contacts {
				contactID, _, err := h.db.UpsertSyncedContactFromContact(ctx, userID, account.ID, contact)
				if err != nil {
					return imported, err
				}
				if book.ID != "" {
					_ = h.db.AddContactSaveTarget(ctx, userID, contactID, "book:"+book.ID)
				}
				if contactID != "" && remoteID != "" {
					if err := h.db.UpsertContactSource(ctx, storage.ContactSource{
						ContactID:     contactID,
						UserID:        userID,
						Provider:      providers.ProviderCardDAV,
						AccountID:     account.ID,
						AddressBookID: book.ID,
						RemoteID:      remoteID,
						Etag:          response.etag(),
					}); err != nil {
						return imported, err
					}
				}
				imported++
			}
		}
		if result.Fallback {
			if err := h.pruneMissingCardDAVSourcesForBook(ctx, userID, account.ID, bookCfg.AddressBookURL, seenRemoteIDs); err != nil {
				return imported, err
			}
		}
		_ = h.accountStore.MarkContactAddressBookSyncSuccess(ctx, userID, account.ID, bookCfg.AddressBookURL, result.SyncToken)
	}
	return imported, nil
}

func (h *Handler) pruneMissingCardDAVSources(ctx context.Context, userID, accountID string, seenRemoteIDs map[string]bool) error {
	return h.pruneMissingCardDAVSourcesForBook(ctx, userID, accountID, "", seenRemoteIDs)
}

func (h *Handler) pruneMissingCardDAVSourcesForBook(ctx context.Context, userID, accountID, addressBookURL string, seenRemoteIDs map[string]bool) error {
	sources, err := h.db.ListContactSourcesForAccount(ctx, userID, providers.ProviderCardDAV, accountID)
	if err != nil {
		return err
	}
	addressBookURL = strings.TrimRight(strings.TrimSpace(addressBookURL), "/") + "/"
	for _, source := range sources {
		remoteID := strings.TrimSpace(source.RemoteID)
		if addressBookURL != "/" && !strings.HasPrefix(remoteID, addressBookURL) {
			continue
		}
		if remoteID == "" || seenRemoteIDs[remoteID] {
			continue
		}
		if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderCardDAV, accountID, remoteID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) pushContactToCardDAVAccount(ctx context.Context, userID string, contact models.Contact, accountID string) error {
	cfg, password, err := h.cardDAVConfig(ctx, userID, accountID)
	if err != nil {
		return err
	}
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderCardDAV)
	if err != nil {
		return err
	}
	accountSources := make([]storage.ContactSource, 0)
	for _, source := range sources {
		if source.AccountID == accountID {
			accountSources = append(accountSources, source)
		}
	}
	selectedBooks := cardDAVTargetBooks(cfg, contact.SaveTargets)
	if len(selectedBooks) > 0 {
		selected := make(map[string]bool, len(selectedBooks))
		for _, book := range selectedBooks {
			selected[book.ID] = true
		}
		byBook := make(map[string]*storage.ContactSource)
		for i := range accountSources {
			source := accountSources[i]
			bookID := source.AddressBookID
			if bookID == "" {
				_, book := cardDAVConfigForSource(cfg, &source)
				bookID = book.ID
			}
			if selected[bookID] {
				copySource := source
				copySource.AddressBookID = bookID
				byBook[bookID] = &copySource
				continue
			}
			if strings.TrimSpace(source.RemoteID) != "" {
				if err := h.deleteCardDAVContact(ctx, userID, source); err != nil {
					return err
				}
				if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderCardDAV, accountID, source.RemoteID); err != nil {
					return err
				}
			}
		}
		for _, book := range selectedBooks {
			bookCfg := cfg
			bookCfg.AddressBookURL = book.URL
			bookCfg.AddressBooks = []models.ContactAddressBook{book}
			if err := h.pushContactToCardDAVBook(ctx, userID, contact, bookCfg, password, byBook[book.ID]); err != nil {
				return err
			}
		}
		return nil
	}
	if len(accountSources) == 0 {
		return h.pushContactToCardDAVBook(ctx, userID, contact, cfg, password, nil)
	}
	for i := range accountSources {
		source := accountSources[i]
		if err := h.pushContactToCardDAVBook(ctx, userID, contact, cfg, password, &source); err != nil {
			return err
		}
	}
	return nil
}

func cardDAVTargetBooks(cfg models.ContactSyncConfig, targets []string) []models.ContactAddressBook {
	requested := make(map[string]bool)
	for _, target := range targets {
		bookID, ok := strings.CutPrefix(strings.TrimSpace(target), "book:")
		if ok && bookID != "" {
			requested[bookID] = true
		}
	}
	if len(requested) == 0 {
		return nil
	}
	books := make([]models.ContactAddressBook, 0, len(requested))
	for _, book := range cfg.AddressBooks {
		if requested[book.ID] {
			books = append(books, book)
		}
	}
	return books
}

func (h *Handler) pushContactToCardDAVBook(ctx context.Context, userID string, contact models.Contact, cfg models.ContactSyncConfig, password string, source *storage.ContactSource) error {
	bookCfg, book := cardDAVConfigForSource(cfg, source)
	remoteID := ""
	etag := ""
	if source != nil {
		remoteID = strings.TrimSpace(source.RemoteID)
		etag = strings.TrimSpace(source.Etag)
	}
	if remoteID == "" {
		remoteID = newCardDAVContactHref(bookCfg.AddressBookURL)
	} else if etag == "" {
		latest, err := cardDAVFetchContact(ctx, bookCfg, password, remoteID)
		if err != nil {
			if isCardDAVStatus(err, http.StatusNotFound, http.StatusGone) {
				remoteID = newCardDAVContactHref(bookCfg.AddressBookURL)
			} else {
				return err
			}
		} else {
			etag = latest.etag()
		}
	}

	body, err := renderVCard4([]models.Contact{contact})
	if err != nil {
		return err
	}
	newEtag, err := cardDAVPut(ctx, bookCfg, password, remoteID, etag, body)
	if err != nil {
		if isCardDAVStatus(err, http.StatusNotFound, http.StatusGone) {
			remoteID = newCardDAVContactHref(bookCfg.AddressBookURL)
			newEtag, err = cardDAVPut(ctx, bookCfg, password, remoteID, "", body)
		}
		if isCardDAVStatus(err, http.StatusPreconditionFailed) {
			latest, fetchErr := cardDAVFetchContact(ctx, bookCfg, password, remoteID)
			if fetchErr == nil && source != nil {
				_ = h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderCardDAV, AccountID: cfg.AccountID, AddressBookID: book.ID, RemoteID: remoteID, Etag: latest.etag()})
			}
			return fmt.Errorf("CardDAV contact changed remotely; sync again before saving this contact")
		}
	}
	if err != nil {
		return err
	}
	return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderCardDAV, AccountID: cfg.AccountID, AddressBookID: book.ID, RemoteID: remoteID, Etag: newEtag})
}

func cardDAVConfigForSource(cfg models.ContactSyncConfig, source *storage.ContactSource) (models.ContactSyncConfig, models.ContactAddressBook) {
	var selected models.ContactAddressBook
	if source != nil {
		if strings.TrimSpace(source.AddressBookID) != "" {
			for _, book := range cfg.AddressBooks {
				if book.ID == source.AddressBookID {
					selected = book
					break
				}
			}
		}
		remoteID := strings.TrimSpace(source.RemoteID)
		for _, book := range cfg.AddressBooks {
			if selected.URL != "" {
				break
			}
			prefix := strings.TrimRight(strings.TrimSpace(book.URL), "/") + "/"
			if prefix != "/" && strings.HasPrefix(remoteID, prefix) {
				selected = book
				break
			}
		}
	}
	if selected.URL == "" {
		for _, book := range cfg.AddressBooks {
			if book.Default {
				selected = book
				break
			}
		}
	}
	if selected.URL == "" && len(cfg.AddressBooks) > 0 {
		selected = cfg.AddressBooks[0]
	}
	if selected.URL != "" {
		cfg.AddressBookURL = selected.URL
	}
	return cfg, selected
}

func (h *Handler) deleteCardDAVContactsByEmail(ctx context.Context, userID, accountID, email string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil
	}
	sources, err := h.db.ListContactSourcesForEmail(ctx, userID, providers.ProviderCardDAV, accountID, email)
	if err != nil {
		return err
	}
	deletedTracked := false
	for _, source := range sources {
		if strings.TrimSpace(source.RemoteID) != "" {
			if err := h.deleteCardDAVContact(ctx, userID, source); err != nil {
				return err
			}
			if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderCardDAV, accountID, source.RemoteID); err != nil {
				return err
			}
			deletedTracked = true
			continue
		}
		if err := h.db.DeleteContactSource(ctx, userID, source.ContactID, providers.ProviderCardDAV, accountID); err != nil {
			return err
		}
		deletedTracked = true
	}
	if deletedTracked {
		return nil
	}

	cfg, password, err := h.cardDAVConfig(ctx, userID, accountID)
	if err != nil {
		return err
	}
	for _, book := range cfg.AddressBooks {
		bookCfg := cfg
		bookCfg.AddressBookURL = book.URL
		responses, err := cardDAVAddressBookEmailQuery(ctx, bookCfg, password, email)
		if err != nil {
			return err
		}
		for _, response := range responses {
			remoteID := absoluteDAVHref(bookCfg.AddressBookURL, response.Href)
			if remoteID == "" || response.deleted() {
				continue
			}
			if !cardDAVResponseHasExactEmail(response, email) {
				continue
			}
			if err := cardDAVDelete(ctx, bookCfg, password, remoteID, response.etag()); err != nil {
				return err
			}
		}
	}
	return nil
}

func cardDAVResponseHasExactEmail(response davResponse, email string) bool {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return false
	}
	addressData := strings.TrimSpace(response.addressData())
	if addressData == "" {
		return false
	}
	contacts, err := parseVCardContacts(strings.NewReader(addressData), nil)
	if err != nil {
		return false
	}
	for _, contact := range contacts {
		if strings.EqualFold(strings.TrimSpace(contact.Email), email) {
			return true
		}
	}
	return false
}

func (h *Handler) deleteCardDAVContact(ctx context.Context, userID string, source storage.ContactSource) error {
	cfg, password, err := h.cardDAVConfig(ctx, userID, source.AccountID)
	if err != nil {
		return err
	}
	bookCfg, _ := cardDAVConfigForSource(cfg, &source)
	return cardDAVDelete(ctx, bookCfg, password, source.RemoteID, source.Etag)
}

func (h *Handler) removeUnwantedContactSources(ctx context.Context, userID string, contact models.Contact, provider string, desired map[string]contactSyncAccount, handled map[string]bool) error {
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, provider)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if _, ok := desired[source.AccountID]; ok {
			continue
		}
		handled[source.AccountID] = true
		switch provider {
		case providers.ProviderGmail:
			if strings.TrimSpace(source.RemoteID) != "" {
				token, err := h.auth.GetOAuthTokenForAccount(ctx, source.AccountID)
				if err != nil {
					return err
				}
				if err := h.deleteGoogleContactByResourceAndEmail(ctx, token, source.RemoteID, contact.Email); err != nil {
					return err
				}
			}
		case providers.ProviderOutlook:
			if strings.TrimSpace(source.RemoteID) != "" {
				token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, source.AccountID)
				if err != nil {
					return err
				}
				if err := h.deleteOutlookContactByIDAndEmail(ctx, token, source.RemoteID, contact.Email); err != nil {
					return err
				}
			}
		case providers.ProviderCardDAV:
			if strings.TrimSpace(source.RemoteID) != "" {
				if err := h.deleteCardDAVContact(ctx, userID, source); err != nil {
					return err
				}
			}
		}
		if err := h.db.DeleteContactSource(ctx, userID, contact.ID, provider, source.AccountID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) cardDAVConfig(ctx context.Context, userID, accountID string) (models.ContactSyncConfig, string, error) {
	cfg, err := h.accountStore.GetContactSyncConfig(ctx, userID, accountID)
	if err != nil {
		return cfg, "", err
	}
	if !cfg.Enabled || cfg.Provider != providers.ProviderCardDAV {
		return cfg, "", fmt.Errorf("CardDAV is not configured for this account")
	}
	password, err := h.accountStore.DecryptContactSyncPassword(ctx, userID, accountID)
	if err != nil {
		return cfg, "", err
	}
	if cfg.AddressBookURL == "" {
		cfg.AddressBookURL = cfg.BaseURL
	}
	if len(cfg.AddressBooks) == 0 {
		cfg.AddressBooks = contactSyncAddressBooksFromURL(cfg.AddressBookURL)
	}
	if len(cfg.AddressBooks) == 0 {
		return cfg, "", fmt.Errorf("CardDAV address book is not configured for this account")
	}
	return cfg, password, nil
}

func cardDAVSync(ctx context.Context, cfg models.ContactSyncConfig, password string) (cardDAVSyncResult, error) {
	if strings.TrimSpace(cfg.LastSyncToken) != "" {
		result, err := cardDAVSyncCollection(ctx, cfg, password)
		if err == nil {
			return result, nil
		}
	}
	responses, syncToken, err := cardDAVAddressBookQuery(ctx, cfg, password)
	return cardDAVSyncResult{Responses: responses, SyncToken: syncToken, Fallback: true}, err
}

func cardDAVAddressBookQuery(ctx context.Context, cfg models.ContactSyncConfig, password string) ([]davResponse, string, error) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?>
<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop><d:getetag/><card:address-data/></d:prop>
</card:addressbook-query>`)
	req, err := newCardDAVRequest(ctx, "REPORT", cfg.AddressBookURL, cfg.Username, password, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("CardDAV returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	multi, err := decodeDAVMultiStatus(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return multi.Responses, strings.TrimSpace(multi.SyncToken), nil
}

func cardDAVAddressBookEmailQuery(ctx context.Context, cfg models.ContactSyncConfig, password, email string) ([]davResponse, error) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop><d:getetag/><card:address-data/></d:prop>
  <card:filter>
    <card:prop-filter name="EMAIL">
      <card:text-match collation="i;unicode-casemap" match-type="equals">%s</card:text-match>
    </card:prop-filter>
  </card:filter>
</card:addressbook-query>`, xmlEscapeText(strings.TrimSpace(email)))
	req, err := newCardDAVRequest(ctx, "REPORT", cfg.AddressBookURL, cfg.Username, password, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, cardDAVHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	multi, err := decodeDAVMultiStatus(resp.Body)
	if err != nil {
		return nil, err
	}
	return multi.Responses, nil
}

func cardDAVSyncCollection(ctx context.Context, cfg models.ContactSyncConfig, password string) (cardDAVSyncResult, error) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<d:sync-collection xmlns:d="DAV:">
  <d:sync-token>%s</d:sync-token>
  <d:sync-level>1</d:sync-level>
  <d:prop><d:getetag/></d:prop>
</d:sync-collection>`, xmlEscapeText(cfg.LastSyncToken))
	req, err := newCardDAVRequest(ctx, "REPORT", cfg.AddressBookURL, cfg.Username, password, strings.NewReader(body))
	if err != nil {
		return cardDAVSyncResult{}, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return cardDAVSyncResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return cardDAVSyncResult{}, fmt.Errorf("CardDAV sync returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	multi, err := decodeDAVMultiStatus(resp.Body)
	if err != nil {
		return cardDAVSyncResult{}, err
	}

	changed := make([]davResponse, 0, len(multi.Responses))
	deleted := make([]davResponse, 0)
	for _, response := range multi.Responses {
		if response.deleted() {
			deleted = append(deleted, response)
			continue
		}
		if strings.TrimSpace(response.Href) != "" {
			changed = append(changed, response)
		}
	}
	if len(changed) > 0 {
		fetched, err := cardDAVAddressBookMultiget(ctx, cfg, password, changed)
		if err != nil {
			return cardDAVSyncResult{}, err
		}
		changed = fetched
	}
	syncToken := strings.TrimSpace(multi.SyncToken)
	if syncToken == "" {
		syncToken = strings.TrimSpace(cfg.LastSyncToken)
	}
	return cardDAVSyncResult{Responses: append(changed, deleted...), SyncToken: syncToken}, nil
}

func cardDAVAddressBookMultiget(ctx context.Context, cfg models.ContactSyncConfig, password string, responses []davResponse) ([]davResponse, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/><card:address-data/></d:prop>`)
	for _, response := range responses {
		href := strings.TrimSpace(response.Href)
		if href == "" {
			continue
		}
		body.WriteString(`<d:href>`)
		body.WriteString(xmlEscapeText(href))
		body.WriteString(`</d:href>`)
	}
	body.WriteString(`</card:addressbook-multiget>`)
	req, err := newCardDAVRequest(ctx, "REPORT", cfg.AddressBookURL, cfg.Username, password, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, cardDAVHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	multi, err := decodeDAVMultiStatus(resp.Body)
	if err != nil {
		return nil, err
	}
	return multi.Responses, nil
}

func cardDAVFetchContact(ctx context.Context, cfg models.ContactSyncConfig, password, remoteID string) (davResponse, error) {
	responses, err := cardDAVAddressBookMultiget(ctx, cfg, password, []davResponse{{Href: remoteID}})
	if err != nil {
		return davResponse{}, err
	}
	for _, response := range responses {
		if response.deleted() {
			return davResponse{}, cardDAVHTTPError{Status: http.StatusNotFound, Body: "contact not found"}
		}
		if strings.TrimSpace(response.Href) != "" {
			return response, nil
		}
	}
	return davResponse{}, cardDAVHTTPError{Status: http.StatusNotFound, Body: "contact not found"}
}

func testCardDAVAddressBook(ctx context.Context, cfg models.ContactSyncConfig, password string) error {
	req, err := newCardDAVRequest(ctx, "PROPFIND", cfg.AddressBookURL, cfg.Username, password, strings.NewReader(`<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`))
	if err != nil {
		return err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: cardDAVDiscoveryHTTPTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func discoverCardDAVAddressBooks(ctx context.Context, rawBaseURL, username, password string) ([]models.ContactAddressBook, error) {
	baseURL, err := normalizeCardDAVBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}

	principalURL, err := discoverCardDAVCurrentUserPrincipal(ctx, cardDAVWellKnownURL(baseURL), username, password)
	if err != nil || principalURL == "" {
		principalURL, err = discoverCardDAVCurrentUserPrincipal(ctx, baseURL, username, password)
	}
	if err != nil {
		return nil, err
	}
	if principalURL == "" {
		return nil, fmt.Errorf("Could not auto-discover CardDAV for this account. Tried %s and %s, but the server did not return CardDAV account details. Enter the provider's CardDAV base URL, such as https://dav.example.com/ or https://mail.example.com/SOGo/dav/, then try again", cardDAVWellKnownURL(baseURL), baseURL)
	}
	principalURL = absoluteDAVHref(baseURL, principalURL)

	homeURL, err := discoverCardDAVAddressBookHome(ctx, principalURL, username, password)
	if err != nil {
		return nil, err
	}
	if homeURL == "" {
		return nil, fmt.Errorf("CardDAV server did not return an address book home")
	}
	homeURL = absoluteDAVHref(principalURL, homeURL)

	books, err := listCardDAVAddressBooks(ctx, homeURL, username, password)
	if err != nil {
		return nil, err
	}
	if len(books) == 0 {
		return nil, fmt.Errorf("no CardDAV address books found")
	}
	return books, nil
}

func discoverCardDAVAddressBooksAny(ctx context.Context, candidates []string, username, password string) ([]models.ContactAddressBook, error) {
	return discoverCardDAVAddressBooksAnyWithProgress(ctx, candidates, username, password, nil)
}

func discoverCardDAVAddressBooksAnyWithProgress(ctx context.Context, candidates []string, username, password string, progress cardDAVDiscoveryProgress) ([]models.ContactAddressBook, error) {
	return discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, progress, true)
}

func discoverCardDAVAddressBooksCandidates(ctx context.Context, candidates []string, username, password string, progress cardDAVDiscoveryProgress, autodiscover bool) ([]models.ContactAddressBook, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("Could not auto-discover CardDAV because no server URL, username, or email address is available. Enter the provider's CardDAV base URL and try again")
	}
	tracker := &cardDAVDiscoveryTracker{progress: progress}
	if autodiscover {
		candidates = expandCardDAVDiscoveryCandidates(ctx, candidates, tracker)
	}
	type candidateAttempt struct {
		candidate string
		baseURL   string
	}
	attemptCandidates := make([]candidateAttempt, 0, len(candidates))
	attempts := make([]string, 0, len(candidates)*2)
	seenBase := make(map[string]bool)
	for _, candidate := range candidates {
		baseURL, err := normalizeCardDAVBaseURL(candidate)
		if err != nil {
			continue
		}
		key := strings.ToLower(baseURL)
		if seenBase[key] {
			continue
		}
		seenBase[key] = true
		endpoints := []string{cardDAVWellKnownURL(baseURL), baseURL}
		attempts = append(attempts, endpoints...)
		attemptCandidates = append(attemptCandidates, candidateAttempt{candidate: candidate, baseURL: baseURL})
	}
	tracker.addTotal(len(attempts))
	var lastErr error
	for _, candidate := range attemptCandidates {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("CardDAV discovery timed out. Tried %s. Enter the provider's exact CardDAV base URL and try again", strings.Join(attempts, ", "))
		}
		books, _, err := discoverCardDAVAddressBooksCandidate(ctx, candidate.candidate, candidate.baseURL, username, password, func(endpoint string) {
			log.Printf("contacts carddav discover: probing %s", endpoint)
			tracker.start(endpoint)
		}, func() {
			tracker.complete()
		})
		if err == nil {
			return books, nil
		}
		log.Printf("contacts carddav discover: %s failed: %v", candidate.baseURL, err)
		lastErr = err
	}
	if len(attempts) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("Could not auto-discover CardDAV for this account. Tried %s, but none returned CardDAV account details. Enter the provider's CardDAV base URL, such as https://dav.example.com/ or https://mail.example.com/SOGo/dav/, then try again", strings.Join(attempts, ", "))
}

type cardDAVDiscoveryTracker struct {
	completed int
	total     int
	progress  cardDAVDiscoveryProgress
}

func (t *cardDAVDiscoveryTracker) addTotal(count int) {
	if t == nil {
		return
	}
	t.total += count
}

func (t *cardDAVDiscoveryTracker) start(endpoint string) {
	if t == nil || t.progress == nil {
		return
	}
	t.progress(t.completed, t.total, endpoint)
}

func (t *cardDAVDiscoveryTracker) complete() {
	if t == nil {
		return
	}
	t.completed++
}

func discoverCardDAVAddressBooksCandidate(ctx context.Context, rawBaseURL, baseURL, username, password string, beforeEndpoint func(endpoint string), afterEndpoint func()) ([]models.ContactAddressBook, string, error) {
	wellKnown := cardDAVWellKnownURL(baseURL)
	if beforeEndpoint != nil {
		beforeEndpoint(wellKnown)
	}
	principalURL, err := discoverCardDAVCurrentUserPrincipal(ctx, wellKnown, username, password)
	if afterEndpoint != nil {
		afterEndpoint()
	}
	used := wellKnown
	if err != nil || principalURL == "" {
		if beforeEndpoint != nil {
			beforeEndpoint(baseURL)
		}
		principalURL, err = discoverCardDAVCurrentUserPrincipal(ctx, baseURL, username, password)
		if afterEndpoint != nil {
			afterEndpoint()
		}
		used = baseURL
	}
	if err != nil {
		return nil, used, err
	}
	if principalURL == "" {
		return nil, used, fmt.Errorf("Could not auto-discover CardDAV for %s. The server did not return CardDAV account details", rawBaseURL)
	}
	principalURL = absoluteDAVHref(baseURL, principalURL)

	homeURL, err := discoverCardDAVAddressBookHome(ctx, principalURL, username, password)
	if err != nil {
		return nil, used, err
	}
	if homeURL == "" {
		return nil, used, fmt.Errorf("CardDAV server did not return an address book home")
	}
	homeURL = absoluteDAVHref(principalURL, homeURL)

	books, err := listCardDAVAddressBooks(ctx, homeURL, username, password)
	if err != nil {
		return nil, used, err
	}
	if len(books) == 0 {
		return nil, used, fmt.Errorf("no CardDAV address books found")
	}
	return books, used, nil
}

func expandCardDAVDiscoveryCandidates(ctx context.Context, candidates []string, tracker *cardDAVDiscoveryTracker) []string {
	seen := make(map[string]bool)
	expanded := make([]string, 0, len(candidates)*2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if seen[key] {
			return
		}
		seen[key] = true
		expanded = append(expanded, value)
	}

	for _, candidate := range candidates {
		domain := cardDAVDiscoveryDomain(candidate)
		if domain != "" {
			for _, discoveryDomain := range cardDAVDiscoveryDomains(domain) {
				for _, srvCandidate := range cardDAVSRVCandidates(ctx, discoveryDomain, tracker) {
					add(srvCandidate)
				}
				if discoveryDomain != domain {
					add(discoveryDomain)
				}
			}
		}
		add(candidate)
	}
	return expanded
}

func cardDAVDiscoveryDomains(domain string) []string {
	domain = strings.Trim(strings.TrimSpace(domain), ".")
	if domain == "" {
		return nil
	}
	domains := []string{domain}
	parts := strings.Split(domain, ".")
	if len(parts) > 2 {
		parent := strings.Join(parts[len(parts)-2:], ".")
		if parent != domain {
			domains = append(domains, parent)
		}
	}
	return domains
}

func cardDAVDiscoveryDomain(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if strings.Contains(candidate, "@") && !strings.Contains(candidate, "://") {
		parts := strings.Split(candidate, "@")
		return strings.Trim(strings.TrimSpace(parts[len(parts)-1]), ".")
	}
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	return strings.Trim(strings.TrimSpace(host), ".")
}

func cardDAVSRVCandidates(ctx context.Context, domain string, tracker *cardDAVDiscoveryTracker) []string {
	type record struct {
		scheme string
		name   string
	}
	records := []record{{scheme: "https", name: "carddavs"}, {scheme: "http", name: "carddav"}}
	candidates := make([]string, 0)
	for _, record := range records {
		select {
		case <-ctx.Done():
			return candidates
		default:
		}
		lookupName := "_" + record.name + "._tcp." + domain
		tracker.addTotal(1)
		tracker.start("DNS SRV " + lookupName)
		lookupCtx, cancel := context.WithTimeout(ctx, cardDAVDiscoveryDNSTimeout)
		_, addrs, err := net.DefaultResolver.LookupSRV(lookupCtx, record.name, "tcp", domain)
		cancel()
		tracker.complete()
		if err != nil {
			log.Printf("contacts carddav discover: DNS SRV _%s._tcp.%s failed: %v", record.name, domain, err)
			continue
		}
		pathHint := cardDAVTXTPathHint(ctx, lookupName, tracker)
		for _, addr := range addrs {
			host := strings.Trim(strings.TrimSpace(addr.Target), ".")
			if host == "" {
				continue
			}
			base := record.scheme + "://" + host
			defaultPort := uint16(443)
			if record.scheme == "http" {
				defaultPort = 80
			}
			if addr.Port != defaultPort {
				base += ":" + strconv.Itoa(int(addr.Port))
			}
			if pathHint != "" {
				candidates = append(candidates, joinCardDAVURLPath(base, pathHint))
			} else {
				candidates = append(candidates, base)
			}
			log.Printf("contacts carddav discover: DNS SRV _%s._tcp.%s -> %s", record.name, domain, candidates[len(candidates)-1])
		}
	}
	return candidates
}

func cardDAVTXTPathHint(ctx context.Context, name string, tracker *cardDAVDiscoveryTracker) string {
	select {
	case <-ctx.Done():
		return ""
	default:
	}
	tracker.addTotal(1)
	tracker.start("DNS TXT " + name)
	lookupCtx, cancel := context.WithTimeout(ctx, cardDAVDiscoveryDNSTimeout)
	texts, err := net.DefaultResolver.LookupTXT(lookupCtx, name)
	cancel()
	tracker.complete()
	if err != nil {
		log.Printf("contacts carddav discover: DNS TXT %s failed: %v", name, err)
		return ""
	}
	for _, text := range texts {
		for _, part := range strings.Fields(text) {
			key, value, ok := strings.Cut(part, "=")
			if !ok || !strings.EqualFold(key, "path") {
				continue
			}
			value = strings.Trim(value, `"`)
			if strings.HasPrefix(value, "/") {
				log.Printf("contacts carddav discover: DNS TXT %s -> path=%s", name, value)
				return value
			}
		}
	}
	return ""
}

func joinCardDAVURLPath(baseURL, pathHint string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	parsed.Path = pathHint
	return parsed.String()
}

func discoverCardDAVCurrentUserPrincipal(ctx context.Context, endpoint, username, password string) (string, error) {
	multi, err := cardDAVPropfind(ctx, endpoint, username, password, "0", `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`)
	if err != nil {
		return "", err
	}
	for _, response := range multi.Responses {
		prop := response.okProp()
		if strings.TrimSpace(prop.CurrentUserPrincipal.Href) != "" {
			return strings.TrimSpace(prop.CurrentUserPrincipal.Href), nil
		}
	}
	return "", nil
}

func discoverCardDAVAddressBookHome(ctx context.Context, principalURL, username, password string) (string, error) {
	multi, err := cardDAVPropfind(ctx, principalURL, username, password, "0", `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><card:addressbook-home-set/></d:prop></d:propfind>`)
	if err != nil {
		return "", err
	}
	for _, response := range multi.Responses {
		prop := response.okProp()
		if strings.TrimSpace(prop.AddressBookHomeSet.Href) != "" {
			return strings.TrimSpace(prop.AddressBookHomeSet.Href), nil
		}
	}
	return "", nil
}

func listCardDAVAddressBooks(ctx context.Context, homeURL, username, password string) ([]models.ContactAddressBook, error) {
	multi, err := cardDAVPropfind(ctx, homeURL, username, password, "1", `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cs="http://calendarserver.org/ns/"><d:prop><d:displayname/><d:resourcetype/><cs:getctag/></d:prop></d:propfind>`)
	if err != nil {
		return nil, err
	}
	books := make([]models.ContactAddressBook, 0)
	seen := make(map[string]bool)
	for _, response := range multi.Responses {
		prop := response.okProp()
		if !prop.ResourceType.AddressBook {
			continue
		}
		bookURL := absoluteDAVHref(homeURL, response.Href)
		if bookURL == "" || seen[bookURL] {
			continue
		}
		seen[bookURL] = true
		name := strings.TrimSpace(prop.DisplayName)
		if name == "" {
			name = cardDAVAddressBookName(bookURL)
		}
		books = append(books, models.ContactAddressBook{Name: name, URL: bookURL})
	}
	return books, nil
}

func cardDAVPropfind(ctx context.Context, endpoint, username, password, depth, body string) (davMultiStatus, error) {
	req, err := newCardDAVRequest(ctx, "PROPFIND", endpoint, username, password, strings.NewReader(body))
	if err != nil {
		return davMultiStatus{}, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := (&http.Client{Timeout: cardDAVDiscoveryHTTPTimeout}).Do(req)
	if err != nil {
		return davMultiStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return davMultiStatus{}, fmt.Errorf("CardDAV discovery returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	multi, err := decodeDAVMultiStatus(resp.Body)
	if err != nil {
		return davMultiStatus{}, err
	}
	return multi, nil
}

func decodeDAVMultiStatus(r io.Reader) (davMultiStatus, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return davMultiStatus{}, err
	}
	var multi davMultiStatus
	if err := xml.Unmarshal(data, &multi); err == nil {
		return multi, nil
	}
	data = escapeInvalidXMLAmpersands(data)
	multi = davMultiStatus{}
	if err := xml.Unmarshal(data, &multi); err != nil {
		return davMultiStatus{}, err
	}
	return multi, nil
}

func escapeInvalidXMLAmpersands(data []byte) []byte {
	if !bytes.Contains(data, []byte("&")) {
		return data
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] != '&' {
			out = append(out, data[i])
			continue
		}
		if xmlEntityEnd(data, i+1) > i {
			out = append(out, '&')
			continue
		}
		out = append(out, '&', 'a', 'm', 'p', ';')
	}
	return out
}

func xmlEntityEnd(data []byte, start int) int {
	if start >= len(data) {
		return -1
	}
	for i := start; i < len(data) && i-start <= 32; i++ {
		switch data[i] {
		case ';':
			entity := string(data[start:i])
			if entity == "amp" || entity == "lt" || entity == "gt" || entity == "apos" || entity == "quot" || validNumericXMLEntity(entity) {
				return i
			}
			return -1
		case ' ', '\t', '\n', '\r', '<', '>', '&':
			return -1
		}
	}
	return -1
}

func validNumericXMLEntity(entity string) bool {
	if strings.HasPrefix(entity, "#x") || strings.HasPrefix(entity, "#X") {
		if len(entity) <= 2 {
			return false
		}
		for _, r := range entity[2:] {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(entity, "#") {
		if len(entity) <= 1 {
			return false
		}
		for _, r := range entity[1:] {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}

func normalizeCardDAVBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("CardDAV base URL is required")
	}
	if strings.Contains(raw, "@") && !strings.Contains(raw, "://") {
		parts := strings.Split(raw, "@")
		raw = parts[len(parts)-1]
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("enter a valid http or https CardDAV base URL")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func cardDAVWellKnownURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	parsed.Path = "/.well-known/carddav"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func cardDAVAddressBookName(bookURL string) string {
	parsed, err := url.Parse(bookURL)
	if err != nil {
		return "Address book"
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[len(parts)-1]) == "" {
		return "Address book"
	}
	name, err := url.PathUnescape(parts[len(parts)-1])
	if err != nil || strings.TrimSpace(name) == "" {
		return "Address book"
	}
	return name
}

func cardDAVPut(ctx context.Context, cfg models.ContactSyncConfig, password, remoteID, etag string, body []byte) (string, error) {
	req, err := newCardDAVRequest(ctx, http.MethodPut, remoteID, cfg.Username, password, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	} else {
		req.Header.Set("If-None-Match", "*")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", cardDAVHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	return resp.Header.Get("ETag"), nil
}

func cardDAVDelete(ctx context.Context, cfg models.ContactSyncConfig, password, remoteID, etag string) error {
	if strings.TrimSpace(remoteID) == "" {
		return nil
	}
	req, err := newCardDAVRequest(ctx, http.MethodDelete, remoteID, cfg.Username, password, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(etag) != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return cardDAVHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func newCardDAVRequest(ctx context.Context, method, endpoint, username, password string, body io.Reader) (*http.Request, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("CardDAV URL is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("enter a valid http or https CardDAV URL")
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(username) != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	return req, nil
}

func (r davResponse) addressData() string {
	for _, propStat := range r.PropStats {
		if strings.Contains(propStat.Status, " 200 ") && strings.TrimSpace(propStat.Prop.AddressData) != "" {
			return propStat.Prop.AddressData
		}
	}
	for _, propStat := range r.PropStats {
		if strings.TrimSpace(propStat.Prop.AddressData) != "" {
			return propStat.Prop.AddressData
		}
	}
	return ""
}

func (r davResponse) deleted() bool {
	status := strings.ToLower(strings.TrimSpace(r.Status))
	if strings.Contains(status, " 404 ") || strings.Contains(status, " 410 ") {
		return true
	}
	for _, propStat := range r.PropStats {
		status = strings.ToLower(strings.TrimSpace(propStat.Status))
		if strings.Contains(status, " 404 ") || strings.Contains(status, " 410 ") {
			return true
		}
	}
	return false
}

func (r davResponse) okProp() davProp {
	for _, propStat := range r.PropStats {
		if strings.Contains(propStat.Status, " 200 ") {
			return propStat.Prop
		}
	}
	if len(r.PropStats) > 0 {
		return r.PropStats[0].Prop
	}
	return davProp{}
}

func (r davResponse) etag() string {
	for _, propStat := range r.PropStats {
		if strings.TrimSpace(propStat.Prop.GetETag) != "" {
			return strings.TrimSpace(propStat.Prop.GetETag)
		}
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func isCardDAVStatus(err error, statuses ...int) bool {
	if err == nil {
		return false
	}
	var davErr cardDAVHTTPError
	if typed, ok := err.(cardDAVHTTPError); ok {
		davErr = typed
	} else {
		return false
	}
	for _, status := range statuses {
		if davErr.Status == status {
			return true
		}
	}
	return false
}

func xmlEscapeText(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func absoluteDAVHref(baseURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if parsed, err := url.Parse(href); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.String()
	}
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

func newCardDAVContactHref(addressBookURL string) string {
	base, err := url.Parse(strings.TrimSpace(addressBookURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return strings.TrimRight(addressBookURL, "/") + "/" + uuid.NewString() + ".vcf"
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.Path = path.Join(base.Path, uuid.NewString()+".vcf")
	return base.String()
}

func formValueBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
