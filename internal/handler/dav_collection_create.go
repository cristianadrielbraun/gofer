package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// davCreate asks discovery to make a new calendar or address book, named
// Name, in the home collection the server reports, before listing them.
// Created is the new collection's URL once the server has made it.
type davCreate struct {
	Name    string
	Created string
	// Target, for an address book, is one to rename to Name instead, or with
	// Delete set to delete with its contacts, by its URL or JMAP id.
	Target string
	Delete bool
	// attempted is set once the server was asked, so discovery doesn't go
	// on to make another one on the next server it tries.
	attempted bool
}

const davCreateNameMaxLength = 100

// davCreateFromForm is the calendar or address book a discovery request asks
// to create, from its create_name field, or nil when it asks for none.
func davCreateFromForm(r *http.Request) (*davCreate, error) {
	name := strings.TrimSpace(r.FormValue("create_name"))
	if name == "" {
		return nil, nil
	}
	if err := checkCollectionName(name); err != nil {
		return nil, err
	}
	return &davCreate{Name: name}, nil
}

// davChangeFromForm is davCreateFromForm for address books, which can also
// be renamed (rename_target, rename_name) or deleted (delete_target).
func davChangeFromForm(r *http.Request) (*davCreate, error) {
	if target := strings.TrimSpace(r.FormValue("delete_target")); target != "" {
		return &davCreate{Target: target, Delete: true}, nil
	}
	if target := strings.TrimSpace(r.FormValue("rename_target")); target != "" {
		name := strings.TrimSpace(r.FormValue("rename_name"))
		if name == "" {
			return nil, errors.New("Enter a name.")
		}
		if err := checkCollectionName(name); err != nil {
			return nil, err
		}
		return &davCreate{Target: target, Name: name}, nil
	}
	return davCreateFromForm(r)
}

// changeDAVAddressBook renames or deletes the address book change targets,
// which must be in home, and records it as done.
func changeDAVAddressBook(ctx context.Context, home, username, password string, change *davCreate) error {
	target := absoluteDAVHref(home, change.Target)
	if !strings.HasPrefix(target, strings.TrimRight(home, "/")+"/") || davSameCollection(target, home) {
		return errors.New("that isn't an address book on this server")
	}
	change.attempted = true
	if change.Delete {
		return deleteDAVCollection(ctx, cardDAVHTTPTransport, target, username, password, "address book")
	}
	return renameDAVCollection(ctx, cardDAVHTTPTransport, target, username, password, change.Name, "address book")
}

// withoutAddressBook is cfg with the address book at target left out, and
// whether it had it. Contact sync turns off when no book is left.
func withoutAddressBook(cfg models.ContactSyncConfig, target string) (models.ContactSyncConfig, bool) {
	found := false
	books := make([]models.ContactAddressBook, 0, len(cfg.AddressBooks))
	for _, book := range cfg.AddressBooks {
		if book.URL == target || davSameCollection(book.URL, target) {
			found = true
			continue
		}
		books = append(books, book)
	}
	if cfg.AddressBookURL == target || davSameCollection(cfg.AddressBookURL, target) {
		cfg.AddressBookURL, found = "", true
	}
	cfg.AddressBooks = books
	if len(books) == 0 {
		cfg.Enabled = false
	}
	return cfg, found
}

// checkCollectionName refuses a calendar or address book name that is too
// long or has line breaks or control characters.
func checkCollectionName(name string) error {
	if len([]rune(name)) > davCreateNameMaxLength {
		return fmt.Errorf("Use a name of at most %d characters.", davCreateNameMaxLength)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("The name can't have line breaks or control characters.")
	}
	return nil
}

// davNewCollectionURL is a new collection's URL in a home collection, under
// a random name, as DAVx5 and other clients do.
func davNewCollectionURL(home string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(home))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("the server returned an invalid home URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + uuid.NewString() + "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// davSameCollection reports whether two URLs name the same collection,
// whether or not they end in a slash.
func davSameCollection(a, b string) bool {
	left, leftErr := url.Parse(a)
	right, rightErr := url.Parse(b)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return strings.EqualFold(left.Host, right.Host) && strings.TrimRight(left.Path, "/") == strings.TrimRight(right.Path, "/")
}

// createCalDAVCalendar makes a calendar named name in a CalDAV calendar home
// with MKCALENDAR and returns its URL. The server picks which kinds of items
// it holds, as it does for calendars made in other clients.
func createCalDAVCalendar(ctx context.Context, home, username, password, name string) (string, error) {
	if err := calendarDiscoveryDAVGuard(ctx); err != nil {
		return "", err
	}
	target, err := davNewCollectionURL(home)
	if err != nil {
		return "", err
	}
	body := `<?xml version="1.0" encoding="utf-8"?><c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><d:displayname>` + xmlEscapeText(name) + `</d:displayname></d:prop></d:set></c:mkcalendar>`
	if err := davCreateCollection(ctx, calDAVHTTPTransport, "MKCALENDAR", target, username, password, body); err != nil {
		return "", fmt.Errorf("the server did not create the calendar: %w", err)
	}
	return target, nil
}

// createCardDAVAddressBook makes an address book named name in a CardDAV
// address book home with an extended MKCOL (RFC 5689) and returns its URL.
func createCardDAVAddressBook(ctx context.Context, home, username, password, name string) (string, error) {
	target, err := davNewCollectionURL(home)
	if err != nil {
		return "", err
	}
	body := `<?xml version="1.0" encoding="utf-8"?><d:mkcol xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:set><d:prop><d:resourcetype><d:collection/><card:addressbook/></d:resourcetype><d:displayname>` + xmlEscapeText(name) + `</d:displayname></d:prop></d:set></d:mkcol>`
	if err := davCreateCollection(ctx, cardDAVHTTPTransport, "MKCOL", target, username, password, body); err != nil {
		return "", fmt.Errorf("the server did not create the address book: %w", err)
	}
	return target, nil
}

func davCreateCollection(ctx context.Context, transport http.RoundTripper, method, target, username, password, body string) error {
	req, err := newCardDAVRequest(ctx, method, target, username, password, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := calendarDeleteClient(transport).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := sanitizeProviderErrorBody(strings.TrimSpace(string(data)))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, message)
	}
	return nil
}

// calendarDiscoveryMessage tells what calendar discovery did when it made a
// calendar or found none, and otherwise keeps found.
func calendarDiscoveryMessage(count int, create *davCreate, found string) string {
	if create != nil && create.Created != "" {
		return fmt.Sprintf("Created the calendar %q.", create.Name)
	}
	if count == 0 {
		return "Signed in. There are no calendars here yet."
	}
	return found
}

// renameDAVCollection gives a calendar or address book, named what in
// errors, a new display name with PROPPATCH.
func renameDAVCollection(ctx context.Context, transport http.RoundTripper, target, username, password, name, what string) error {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><d:displayname>` + xmlEscapeText(name) + `</d:displayname></d:prop></d:set></d:propertyupdate>`
	status, wire, err := davCollectionDo(ctx, transport, "PROPPATCH", target, username, password, body)
	if err != nil {
		return fmt.Errorf("the server did not rename the %s: %w", what, err)
	}
	if status == http.StatusMultiStatus {
		// A 207 can still refuse the property itself.
		multi, err := decodeDAVMultiStatus(bytes.NewReader(wire))
		if err != nil {
			return fmt.Errorf("the server did not confirm renaming the %s: %w", what, err)
		}
		for _, response := range multi.Responses {
			for _, propStat := range response.PropStats {
				if !strings.Contains(propStat.Status, " 200 ") {
					return fmt.Errorf("the server did not rename the %s: %s", what, strings.TrimSpace(propStat.Status))
				}
			}
		}
	}
	return nil
}

// deleteDAVCollection deletes a calendar or address book, named what in
// errors, with all it holds. One already gone counts as deleted.
func deleteDAVCollection(ctx context.Context, transport http.RoundTripper, target, username, password, what string) error {
	status, _, err := davCollectionDo(ctx, transport, http.MethodDelete, target, username, password, "")
	if status == http.StatusNotFound || status == http.StatusGone {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the server did not delete the %s: %w", what, err)
	}
	return nil
}

// davCollectionDo sends one request to a collection, without following
// redirects, and returns its status and body; a status outside 2xx is an
// error carrying the server's answer.
func davCollectionDo(ctx context.Context, transport http.RoundTripper, method, target, username, password, body string) (int, []byte, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := newCardDAVRequest(ctx, method, target, username, password, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	}
	resp, err := calendarDeleteClient(transport).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	wire, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := sanitizeProviderErrorBody(strings.TrimSpace(string(wire)))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, wire, fmt.Errorf("server returned %d: %s", resp.StatusCode, message)
	}
	return resp.StatusCode, wire, nil
}

// dropDeletedAddressBook takes an address book deleted on its server out of
// a shared-store account's saved contact sync, as unticking it and saving
// would, so sync doesn't keep asking for it.
func (h *Handler) dropDeletedAddressBook(ctx context.Context, userID, accountID string, change *davCreate) {
	if change == nil || !change.Delete || h.accountStore == nil {
		return
	}
	cfg, err := h.accountStore.GetContactSyncConfig(ctx, userID, accountID)
	if err != nil {
		return
	}
	if next, found := withoutAddressBook(cfg, change.Target); found {
		if err := h.accountStore.SaveContactSyncConfig(ctx, userID, accountID, next, ""); err != nil {
			log.Printf("contacts account=%s: drop deleted address book: %v", accountID, err)
		}
	}
}

// dropUserDeletedAddressBook is dropDeletedAddressBook for a per-user
// account, through the service snapshot discovery ran with.
func (h *Handler) dropUserDeletedAddressBook(ctx context.Context, snapshot *config.AccountServiceSnapshot, change *davCreate) error {
	if change == nil || !change.Delete {
		return nil
	}
	if next, found := withoutAddressBook(snapshot.ContactConfig(), change.Target); found {
		return h.userAccounts.SaveContactServices(ctx, snapshot, next, "")
	}
	return nil
}
