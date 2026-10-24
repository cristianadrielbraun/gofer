package mail

import (
	"context"
	"errors"
	"maps"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// PushJMAPContact writes a contact to its card in a JMAP address book (a JMAP
// id) and gives the card's id. remoteID is the card Gofer synced before, if
// any. A new card gets uid, so when the reply to its creation was lost, the
// next try finds that card instead of making a second one.
func PushJMAPContact(ctx context.Context, cfg *models.AccountConfig, password, book, remoteID, uid string, contact models.Contact) (string, error) {
	if book == "" {
		return "", errors.New("no JMAP address book to save the contact in")
	}
	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	pusher := jmapContactPusher{client: client, session: session, account: account}
	var card *jmap.ContactCard
	if remoteID != "" {
		if card, err = pusher.card(ctx, remoteID); err != nil {
			return "", err
		}
	}
	if card == nil && uid != "" {
		ids, err := client.FindContactCards(ctx, session, account, uid)
		if err != nil {
			return "", err
		}
		if len(ids) > 0 {
			if card, err = pusher.card(ctx, ids[0]); err != nil {
				return "", err
			}
		}
	}
	if card != nil {
		updated, err := pusher.update(ctx, *card, book, contact)
		if err != nil || updated {
			return card.ID, err
		}
		// The card was destroyed after Gofer read it; make it again.
	}
	id, err := pusher.create(ctx, book, uid, contact)
	var failure *jmap.SetError
	if errors.As(err, &failure) && failure.ExistingID != "" {
		// The server has a card with this uid that its search didn't find
		// yet; that card is the one to update.
		existing, getErr := pusher.card(ctx, failure.ExistingID)
		if getErr != nil || existing == nil {
			return "", errors.Join(err, getErr)
		}
		if _, err := pusher.update(ctx, *existing, book, contact); err != nil {
			return "", err
		}
		return existing.ID, nil
	}
	return id, err
}

// DeleteJMAPContact destroys a card. One that is already gone counts as
// deleted.
func DeleteJMAPContact(ctx context.Context, cfg *models.AccountConfig, password, remoteID string) error {
	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		return err
	}
	result, err := client.SetContactCards(ctx, session, jmap.ContactCardSet{AccountID: account, Destroy: []string{remoteID}})
	if err != nil {
		return err
	}
	if failure, ok := result.NotDestroyed[remoteID]; ok && failure.Type != "notFound" {
		return &failure
	}
	return nil
}

type jmapContactPusher struct {
	client  *jmap.Client
	session *jmap.Session
	account string
}

// card fetches one card, or nil when the server no longer has it.
func (p jmapContactPusher) card(ctx context.Context, id string) (*jmap.ContactCard, error) {
	cards, err := p.client.GetContactCards(ctx, p.session, p.account, []string{id})
	if err != nil {
		return nil, err
	}
	for _, card := range cards.List {
		if card.ID == id {
			return &card, nil
		}
	}
	return nil, nil
}

// update patches the card's details that differ from the contact and puts it
// in the book if it has left it. It reports false when the card is gone.
func (p jmapContactPusher) update(ctx context.Context, card jmap.ContactCard, book string, contact models.Contact) (bool, error) {
	patch := JMAPContactPatch(card, contact)
	if !card.AddressBookIDs[book] {
		maps.Copy(patch, jmap.AddressBookPatch(book))
	}
	if len(patch) == 0 {
		return true, nil
	}
	result, err := p.client.SetContactCards(ctx, p.session, jmap.ContactCardSet{AccountID: p.account, Update: map[string]jmap.PatchObject{card.ID: patch}})
	if err != nil {
		return false, err
	}
	if failure, ok := result.NotUpdated[card.ID]; ok {
		if failure.Type == "notFound" {
			return false, nil
		}
		return false, &failure
	}
	return true, nil
}

func (p jmapContactPusher) create(ctx context.Context, book, uid string, contact models.Contact) (string, error) {
	card := JMAPCardFromContact(contact)
	card.UID = uid
	card.AddressBookIDs = map[string]bool{book: true}
	result, err := p.client.SetContactCards(ctx, p.session, jmap.ContactCardSet{AccountID: p.account, Create: map[string]jmap.ContactCard{"new": card}})
	if err != nil {
		return "", err
	}
	if failure, ok := result.NotCreated["new"]; ok {
		return "", &failure
	}
	created := result.Created["new"].ID
	if created == "" {
		return "", errors.New("ContactCard/set: the server gave the new card no id")
	}
	return created, nil
}
