package mail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ErrJMAPNoContacts means the login reaches the JMAP server but is not
// offered contacts, as with an API token made without the contacts
// permission or a server without JMAP for Contacts.
var ErrJMAPNoContacts = errors.New("the JMAP server doesn't offer contacts for this login; if you signed in with an API token, create one that also has the contacts permission")

const (
	// jmapContactPageSize is how many cards are fetched per request when the
	// server sets no lower limit.
	jmapContactPageSize = 500
	// jmapContactPhotoMaxBytes caps a downloaded contact photo; larger ones
	// are left out rather than stored.
	jmapContactPhotoMaxBytes = 1 << 20
)

// JMAPContact is one card from a JMAP address book Gofer syncs.
// AddressBookID is the JMAP id of the first synced book the card is in.
type JMAPContact struct {
	ID            string
	AddressBookID string
	Contact       models.Contact
}

// JMAPContactsPage is what changed in an account's synced address books.
// Contacts holds the new or changed cards; Removed the cards destroyed or no
// longer in a synced book. Full means the whole account was listed instead,
// and every card not in Seen is gone. State is where the next pull starts.
type JMAPContactsPage struct {
	Contacts []JMAPContact
	Removed  []string
	Seen     []string
	Full     bool
	State    string
}

// JMAPAddressBooks lists the address books of a JMAP account.
func JMAPAddressBooks(ctx context.Context, cfg *models.AccountConfig, password string) ([]jmap.AddressBook, error) {
	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	books, err := client.GetAddressBooks(ctx, session, account)
	if err != nil {
		return nil, err
	}
	return books.List, nil
}

// CreateJMAPAddressBook makes an address book named name on the account's
// JMAP server and returns its id.
func CreateJMAPAddressBook(ctx context.Context, cfg *models.AccountConfig, password, name string) (string, error) {
	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	return client.CreateAddressBook(ctx, session, account, name)
}

// PullJMAPContacts fetches the cards in the given address books (JMAP ids)
// that changed since sinceState, or all of them when sinceState is empty or
// the server can no longer tell what changed since then.
func PullJMAPContacts(ctx context.Context, cfg *models.AccountConfig, password, sinceState string, books []string) (*JMAPContactsPage, error) {
	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	puller := jmapContactPuller{client: client, session: session, account: account, books: books}
	if sinceState != "" {
		page, err := puller.changes(ctx, sinceState)
		if !jmap.IsCannotCalculateChanges(err) {
			return page, err
		}
	}
	return puller.full(ctx)
}

func jmapContactsSession(ctx context.Context, cfg *models.AccountConfig, password string) (*jmap.Client, *jmap.Session, string, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, nil, "", err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("JMAP session: %w", jmapLoginError(err))
	}
	account := session.PrimaryAccount(jmap.CapabilityContacts)
	if _, ok := session.Capabilities[jmap.CapabilityContacts]; !ok || account == "" {
		return nil, nil, "", ErrJMAPNoContacts
	}
	return client, session, account, nil
}

type jmapContactPuller struct {
	client  *jmap.Client
	session *jmap.Session
	account string
	books   []string
}

// full lists every card. The state is read before the listing, so a card
// changed while it runs is fetched again by the next pull rather than missed.
func (p jmapContactPuller) full(ctx context.Context) (*JMAPContactsPage, error) {
	current, err := p.client.GetContactCards(ctx, p.session, p.account, []string{})
	if err != nil {
		return nil, err
	}
	if current.State == "" {
		return nil, errors.New("ContactCard/get: the server sent no state")
	}
	page := &JMAPContactsPage{Full: true, State: current.State}
	limit := p.pageSize()
	for position := 0; ; {
		query, err := p.client.QueryContactCards(ctx, p.session, p.account, position, limit)
		if err != nil {
			return nil, err
		}
		if err := p.fetch(ctx, query.IDs, page); err != nil {
			return nil, err
		}
		position += len(query.IDs)
		// A server that leaves out the total is read until a page is empty.
		if len(query.IDs) == 0 || (query.Total > 0 && position >= query.Total) {
			return page, nil
		}
	}
}

func (p jmapContactPuller) changes(ctx context.Context, sinceState string) (*JMAPContactsPage, error) {
	page := &JMAPContactsPage{State: sinceState}
	var changed, destroyed []string
	for {
		changes, err := p.client.GetContactCardChanges(ctx, p.session, p.account, page.State, 0)
		if err != nil {
			return nil, err
		}
		if changes.NewState == "" {
			return nil, errors.New("ContactCard/changes: the server sent no new state")
		}
		changed = append(changed, changes.Created...)
		changed = append(changed, changes.Updated...)
		destroyed = append(destroyed, changes.Destroyed...)
		page.State = changes.NewState
		if !changes.HasMoreChanges || changes.NewState == changes.OldState {
			break
		}
	}
	changed = slices.DeleteFunc(dedupeStrings(changed), func(id string) bool { return slices.Contains(destroyed, id) })
	page.Removed = dedupeStrings(destroyed)
	limit := p.pageSize()
	for start := 0; start < len(changed); start += limit {
		if err := p.fetch(ctx, changed[start:min(start+limit, len(changed))], page); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// fetch reads cards into the page: those in a synced book as contacts (or
// as seen, when Gofer can't use them), the rest as removed, since a card
// moved out of every synced book is gone as far as Gofer is concerned.
func (p jmapContactPuller) fetch(ctx context.Context, ids []string, page *JMAPContactsPage) error {
	if len(ids) == 0 {
		return nil
	}
	cards, err := p.client.GetContactCards(ctx, p.session, p.account, ids)
	if err != nil {
		return err
	}
	page.Removed = append(page.Removed, cards.NotFound...)
	for _, card := range cards.List {
		book := p.bookOf(card)
		if book == "" {
			page.Removed = append(page.Removed, card.ID)
			continue
		}
		page.Seen = append(page.Seen, card.ID)
		contact, photo, ok := JMAPContactFromCard(card)
		if !ok {
			continue
		}
		if contact.AvatarURL == "" && photo.BlobID != "" {
			contact.AvatarURL = p.photoDataURL(ctx, photo)
		}
		page.Contacts = append(page.Contacts, JMAPContact{ID: card.ID, AddressBookID: book, Contact: contact})
	}
	return nil
}

func (p jmapContactPuller) bookOf(card jmap.ContactCard) string {
	for _, book := range p.books {
		if card.AddressBookIDs[book] {
			return book
		}
	}
	return ""
}

func (p jmapContactPuller) pageSize() int {
	if limit := p.session.Core.MaxObjectsInGet; limit > 0 && limit < jmapContactPageSize {
		return limit
	}
	return jmapContactPageSize
}

// photoDataURL downloads a photo blob as a data URL. A photo that fails to
// download, isn't an image or is too large is left out; the rest of the
// contact still syncs.
func (p jmapContactPuller) photoDataURL(ctx context.Context, photo jmap.ContactMedia) string {
	mediaType := strings.ToLower(strings.TrimSpace(photo.MediaType))
	if !strings.HasPrefix(mediaType, "image/") {
		return ""
	}
	body, err := p.client.DownloadBlob(ctx, p.session, p.account, photo.BlobID, "photo", mediaType)
	if err != nil {
		return ""
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, jmapContactPhotoMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > jmapContactPhotoMaxBytes {
		return ""
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}
