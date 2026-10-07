package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func contactTestConfig(r *http.Request, owner, id string) models.ContactSyncConfig {
	cfg := models.ContactSyncConfig{AccountID: id, UserID: owner, Provider: providers.ProviderCardDAV, Enabled: true,
		BaseURL: strings.TrimSpace(r.FormValue("base_url")), AddressBookURL: strings.TrimSpace(r.FormValue("addressbook_url")), Username: strings.TrimSpace(r.FormValue("username"))}
	cfg.AddressBooks = contactSyncAddressBooksFromForm(r, cfg.AddressBookURL)
	if cfg.AddressBookURL == "" && len(cfg.AddressBooks) == 0 {
		cfg.AddressBookURL = cfg.BaseURL
		cfg.AddressBooks = contactSyncAddressBooksFromURL(cfg.AddressBookURL)
	}
	return cfg
}
func userContactSetupError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, config.ErrAccountServicesChanged) {
		http.Error(w, "Account settings changed; retry the connection test or discovery.", http.StatusConflict)
		return
	}
	userAccountError(w, r, err)
}

func parseUserContactSetupForm(w http.ResponseWriter, r *http.Request) error {
	const limit = 64 << 10
	if r.ContentLength > limit {
		return errors.New("contact setup form is too large")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return r.ParseForm()
}

func (h *Handler) handleUserSaveAccountContactSync(w http.ResponseWriter, r *http.Request) {
	if err := parseUserContactSetupForm(w, r); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid contact sync settings.")
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	cfg := contactTestConfig(r, owner, id)
	password := r.FormValue("password")
	cfg.Enabled = cfg.BaseURL != "" || len(cfg.AddressBooks) > 0 || cfg.Username != "" || password != ""
	inputMessage := ""
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		if cfg.Enabled {
			if cfg.Username == "" || r.FormValue("use_account_credentials") == "1" {
				identity := snapshot.Identity()
				cfg.Username = identity.Username
				if cfg.Username == "" {
					cfg.Username = identity.EmailAddress
				}
			}
			if len(cfg.AddressBooks) == 0 || cfg.Username == "" {
				inputMessage = "Choose at least one CardDAV address book and enter a username."
				return nil
			}
			password, err = snapshot.ContactSyncPassword(password)
			if err != nil || password == "" {
				inputMessage = "CardDAV password is required."
				return nil
			}
			for _, book := range cfg.AddressBooks {
				bookCfg := cfg
				bookCfg.AddressBookURL = book.URL
				if err := testCardDAVAddressBook(ctx, bookCfg, password); err != nil {
					inputMessage = "CardDAV save failed: could not connect to " + contactAddressBookLabel(book) + ": " + err.Error()
					return h.userAccounts.ValidateContactServices(ctx, snapshot)
				}
			}
		}
		return h.userAccounts.SaveContactServices(ctx, snapshot, cfg, password)
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

func (h *Handler) handleUserTestAccountContactSync(w http.ResponseWriter, r *http.Request) {
	if err := parseUserContactSetupForm(w, r); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid contact sync test.")
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	var result string
	inputStatus := 0
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceContacts, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		cfg := contactTestConfig(r, owner, id)
		password, err := snapshot.ContactSyncPassword(r.FormValue("password"))
		if err != nil || password == "" {
			inputStatus = http.StatusBadRequest
			result = "Enter the CardDAV password before testing."
			return nil
		}
		if len(cfg.AddressBooks) == 0 {
			inputStatus = http.StatusBadRequest
			result = "Choose at least one CardDAV address book before testing."
			return nil
		}
		result = "CardDAV connection succeeded."
		for _, book := range cfg.AddressBooks {
			bookCfg := cfg
			bookCfg.AddressBookURL = book.URL
			if err := testCardDAVAddressBook(ctx, bookCfg, password); err != nil {
				result = "CardDAV test failed: " + err.Error()
				break
			}
		}
		return h.userAccounts.ValidateContactServices(ctx, snapshot)
	})
	if err != nil {
		userContactSetupError(w, r, err)
		return
	}
	if inputStatus != 0 {
		htmlStatus(w, inputStatus, result)
		return
	}
	htmlStatus(w, http.StatusOK, result)
}

func (h *Handler) handleUserDiscoverAccountContactSync(w http.ResponseWriter, r *http.Request) {
	if err := parseUserContactSetupForm(w, r); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid discovery request")
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	streaming := strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
	streamed := false
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceContacts, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		identity := snapshot.Identity()
		baseURL := strings.TrimSpace(r.FormValue("base_url"))
		if baseURL == "" {
			baseURL = strings.TrimSpace(r.FormValue("addressbook_url"))
		}
		username := strings.TrimSpace(r.FormValue("username"))
		if username == "" {
			username = strings.TrimSpace(identity.Username)
			if username == "" {
				username = strings.TrimSpace(identity.EmailAddress)
			}
		}
		autodiscover := r.FormValue("autodiscover") == "1"
		candidates := contactSyncDiscoveryCandidates(baseURL)
		if autodiscover {
			candidates = contactSyncDiscoveryCandidates(baseURL, identity.IMAPHost, identity.SMTPHost, username, identity.EmailAddress)
		} else if len(candidates) == 0 {
			writeJSONError(w, http.StatusBadRequest, "enter a CardDAV base URL, or use Attempt URL autodiscover")
			return nil
		}
		password, err := snapshot.ContactSyncPassword(r.FormValue("password"))
		if err != nil || password == "" {
			writeJSONError(w, http.StatusBadRequest, "enter the CardDAV password before discovery")
			return nil
		}
		if streaming {
			streamed = true
			return h.streamUserCardDAVDiscovery(w, ctx, snapshot, candidates, username, password, autodiscover)
		}
		books, discoveryErr := discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, nil, autodiscover)
		if err := h.userAccounts.ValidateContactServices(ctx, snapshot); err != nil {
			return err
		}
		if discoveryErr != nil {
			writeJSONError(w, http.StatusBadRequest, discoveryErr.Error())
			return nil
		}
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(map[string]any{"address_books": books})
	})
	if err != nil && !streamed {
		userContactSetupError(w, r, err)
	}
}

func (h *Handler) streamUserCardDAVDiscovery(w http.ResponseWriter, parent context.Context, snapshot *config.AccountServiceSnapshot, candidates []string, username, password string, autodiscover bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	var writeErr error
	writeEvent := func(event map[string]any) {
		if writeErr != nil {
			return
		}
		writeErr = encoder.Encode(event)
		if writeErr != nil {
			cancel()
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeEvent(map[string]any{"type": "start", "message": "Preparing CardDAV discovery..."})
	books, discoveryErr := discoverCardDAVAddressBooksCandidates(ctx, candidates, username, password, func(completed, total int, endpoint string) {
		if ctx.Err() != nil {
			return
		}
		writeEvent(map[string]any{"type": "progress", "completed": completed, "total": total, "endpoint": endpoint})
	}, autodiscover)
	if writeErr != nil {
		return writeErr
	}
	if err := h.userAccounts.ValidateContactServices(ctx, snapshot); err != nil {
		writeEvent(map[string]any{"type": "error", "error": "Account settings changed or the connection was cancelled; retry discovery."})
		return err
	}
	if discoveryErr != nil {
		writeEvent(map[string]any{"type": "error", "error": discoveryErr.Error()})
		return writeErr
	}
	writeEvent(map[string]any{"type": "done", "address_books": books})
	if writeErr != nil {
		return fmt.Errorf("write CardDAV discovery: %w", writeErr)
	}
	return nil
}
