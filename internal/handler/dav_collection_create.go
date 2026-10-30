package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// davCreate asks discovery to make a new calendar or address book, named
// Name, in the home collection the server reports, before listing them.
// Created is the new collection's URL once the server has made it.
type davCreate struct {
	Name    string
	Created string
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
	if len([]rune(name)) > davCreateNameMaxLength {
		return nil, fmt.Errorf("Use a name of at most %d characters.", davCreateNameMaxLength)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, errors.New("The name can't have line breaks or control characters.")
	}
	return &davCreate{Name: name}, nil
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

// calDAVDiscoveryMessage tells what CalDAV discovery did when it made a
// calendar or found none, and otherwise keeps found.
func calDAVDiscoveryMessage(count int, create *davCreate, found string) string {
	if create != nil && create.Created != "" {
		return fmt.Sprintf("Created the calendar %q.", create.Name)
	}
	if count == 0 {
		return "Signed in. This server has no calendars yet."
	}
	return found
}
