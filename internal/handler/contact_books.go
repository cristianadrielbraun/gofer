package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// contactAccountBooks is what the contacts sidebar needs to change which of
// an account's address books Gofer syncs: the saved connection, which the
// account's contact sync form takes again, the books it syncs, and the ones
// the server has besides.
type contactAccountBooks struct {
	Provider string                      `json:"provider"`
	BaseURL  string                      `json:"base_url,omitempty"`
	Username string                      `json:"username,omitempty"`
	Synced   []models.ContactAddressBook `json:"synced"`
	Other    []models.ContactAddressBook `json:"other"`
}

// handleContactAccountBooks asks an account's server which address books it
// has, with the saved contact sync settings, for the sidebar's Other address
// books. Nothing is stored: the list is as current as the server.
func (h *Handler) handleContactAccountBooks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), cardDAVDiscoveryOverallTimeout)
	defer cancel()
	userID, accountID := h.userID(ctx), r.PathValue("id")
	if h.userStorage == nil && !h.requireOwnedAccount(w, r, accountID) {
		return
	}
	var result contactAccountBooks
	var found []models.ContactAddressBook
	var lookupErr error
	if h.userStorage != nil {
		err := h.userIMAP.RunAccountService(ctx, userID, accountID, mail.AccountServiceContacts, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
			snapshot, err := h.userAccounts.SnapshotServices(ctx, userID, accountID)
			if err != nil {
				return err
			}
			cfg := snapshot.ContactConfig()
			result = contactAccountBooksFrom(cfg)
			if cfg.Provider == providers.ProviderJMAP {
				mailCfg, err := snapshot.JMAPConfig(ctx)
				if err != nil {
					lookupErr = err
					return nil
				}
				password, err := snapshot.MailboxPassword()
				if err != nil {
					return err
				}
				found, lookupErr = jmapBooksOnServer(ctx, mailCfg, password)
				return nil
			}
			password, err := snapshot.ContactSyncPassword("")
			if err != nil || password == "" {
				lookupErr = err
				return nil
			}
			found, lookupErr = cardDAVBooksOnServer(ctx, cfg, snapshot.Identity().Username, snapshot.Identity().EmailAddress, password)
			return nil
		})
		if err != nil {
			userContactSetupError(w, r, err)
			return
		}
	} else {
		cfg, err := h.accountStore.GetContactSyncConfig(ctx, userID, accountID)
		if err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "account is required")
			return
		}
		result = contactAccountBooksFrom(cfg)
		if cfg.Provider == providers.ProviderJMAP {
			mailCfg, password, err := h.jmapAccountConnection(ctx, accountID)
			if err == nil {
				found, err = jmapBooksOnServer(ctx, mailCfg, password)
			}
			lookupErr = err
		} else {
			email, username, _, _, _, err := h.contactSyncAccountIdentity(ctx, userID, accountID)
			password, passErr := h.contactSyncPasswordOrAccount(ctx, userID, accountID, "")
			if err == nil {
				err = passErr
			}
			if err == nil {
				found, err = cardDAVBooksOnServer(ctx, cfg, username, email, password)
			}
			lookupErr = err
		}
	}
	if lookupErr != nil {
		writeJSONError(w, r, http.StatusBadGateway, i18n.T(r.Context(), "server.error.contact_books_lookup_failed"))
		return
	}
	synced := make(map[string]bool, len(result.Synced))
	for _, book := range result.Synced {
		synced[strings.TrimSpace(book.URL)] = true
	}
	result.Other = []models.ContactAddressBook{}
	for _, book := range found {
		if !synced[strings.TrimSpace(book.URL)] {
			result.Other = append(result.Other, models.ContactAddressBook{URL: book.URL, Name: book.Name})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func contactAccountBooksFrom(cfg models.ContactSyncConfig) contactAccountBooks {
	result := contactAccountBooks{Provider: cfg.Provider, Synced: cfg.AddressBooks}
	if result.Provider == "" {
		result.Provider = providers.ProviderCardDAV
	}
	if result.Provider != providers.ProviderJMAP {
		result.BaseURL, result.Username = strings.TrimSpace(cfg.BaseURL), strings.TrimSpace(cfg.Username)
	}
	if result.Synced == nil {
		result.Synced = []models.ContactAddressBook{}
	}
	return result
}

// cardDAVBooksOnServer lists the address books of the saved CardDAV server.
func cardDAVBooksOnServer(ctx context.Context, cfg models.ContactSyncConfig, accountUsername, accountEmail, password string) ([]models.ContactAddressBook, error) {
	username := strings.TrimSpace(cfg.Username)
	if username == "" {
		username = strings.TrimSpace(accountUsername)
	}
	if username == "" {
		username = strings.TrimSpace(accountEmail)
	}
	candidates := contactSyncDiscoveryCandidates(cfg.BaseURL, cfg.AddressBookURL)
	return discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, nil, false, nil)
}

// jmapBooksOnServer lists a JMAP account's address books, as its contact sync
// form names them.
func jmapBooksOnServer(ctx context.Context, cfg *models.AccountConfig, password string) ([]models.ContactAddressBook, error) {
	var books []jmap.AddressBook
	books, err := mail.JMAPAddressBooks(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	return jmapFormAddressBooks(books), nil
}

// handleContactBookUsage says how the contacts use an address book, for the
// confirmation before Gofer stops syncing it.
func (h *Handler) handleContactBookUsage(w http.ResponseWriter, r *http.Request) {
	userID := h.userID(r.Context())
	var usage storage.ContactBookUsage
	if err := h.withUserDB(r.Context(), userID, func(db *storage.DB) error {
		var err error
		usage, err = db.ContactAddressBookUsage(r.Context(), userID, r.PathValue("id"))
		return err
	}); err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "server.error.save_failed"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(usage)
}

// handleContactBookMovers lists the contacts living in an address book that
// also live in other places, with those places, for the user to choose
// where each goes before Gofer stops syncing it.
func (h *Handler) handleContactBookMovers(w http.ResponseWriter, r *http.Request) {
	userID := h.userID(r.Context())
	var movers []storage.ContactBookMover
	if err := h.withUserDB(r.Context(), userID, func(db *storage.DB) error {
		var err error
		movers, err = db.ContactAddressBookMovers(r.Context(), userID, r.PathValue("id"))
		return err
	}); err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "server.error.save_failed"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(movers)
}

// handleReleaseContactBook lets go of the copies of an address book Gofer no
// longer syncs, keeping in Gofer Local or removing the contacts that lived
// only there, as chosen. The sidebar calls it once the book is out of the
// account's synced books.
func (h *Handler) handleReleaseContactBook(w http.ResponseWriter, r *http.Request) {
	userID := h.userID(r.Context())
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "server.error.save_failed"))
		return
	}
	release := storage.ContactBookRelease(r.FormValue("release"))
	// The new main location chosen for each contact that lives elsewhere
	// too, as home:<contact>=<place>.
	homes := map[string]string{}
	for key, values := range r.PostForm {
		if id, ok := strings.CutPrefix(key, "home:"); ok && id != "" && len(values) > 0 {
			homes[id] = values[0]
		}
	}
	var moved, removed int
	err := h.withUserDB(r.Context(), userID, func(db *storage.DB) error {
		var err error
		moved, removed, err = db.ReleaseContactAddressBook(r.Context(), userID, r.PathValue("id"), release, homes)
		return err
	})
	if err != nil {
		writeJSONError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "server.error.save_failed"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"moved": moved, "removed": removed})
}
