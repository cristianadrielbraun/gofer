package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errJMAPContactBooksMissing = errors.New("no JMAP address book is selected for this account")

// jmapContactBooks gives the JMAP ids of the synced address books, which are
// saved where CardDAV keeps a book's URL, and maps each to Gofer's book id.
func jmapContactBooks(cfg models.ContactSyncConfig) ([]string, map[string]string) {
	ids := make([]string, 0, len(cfg.AddressBooks))
	gofer := make(map[string]string, len(cfg.AddressBooks))
	for _, book := range cfg.AddressBooks {
		if id := strings.TrimSpace(book.URL); id != "" {
			ids = append(ids, id)
			gofer[id] = book.ID
		}
	}
	return ids, gofer
}

// jmapInboundContacts turns a pulled page into contacts to store, each in
// Gofer's copy of its address book.
func jmapInboundContacts(page *mail.JMAPContactsPage, books map[string]string) []storage.InboundContact {
	out := make([]storage.InboundContact, 0, len(page.Contacts))
	for _, card := range page.Contacts {
		out = append(out, storage.InboundContact{Contact: card.Contact, RemoteID: card.ID, AddressBookID: books[card.AddressBookID]})
	}
	return out
}

// jmap pulls a JMAP account's contacts for the per-user stores.
func (p *userContactPull) jmap(ctx context.Context) (int, error) {
	cfg := p.snapshot.ContactConfig()
	ids, books := jmapContactBooks(cfg)
	if len(ids) == 0 {
		return 0, errJMAPContactBooksMissing
	}
	account, err := p.snapshot.JMAPConfig(ctx)
	if err != nil {
		return 0, err
	}
	password, err := p.snapshot.MailboxPassword()
	if err != nil {
		return 0, err
	}
	if err := p.ready(ctx); err != nil {
		return 0, err
	}
	page, err := mail.PullJMAPContacts(ctx, account, password, cfg.LastSyncToken, ids)
	if err != nil {
		return 0, p.recordRetry(ctx, err)
	}
	results, next, err := p.h.userAccounts.PublishInboundJMAPContacts(ctx, p.snapshot, storage.InboundJMAPContactPage{
		Contacts:         jmapInboundContacts(page, books),
		DeletedRemoteIDs: page.Removed,
		SeenRemoteIDs:    page.Seen,
		Full:             page.Full,
		SyncToken:        page.State,
	})
	if err != nil {
		return 0, err
	}
	p.snapshot = next
	p.wakeFanout(ctx, results)
	return len(results), nil
}

// syncJMAPContacts pulls a JMAP account's contacts for the shared store, as
// syncCardDAVContacts does for a CardDAV one.
func (h *Handler) syncJMAPContacts(ctx context.Context, userID string, account contactSyncAccount) (int, error) {
	if !providers.JMAPEnabled() {
		return 0, providers.ErrJMAPDisabled
	}
	cfg, err := h.accountStore.GetContactSyncConfig(ctx, userID, account.ID)
	if err != nil {
		return 0, err
	}
	ids, books := jmapContactBooks(cfg)
	if len(ids) == 0 {
		return 0, errJMAPContactBooksMissing
	}
	mailCfg, err := h.accountStore.GetConfig(ctx, account.ID)
	if err != nil {
		return 0, err
	}
	if mailCfg.Provider != providers.ProviderJMAP {
		return 0, errors.New("the account does not use JMAP")
	}
	password, err := h.accountStore.DecryptPassword(ctx, account.ID)
	if err != nil {
		return 0, err
	}
	page, err := mail.PullJMAPContacts(ctx, mailCfg, password, cfg.LastSyncToken, ids)
	if err != nil {
		return 0, err
	}

	imported := 0
	for _, input := range jmapInboundContacts(page, books) {
		contactID, canonicalChanged, err := h.upsertInboundSyncedContact(ctx, userID, providers.ProviderJMAP, account.ID, input.RemoteID, input.Contact)
		if err != nil {
			return imported, err
		}
		if contactID == "" {
			continue
		}
		if err := h.db.UpsertContactSource(ctx, storage.ContactSource{
			ContactID: contactID, UserID: userID, Provider: providers.ProviderJMAP,
			AccountID: account.ID, AddressBookID: input.AddressBookID, RemoteID: input.RemoteID,
		}); err != nil {
			return imported, err
		}
		if canonicalChanged {
			if err := h.scheduleInboundContactFanout(ctx, userID, contactID, account.ID); err != nil {
				return imported, err
			}
		}
		imported++
	}
	for _, remoteID := range page.Removed {
		if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderJMAP, account.ID, remoteID); err != nil {
			return imported, err
		}
	}
	if page.Full {
		seen := make(map[string]bool, len(page.Seen))
		for _, remoteID := range page.Seen {
			seen[remoteID] = true
		}
		sources, err := h.db.ListContactSourcesForAccount(ctx, userID, providers.ProviderJMAP, account.ID)
		if err != nil {
			return imported, err
		}
		for _, source := range sources {
			if remoteID := strings.TrimSpace(source.RemoteID); remoteID != "" && !seen[remoteID] {
				if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, providers.ProviderJMAP, account.ID, remoteID); err != nil {
					return imported, err
				}
			}
		}
	}
	return imported, h.accountStore.MarkContactSyncSuccess(ctx, userID, account.ID, page.State)
}

// jmapContactSetupRequested reports whether a contacts setup form asks for
// JMAP contacts rather than CardDAV.
func jmapContactSetupRequested(r *http.Request) bool {
	return strings.TrimSpace(r.FormValue("provider")) == providers.ProviderJMAP
}

// jmapFormAddressBooks lists a JMAP account's address books the way CardDAV
// discovery does, with the book id where a CardDAV book has its URL. Only the
// default book starts selected.
func jmapFormAddressBooks(books []jmap.AddressBook) []models.ContactAddressBook {
	out := make([]models.ContactAddressBook, 0, len(books))
	for _, book := range books {
		if !book.MyRights.MayRead && book.MyRights != (jmap.AddressBookRights{}) {
			continue
		}
		out = append(out, models.ContactAddressBook{URL: book.ID, Name: book.Name, Default: book.IsDefault, Selected: book.IsDefault})
	}
	return out
}

// jmapChosenAddressBooks keeps the books chosen in the form that the server
// still has, named as the server names them.
func jmapChosenAddressBooks(r *http.Request, available []jmap.AddressBook) []models.ContactAddressBook {
	names := make(map[string]string, len(available))
	for _, book := range available {
		names[book.ID] = book.Name
	}
	var chosen []models.ContactAddressBook
	for _, book := range contactSyncAddressBooksFromForm(r, "") {
		if name, ok := names[book.URL]; ok {
			book.Name = name
			chosen = append(chosen, book)
		}
	}
	return chosen
}

// writeJMAPContactBooks answers a discovery request with a JMAP account's
// address books, streamed like CardDAV discovery when the client asks for it.
func writeJMAPContactBooks(w http.ResponseWriter, r *http.Request, books []jmap.AddressBook, err error) {
	if !strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"address_books": jmapFormAddressBooks(books)})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	encoder := json.NewEncoder(w)
	_ = encoder.Encode(map[string]any{"type": "start", "message": "Looking up JMAP address books..."})
	if err != nil {
		_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	_ = encoder.Encode(map[string]any{"type": "done", "address_books": jmapFormAddressBooks(books)})
}

// jmapContactAccount is a shared-store JMAP account's connection and password.
func (h *Handler) jmapContactAccount(ctx context.Context, accountID string) (*models.AccountConfig, string, error) {
	if !providers.JMAPEnabled() {
		return nil, "", providers.ErrJMAPDisabled
	}
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return nil, "", err
	}
	if cfg.Provider != providers.ProviderJMAP || cfg.AuthMethod != "plain" {
		return nil, "", errors.New("contacts over JMAP need a JMAP account")
	}
	password, err := h.accountStore.DecryptPassword(ctx, accountID)
	return cfg, password, err
}

func (h *Handler) discoverJMAPContactBooks(w http.ResponseWriter, r *http.Request, accountID string) {
	ctx, cancel := context.WithTimeout(r.Context(), cardDAVDiscoveryOverallTimeout)
	defer cancel()
	cfg, password, err := h.jmapContactAccount(ctx, accountID)
	var books []jmap.AddressBook
	if err == nil {
		books, err = mail.JMAPAddressBooks(ctx, cfg, password)
	}
	writeJMAPContactBooks(w, r, books, err)
}

// saveJMAPContactSync saves contacts over JMAP for the shared store: the
// chosen address books, checked against the server, or off when none are.
func (h *Handler) saveJMAPContactSync(w http.ResponseWriter, r *http.Request, userID, accountID string) {
	ctx := r.Context()
	cfg := models.ContactSyncConfig{AccountID: accountID, UserID: userID, Provider: providers.ProviderJMAP}
	if len(contactSyncAddressBooksFromForm(r, "")) > 0 {
		mailCfg, password, err := h.jmapContactAccount(ctx, accountID)
		var available []jmap.AddressBook
		if err == nil {
			available, err = mail.JMAPAddressBooks(ctx, mailCfg, password)
		}
		if err != nil {
			htmlStatus(w, http.StatusBadRequest, "JMAP contacts save failed: "+err.Error())
			return
		}
		cfg.AddressBooks = jmapChosenAddressBooks(r, available)
		if len(cfg.AddressBooks) == 0 {
			htmlStatus(w, http.StatusBadRequest, "Choose at least one JMAP address book.")
			return
		}
		cfg.Enabled = true
	}
	if err := h.accountStore.SaveContactSyncConfig(ctx, userID, accountID, cfg, ""); err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not save contact sync settings.")
		return
	}
	w.Header().Set("X-Gofer-Contact-Sync-Enabled", strconv.FormatBool(cfg.Enabled))
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

// discoverUserJMAPContactBooks lists a JMAP account's address books for the
// per-user stores.
func (h *Handler) discoverUserJMAPContactBooks(w http.ResponseWriter, r *http.Request, owner, id string) {
	var books []jmap.AddressBook
	var lookupErr error
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceContacts, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		cfg, err := snapshot.JMAPConfig(ctx)
		if err != nil {
			lookupErr = err
			return nil
		}
		password, err := snapshot.MailboxPassword()
		if err != nil {
			return err
		}
		books, lookupErr = mail.JMAPAddressBooks(ctx, cfg, password)
		return h.userAccounts.ValidateContactServices(ctx, snapshot)
	})
	if err != nil {
		userContactSetupError(w, r, err)
		return
	}
	writeJMAPContactBooks(w, r, books, lookupErr)
}

// saveUserJMAPContactSync is saveJMAPContactSync for the per-user stores.
func (h *Handler) saveUserJMAPContactSync(w http.ResponseWriter, r *http.Request, owner, id string) {
	cfg := models.ContactSyncConfig{AccountID: id, UserID: owner, Provider: providers.ProviderJMAP}
	inputMessage := ""
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		if len(contactSyncAddressBooksFromForm(r, "")) > 0 {
			mailCfg, err := snapshot.JMAPConfig(ctx)
			if err != nil {
				inputMessage = "JMAP contacts save failed: " + err.Error()
				return nil
			}
			password, err := snapshot.MailboxPassword()
			if err != nil {
				return err
			}
			available, err := mail.JMAPAddressBooks(ctx, mailCfg, password)
			if err != nil {
				inputMessage = "JMAP contacts save failed: " + err.Error()
				return h.userAccounts.ValidateContactServices(ctx, snapshot)
			}
			cfg.AddressBooks = jmapChosenAddressBooks(r, available)
			if len(cfg.AddressBooks) == 0 {
				inputMessage = "Choose at least one JMAP address book."
				return nil
			}
			cfg.Enabled = true
		}
		return h.userAccounts.SaveContactServices(ctx, snapshot, cfg, "")
	})
	if err != nil {
		userContactSetupError(w, r, err)
		return
	}
	if inputMessage != "" {
		htmlStatus(w, http.StatusBadRequest, inputMessage)
		return
	}
	if err := h.WakeUserContactAccount(r.Context(), owner, id); err != nil {
		htmlStatus(w, http.StatusServiceUnavailable, "Contact settings saved; worker refresh failed, retry saving.")
		return
	}
	w.Header().Set("X-Gofer-Contact-Sync-Enabled", strconv.FormatBool(cfg.Enabled))
	if !cfg.Enabled {
		htmlStatus(w, http.StatusOK, "Contact sync is disabled for this account.")
		return
	}
	htmlContactSyncSaved(w, id, cfg)
}
