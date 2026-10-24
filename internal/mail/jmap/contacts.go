package jmap

import (
	"context"
	"encoding/json"
	"fmt"
)

// CapabilityContacts is JMAP for Contacts (RFC 9610).
const CapabilityContacts = "urn:ietf:params:jmap:contacts"

// AddressBook is RFC 9610 section 2.
type AddressBook struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	SortOrder    int               `json:"sortOrder"`
	IsDefault    bool              `json:"isDefault"`
	IsSubscribed bool              `json:"isSubscribed"`
	MyRights     AddressBookRights `json:"myRights"`
}

type AddressBookRights struct {
	MayRead   bool `json:"mayRead"`
	MayWrite  bool `json:"mayWrite"`
	MayShare  bool `json:"mayShare"`
	MayDelete bool `json:"mayDelete"`
}

// ContactCard is the part of a JSContact Card (RFC 9553) that Gofer reads,
// plus the JMAP addressBookIds (RFC 9610 section 3). Properties left out here
// stay on the server untouched, since Gofer updates cards with patches.
type ContactCard struct {
	ID             string                         `json:"id,omitempty"`
	UID            string                         `json:"uid,omitempty"`
	Kind           string                         `json:"kind,omitempty"`
	AddressBookIDs map[string]bool                `json:"addressBookIds,omitempty"`
	Name           *ContactName                   `json:"name,omitempty"`
	Emails         map[string]ContactEmail        `json:"emails,omitempty"`
	Phones         map[string]ContactPhone        `json:"phones,omitempty"`
	Organizations  map[string]ContactOrganization `json:"organizations,omitempty"`
	Titles         map[string]ContactTitle        `json:"titles,omitempty"`
	Notes          map[string]ContactNote         `json:"notes,omitempty"`
	Media          map[string]ContactMedia        `json:"media,omitempty"`
}

// ContactName is a JSContact Name. Full is the whole name as shown; the
// components hold its parts, such as {"kind": "given", "value": "Ada"}.
type ContactName struct {
	Full       string                 `json:"full,omitempty"`
	Components []ContactNameComponent `json:"components,omitempty"`
}

type ContactNameComponent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ContactEmail is a JSContact EmailAddress. Contexts are "work" or "private";
// Pref ranks the addresses from 1 (most preferred), with 0 meaning unset.
type ContactEmail struct {
	Address  string          `json:"address"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     int             `json:"pref,omitempty"`
	Label    string          `json:"label,omitempty"`
}

// ContactPhone is a JSContact Phone. Features say what kind of line it is,
// such as "mobile", "voice" or "fax".
type ContactPhone struct {
	Number   string          `json:"number"`
	Features map[string]bool `json:"features,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     int             `json:"pref,omitempty"`
	Label    string          `json:"label,omitempty"`
}

type ContactOrganization struct {
	Name string `json:"name,omitempty"`
}

// ContactTitle is a job title, or a role when Kind is "role".
type ContactTitle struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type ContactNote struct {
	Note string `json:"note"`
}

// ContactMedia is a JSContact Media. A server gives a photo either as a URI
// or as a blob to download (RFC 9610 section 3).
type ContactMedia struct {
	Kind      string `json:"kind"`
	URI       string `json:"uri,omitempty"`
	BlobID    string `json:"blobId,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Pref      int    `json:"pref,omitempty"`
}

// GetAddressBooks runs AddressBook/get for every address book.
func (c *Client) GetAddressBooks(ctx context.Context, session *Session, accountID string) (*GetResponse[AddressBook], error) {
	args := struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{accountID, nil}
	responses, err := c.Call(ctx, session, []string{CapabilityContacts},
		Invocation{Name: "AddressBook/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[AddressBook]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("AddressBook/get: %w", err)
	}
	return &result, nil
}

// GetContactCards runs ContactCard/get. A nil ids fetches every card in the
// account; the server may refuse that for a large account, so a full listing
// should query the ids first and fetch them in pages of MaxObjectsInGet.
func (c *Client) GetContactCards(ctx context.Context, session *Session, accountID string, ids []string) (*GetResponse[ContactCard], error) {
	args := struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{accountID, ids}
	responses, err := c.Call(ctx, session, []string{CapabilityContacts},
		Invocation{Name: "ContactCard/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[ContactCard]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("ContactCard/get: %w", err)
	}
	return &result, nil
}

// QueryContactCards runs ContactCard/query for the ids of every card, from
// position on, at most limit of them (0 leaves it to the server).
func (c *Client) QueryContactCards(ctx context.Context, session *Session, accountID string, position, limit int) (*QueryResponse, error) {
	args := struct {
		AccountID      string `json:"accountId"`
		Position       int    `json:"position,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		CalculateTotal bool   `json:"calculateTotal"`
	}{accountID, position, limit, true}
	responses, err := c.Call(ctx, session, []string{CapabilityContacts},
		Invocation{Name: "ContactCard/query", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result QueryResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("ContactCard/query: %w", err)
	}
	return &result, nil
}

// FindContactCards runs ContactCard/query for the ids of the cards with the
// given uid, which a well-behaved server keeps to one per address book.
func (c *Client) FindContactCards(ctx context.Context, session *Session, accountID, uid string) ([]string, error) {
	args := struct {
		AccountID string            `json:"accountId"`
		Filter    map[string]string `json:"filter"`
	}{accountID, map[string]string{"uid": uid}}
	responses, err := c.Call(ctx, session, []string{CapabilityContacts},
		Invocation{Name: "ContactCard/query", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result QueryResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("ContactCard/query: %w", err)
	}
	return result.IDs, nil
}

// AddressBookPatch puts a card in an address book, leaving it in any others.
func AddressBookPatch(book string) PatchObject {
	return PatchObject{"addressBookIds/" + escapePointer(book): true}
}

// GetContactCardChanges runs ContactCard/changes. Keep calling with NewState
// while HasMoreChanges is true.
func (c *Client) GetContactCardChanges(ctx context.Context, session *Session, accountID, sinceState string, maxChanges int) (*ChangesResponse, error) {
	var result ChangesResponse
	if err := c.getChanges(ctx, session, CapabilityContacts, "ContactCard/changes", changesArgs{accountID, sinceState, maxChanges}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ContactCardSet are the ContactCard/set arguments: new cards by creation id,
// patches by card id, and cards to destroy.
type ContactCardSet struct {
	AccountID string                 `json:"accountId"`
	Create    map[string]ContactCard `json:"create,omitempty"`
	Update    map[string]PatchObject `json:"update,omitempty"`
	Destroy   []string               `json:"destroy,omitempty"`
}

// MarshalJSON adds the JSContact @type and version every new Card needs.
func (s ContactCardSet) MarshalJSON() ([]byte, error) {
	type newCard struct {
		Type    string `json:"@type"`
		Version string `json:"version"`
		ContactCard
	}
	var create map[string]newCard
	for creationID, card := range s.Create {
		if create == nil {
			create = make(map[string]newCard, len(s.Create))
		}
		create[creationID] = newCard{"Card", "1.0", card}
	}
	type plain ContactCardSet
	return json.Marshal(struct {
		plain
		Create map[string]newCard `json:"create,omitempty"`
	}{plain(s), create})
}

// ContactCardSetResponse adds the created cards, which hold the ids the
// server gave them, to the standard /set result.
type ContactCardSetResponse struct {
	SetResponse
	Created    map[string]ContactCard `json:"created"`
	NotCreated map[string]SetError    `json:"notCreated"`
}

// SetContactCards runs ContactCard/set.
func (c *Client) SetContactCards(ctx context.Context, session *Session, args ContactCardSet) (*ContactCardSetResponse, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityContacts},
		Invocation{Name: "ContactCard/set", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result ContactCardSetResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("ContactCard/set: %w", err)
	}
	return &result, nil
}
