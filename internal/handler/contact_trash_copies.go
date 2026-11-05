package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Moving a contact to the Trash deletes it where it lives: its copies on
// accounts are deleted first, then it goes to Gofer's Trash, which keeps its
// home, its Local copy and its Gofer Sync places. Restoring it writes it
// back there. A contact an account refuses to delete stays out of the Trash.

// trashContactsEverywhere deletes each contact's copies on accounts and
// moves it to the Trash. It gives how many went, and the ones that didn't.
func (h *Handler) trashContactsEverywhere(ctx context.Context, owner string, ids []string) (int, []contactDeleteFailure, error) {
	trashed := 0
	var failed []contactDeleteFailure
	err := h.runContactWork(ctx, owner, func(ctx context.Context) error {
		for _, id := range ids {
			if err := h.removeContactCopies(ctx, owner, id, nil); err != nil {
				failed = append(failed, contactDeleteFailure{ID: id, Error: contactDeleteFailureMessage(id, err)})
				continue
			}
			var count int
			var err error
			if h.userStorage == nil {
				count, err = h.db.TrashContacts(ctx, owner, []string{id})
			} else {
				count, err = h.userAccounts.TrashContacts(ctx, owner, []string{id})
			}
			if err != nil {
				failed = append(failed, contactDeleteFailure{ID: id, Error: contactDeleteFailureMessage(id, err)})
				continue
			}
			trashed += count
		}
		return nil
	})
	return trashed, failed, err
}

// removeContactCopies deletes a contact's copies on accounts, those in the
// given places only when there are any, and keeps it in Gofer with its Local
// copy. It runs as the user's contact work.
func (h *Handler) removeContactCopies(ctx context.Context, owner, id string, only map[string]bool) error {
	in := func(accountID, bookID string) bool {
		return len(only) == 0 || only[contactCardPlace(accountID, bookID)]
	}
	if h.userStorage == nil {
		contact, err := h.db.GetContact(ctx, owner, id)
		if err != nil || contact == nil {
			return err
		}
		var deleteErr error
		if len(only) == 0 {
			deleteErr = h.deleteContactFromAccounts(ctx, owner, *contact)
		} else {
			deleteErr = h.deleteContactFromAccountsIn(ctx, owner, *contact, func(source storage.ContactSource) bool {
				return in(source.AccountID, source.AddressBookID)
			})
		}
		if deleteErr != nil {
			return contactSyncDeleteError{deleteErr}
		}
		return nil
	}
	snapshot, err := h.userAccounts.SnapshotContactDelete(ctx, owner, id)
	if err != nil {
		return err
	}
	var cards []models.ContactCard
	for _, card := range snapshot.Sources() {
		if card.Kind != "provider" || card.AccountID == "" || !in(card.AccountID, card.AddressBookID) {
			continue
		}
		if err := validateUserContactDeleteSource(snapshot, card.ID); err != nil {
			return err
		}
		cards = append(cards, card)
	}
	for _, card := range cards {
		cardID := card.ID
		err = h.userIMAP.RunAccountService(ctx, owner, card.AccountID, mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
			if err := h.userAccounts.ValidateContactDelete(ctx, snapshot); err != nil {
				return err
			}
			if card.RemoteID != "" {
				if err := h.deleteUserContactSource(ctx, snapshot, cardID); err != nil {
					return userContactPreviewProviderError{err}
				}
			}
			next, err := h.userAccounts.AcknowledgeContactDeleteSource(ctx, snapshot, cardID)
			if err == nil {
				snapshot = next
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// contactCardPlace is the place a copy is in: its address book, else its
// account.
func contactCardPlace(accountID, bookID string) string {
	if bookID != "" {
		return "book:" + bookID
	}
	return "account:" + accountID
}

// contactSyncPlaces gives the places a contact has Gofer Sync on for.
func (h *Handler) contactSyncPlaces(ctx context.Context, owner, id string) (map[string]bool, error) {
	places := map[string]bool{}
	if id == "" {
		return places, nil
	}
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		memberships, err := db.ListContactSyncMemberships(ctx, owner, id)
		for _, membership := range memberships {
			if membership.Enabled {
				places[contactCardPlace(membership.AccountID, membership.AddressBookID)] = true
			}
		}
		return err
	})
	return places, err
}

// removeDroppedSyncPlaces deletes a contact's copies in the places taken
// out of its Gofer Sync: they no longer hold it. Its home keeps its copy.
func (h *Handler) removeDroppedSyncPlaces(ctx context.Context, owner, id, home string, before map[string]bool) error {
	if len(before) == 0 {
		return nil
	}
	after, err := h.contactSyncPlaces(ctx, owner, id)
	if err != nil {
		return err
	}
	dropped := map[string]bool{}
	for place := range before {
		if !after[place] && place != home {
			dropped[place] = true
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	return h.removeContactCopies(ctx, owner, id, dropped)
}

// writeBackRestoredContacts writes contacts back from the Trash to where
// they live: to every place with Gofer Sync on, else to an account home.
func (h *Handler) writeBackRestoredContacts(ctx context.Context, owner string, ids []string) {
	queued := false
	for _, id := range ids {
		contact, err := h.loadPlacedContact(ctx, owner, id)
		if err != nil || contact == nil {
			continue
		}
		home := contact.StoredIn
		switch {
		case contact.GoferSyncEnabled:
			if h.userStorage == nil {
				queued = h.scheduleContactAccountSync(ctx, owner, *contact, nil) || queued
			} else if op, err := h.userAccounts.EnqueueContactSyncForUser(ctx, owner, id); err == nil && op != "" {
				queued = true
			}
		case home != "" && home != "local":
			var err error
			if h.userStorage == nil {
				_, err = h.db.EnqueueContactStoreOperation(ctx, owner, *contact, home)
			} else {
				_, err = h.userAccounts.EnqueueContactStore(ctx, owner, id, home)
			}
			if err != nil {
				log.Printf("contacts: write restored contact %s back: %v", id, err)
				continue
			}
			queued = true
		}
	}
	if queued {
		h.wakeContactOperations(ctx, owner)
	}
}

// handleContactTrashPlaces names the accounts that hold copies of the
// posted contacts, which moving them to the Trash deletes there.
func (h *Handler) handleContactTrashPlaces(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactSelectionLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	counts := map[string]int{}
	withCopies := 0
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		accounts, err := db.GetAccounts(ctx, owner)
		if err != nil {
			return err
		}
		for _, id := range ids {
			profile, err := db.GetContactProfile(ctx, owner, id)
			if err != nil {
				return err
			}
			if profile == nil || profile.IsDeleted {
				continue
			}
			seen := map[string]bool{}
			for _, card := range profile.Cards {
				if card.IsDeleted || card.Kind != "provider" || card.AccountID == "" {
					continue
				}
				place := "account:" + card.AccountID
				if card.AddressBookID != "" {
					place = "book:" + card.AddressBookID
				}
				label := views.ContactPlaceLabel(ctx, place, card.AccountID, accounts)
				if !seen[label] {
					seen[label] = true
					counts[label]++
				}
			}
			if len(seen) > 0 {
				withCopies++
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, i18n.T(ctx, "server.error.contacts_unavailable"), http.StatusServiceUnavailable)
		return
	}
	places := make([]string, 0, len(counts))
	for label := range counts {
		places = append(places, label)
	}
	sort.Strings(places)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"places": places, "contacts": withCopies})
}
