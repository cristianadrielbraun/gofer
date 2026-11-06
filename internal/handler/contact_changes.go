package handler

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Gmail and Outlook report what changed in an account's contacts since its
// last sync, deletions included: a Google sync token, an Outlook delta link.
// Gofer keeps the one each sync ends with and starts the next from it, so a
// contact deleted there is known to be deleted, never guessed from a list.
// Without one, or when the provider no longer knows it, the sync starts over
// with every contact; that sync can't tell deletions and ends with a new one.

// outlookGraphPrefer asks Graph for immutable ids, which survive a contact
// moving between folders, and on contact changes for big pages: those hold
// 10 contacts otherwise, and take no $top.
func outlookGraphPrefer(endpoint string) string {
	if strings.Contains(endpoint, "/contacts/delta") {
		return `IdType="ImmutableId", odata.maxpagesize=500`
	}
	return `IdType="ImmutableId"`
}

// errContactChangesExpired is a sync token or delta link the provider no
// longer knows.
var errContactChangesExpired = errors.New("contact changes token expired")

// contactChangesExpired says whether a provider refused a sync token or
// delta link as too old: Google and Graph answer 410 Gone, and Google may
// answer 400 naming the expired token.
func contactChangesExpired(err error) bool {
	var google googleAPIError
	if errors.As(err, &google) {
		body := strings.ToLower(google.Body)
		return google.Status == 410 || (google.Status == 400 && (strings.Contains(body, "expired_sync_token") || strings.Contains(body, "sync token")))
	}
	var outlook outlookAPIError
	if errors.As(err, &outlook) {
		return outlook.Status == 410 || (outlook.Status == 400 && strings.Contains(strings.ToLower(outlook.Body), "syncstate"))
	}
	return false
}

// googleContactChanges walks the account's contacts that changed since
// syncToken, or all of them without one. Each page goes to page with the
// people changed and the resource names of the ones deleted; the last also
// carries the token the next sync starts from. An expired token starts over
// with every contact.
func googleContactChanges(ctx context.Context, get func(string, any) error, syncToken string, page func(people []googlePerson, deleted []string, nextSyncToken string) error) error {
	err := googleContactChangePages(ctx, get, syncToken, page)
	if syncToken != "" && errors.Is(err, errContactChangesExpired) {
		return googleContactChangePages(ctx, get, "", page)
	}
	return err
}

func googleContactChangePages(ctx context.Context, get func(string, any) error, syncToken string, page func([]googlePerson, []string, string) error) error {
	pageToken := ""
	seen := map[string]bool{}
	for first := true; ; first = false {
		query := url.Values{"personFields": {googleContactPersonFields()}, "pageSize": {"1000"}, "requestSyncToken": {"true"}}
		if syncToken != "" {
			query.Set("syncToken", syncToken)
		}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var response googlePeopleResponse
		if err := get(googlePeopleAPIBaseURL+"/people/me/connections?"+query.Encode(), &response); err != nil {
			// Only the first page answers for the token itself.
			if first && contactChangesExpired(err) {
				return errContactChangesExpired
			}
			return err
		}
		var people []googlePerson
		var deleted []string
		for _, person := range response.Connections {
			if person.Metadata != nil && person.Metadata.Deleted {
				if name := strings.TrimSpace(person.ResourceName); name != "" {
					deleted = append(deleted, name)
				}
				continue
			}
			people = append(people, person)
		}
		next := ""
		if response.NextPageToken == "" {
			next = strings.TrimSpace(response.NextSyncToken)
		}
		if err := page(people, deleted, next); err != nil {
			return err
		}
		if response.NextPageToken == "" {
			return nil
		}
		if seen[response.NextPageToken] {
			return errors.New("provider repeated a page token")
		}
		seen[response.NextPageToken] = true
		pageToken = response.NextPageToken
	}
}

// outlookContactChanges walks the contacts of the account's default
// contacts folder that changed since deltaLink, or all of them without one.
// Each page goes to page with the contacts changed and the ids of the ones
// deleted; the last also carries the delta link the next sync starts from.
// An account without contacts has no folder to follow yet: its one page is
// empty and has no delta link. An expired delta link starts over.
func outlookContactChanges(ctx context.Context, get func(string, any) error, deltaLink string, page func(contacts []outlookContact, deleted []string, nextDeltaLink string) error) error {
	err := outlookContactChangePages(ctx, get, deltaLink, page)
	if deltaLink != "" && errors.Is(err, errContactChangesExpired) {
		return outlookContactChangePages(ctx, get, "", page)
	}
	return err
}

func outlookContactChangePages(ctx context.Context, get func(string, any) error, deltaLink string, page func([]outlookContact, []string, string) error) error {
	endpoint := deltaLink
	if endpoint == "" {
		// Graph follows changes per folder; /me/contacts is the default one.
		var first outlookContactsResponse
		if err := get(outlookGraphBaseURL+"/me/contacts?$top=1&$select=parentFolderId", &first); err != nil {
			return err
		}
		if len(first.Contacts) == 0 || strings.TrimSpace(first.Contacts[0].ParentFolderID) == "" {
			return page(nil, nil, "")
		}
		endpoint = outlookGraphBaseURL + "/me/contactFolders/" + url.PathEscape(first.Contacts[0].ParentFolderID) + "/contacts/delta?" + url.Values{"$select": {outlookContactListQuery().Get("$select")}}.Encode()
	}
	seen := map[string]bool{}
	for first := true; endpoint != ""; first = false {
		if err := validOutlookContactDeltaLink(endpoint); err != nil {
			return err
		}
		if seen[endpoint] {
			return errors.New("provider repeated a page URL")
		}
		seen[endpoint] = true
		var response outlookContactsResponse
		if err := get(endpoint, &response); err != nil {
			if first && deltaLink != "" && contactChangesExpired(err) {
				return errContactChangesExpired
			}
			return err
		}
		var contacts []outlookContact
		var deleted []string
		for _, remote := range response.Contacts {
			if remote.Removed != nil {
				if id := strings.TrimSpace(remote.ID); id != "" {
					deleted = append(deleted, id)
				}
				continue
			}
			contacts = append(contacts, remote)
		}
		next := ""
		if response.NextLink == "" {
			next = strings.TrimSpace(response.DeltaLink)
		}
		if err := page(contacts, deleted, next); err != nil {
			return err
		}
		endpoint = response.NextLink
	}
	return nil
}

// validOutlookContactDeltaLink keeps the pages and delta links Graph gives
// to contact changes on Graph itself: the token is sent with each.
func validOutlookContactDeltaLink(endpoint string) error {
	base, err := url.Parse(outlookGraphBaseURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	// Only the user's own folders: Graph names them under /me.
	folders := strings.ToLower(strings.TrimRight(base.Path, "/") + "/me/contactFolders")
	if target.User != nil || target.Scheme != base.Scheme || target.Host != base.Host || target.Fragment != "" ||
		!strings.HasPrefix(strings.ToLower(target.Path), folders) || !strings.HasSuffix(target.Path, "/contacts/delta") {
		return errors.New("contact changes link is outside Microsoft Graph's contacts")
	}
	return nil
}
