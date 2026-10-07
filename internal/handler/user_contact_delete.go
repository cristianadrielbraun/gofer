package handler

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserDeleteContact(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	var deleted string
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactDelete(ctx, owner, id)
		if err != nil {
			return err
		}
		// Reject any already-known invalid endpoint before deleting valid sources.
		// Network failures can still leave partial remote progress for retry.
		for _, card := range snapshot.Sources() {
			if err := validateUserContactDeleteSource(snapshot, card.ID); err != nil {
				return err
			}
		}
		for _, card := range snapshot.Sources() {
			deleteSource := func(ctx context.Context) error {
				if err := h.userAccounts.ValidateContactDelete(ctx, snapshot); err != nil {
					return err
				}
				if card.RemoteID != "" {
					if err := h.deleteUserContactSource(ctx, snapshot, card.ID); err != nil {
						return userContactPreviewProviderError{err}
					}
				}
				var err error
				snapshot, err = h.userAccounts.AcknowledgeContactDeleteSource(ctx, snapshot, card.ID)
				return err
			}
			if card.AccountID == "" {
				err = deleteSource(ctx)
			} else {
				err = h.userIMAP.RunAccountService(ctx, owner, card.AccountID, mail.AccountServiceContacts, 2*time.Minute, deleteSource)
			}
			if err != nil {
				return err
			}
		}
		deleted, err = h.userAccounts.FinishContactDelete(ctx, snapshot)
		if err == nil {
			if wakeErr := h.WakeUserContactQueue(ctx, owner); wakeErr != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed delete queue cancellation: %v", wakeErr)
			}
		}
		return err
	})
	if err != nil && deleted == "" {
		var provider userContactPreviewProviderError
		if errors.As(err, &provider) && !errors.Is(err, storage.ErrContactEditChanged) && !errors.Is(err, config.ErrAccountServicesChanged) && !errors.Is(err, storage.ErrAccountRoute) && !errors.Is(err, storage.ErrUserStoreOwner) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Contact sync delete failed: "+err.Error(), http.StatusBadGateway)
		} else {
			userContactEditError(w, r, err)
		}
		return
	}
	http.Redirect(w, r, "/contacts", http.StatusSeeOther)
}

func validateUserContactDeleteSource(snapshot *config.UserContactDeleteSnapshot, cardID string) error {
	card, service, err := snapshot.Source(cardID)
	if err != nil || card.RemoteID == "" {
		return err
	}
	if service == nil {
		return storage.ErrAccountRoute
	}
	switch card.Provider {
	case "gmail":
		if validOwnedGoogleContactID(card.RemoteID) {
			return nil
		}
	case "outlook":
		if validOwnedGraphContactID(card.RemoteID) {
			return nil
		}
	case "carddav":
		_, book := cardDAVConfigForSource(service.ContactConfig(), &storage.ContactSource{AddressBookID: card.AddressBookID, RemoteID: card.RemoteID})
		if book.URL != "" && storage.ContactRemoteInAddressBook(book.URL, card.RemoteID) {
			return nil
		}
	}
	return storage.ErrAccountRoute
}

func (h *Handler) deleteUserContactSource(ctx context.Context, snapshot *config.UserContactDeleteSnapshot, cardID string) error {
	card, service, err := snapshot.Source(cardID)
	if err != nil {
		return err
	}
	if service == nil {
		return storage.ErrAccountRoute
	}
	pull := &userContactPull{h: h, snapshot: service, deletion: snapshot}
	if card.Provider == "carddav" {
		cfg := service.ContactConfig()
		source := &storage.ContactSource{ContactID: card.ProfileID, UserID: card.UserID, Provider: card.Provider, AccountID: card.AccountID, AddressBookID: card.AddressBookID, RemoteID: card.RemoteID, Etag: card.Etag}
		bookCfg, book := cardDAVConfigForSource(cfg, source)
		if book.URL == "" || !storage.ContactRemoteInAddressBook(book.URL, card.RemoteID) {
			return storage.ErrAccountRoute
		}
		password, err := service.ContactSyncPassword("")
		if err != nil {
			return err
		}
		if err := pull.ready(ctx); err != nil {
			return err
		}
		err = userContactDAVDelete(ctx, bookCfg, password, card.RemoteID, card.Etag)
		return pull.recordRetry(ctx, err)
	}
	if h.userCredentials == nil {
		return errors.New("mailbox OAuth is unavailable")
	}
	pull.credentials = h.userCredentials.ContactsAccount(service.OwnerID(), service.AccountID())
	if card.Provider == "gmail" {
		if !validOwnedGoogleContactID(card.RemoteID) {
			return storage.ErrAccountRoute
		}
		err := pull.json(ctx, http.MethodDelete, googlePeopleAPIBaseURL+"/"+card.RemoteID+":deleteContact", nil, nil)
		var api googleAPIError
		if isGoogleNotFound(err, &api) {
			return nil
		}
		return err
	}
	if !validOwnedGraphContactID(card.RemoteID) {
		return storage.ErrAccountRoute
	}
	err = pull.json(ctx, http.MethodDelete, outlookGraphBaseURL+"/me/contacts/"+url.PathEscape(card.RemoteID), nil, nil)
	var api outlookAPIError
	if isOutlookNotFound(err, &api) {
		return nil
	}
	return err
}

func userContactDAVDelete(ctx context.Context, cfg models.ContactSyncConfig, password, remote, etag string) error {
	req, err := newCardDAVRequest(ctx, http.MethodDelete, remote, cfg.Username, password, nil)
	if err != nil {
		return err
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("contact provider delete redirected") }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return err
		}
		return cardDAVHTTPError{Status: resp.StatusCode, Body: string(body), RetryAt: providerRetryAfter(resp)}
	}
	return nil
}
