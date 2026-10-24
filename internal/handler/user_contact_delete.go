package handler

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// deleteUserContact deletes an owned contact for good, in the Trash or not:
// from each address book it syncs with, under that account's contacts
// service, then from Gofer. It gives the deleted contact's id; a
// known-invalid source stops it before any address book is touched.
func (h *Handler) deleteUserContact(ctx context.Context, owner, id string) (deleted string, err error) {
	snapshot, err := h.userAccounts.SnapshotContactDelete(ctx, owner, id)
	if err != nil {
		return "", err
	}
	// Reject any already-known invalid endpoint before deleting valid sources.
	// Network failures can still leave partial remote progress for retry.
	for _, card := range snapshot.Sources() {
		if err := validateUserContactDeleteSource(snapshot, card.ID); err != nil {
			return "", err
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
			return "", err
		}
	}
	deleted, err = h.userAccounts.FinishContactDelete(ctx, snapshot)
	if err == nil {
		if wakeErr := h.WakeUserContactQueue(ctx, owner); wakeErr != nil && ctx.Err() == nil {
			log.Printf("owned contacts: wake committed delete queue cancellation: %v", wakeErr)
		}
	}
	return deleted, err
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
	case "jmap":
		// A JMAP card id is opaque; it belongs to the account it came from.
		if service.ContactConfig().Provider == providers.ProviderJMAP {
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
	if card.Provider == "jmap" {
		if err := validateUserContactDeleteSource(snapshot, cardID); err != nil {
			return err
		}
		account, err := service.JMAPConfig(ctx)
		if err != nil {
			return err
		}
		password, err := service.MailboxPassword()
		if err != nil {
			return err
		}
		if err := pull.ready(ctx); err != nil {
			return err
		}
		return pull.recordRetry(ctx, mail.DeleteJMAPContact(ctx, account, password, card.RemoteID))
	}
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
	client := &http.Client{Transport: cardDAVHTTPTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("contact provider delete redirected") }}
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
