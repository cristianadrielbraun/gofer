package mailauth

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// A Google sign-in asks for full calendar access only when chosen, and
// always keeps what was granted before, so an ordinary reconnect doesn't
// take full access away again.
func TestGoogleAccountOAuthURLAsksForFullCalendarOnlyWhenChosen(t *testing.T) {
	m := &Service{config: &Config{BaseURL: "https://gofer.test", GoogleClient: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth"}, Scopes: googleAccountScopes()}}}
	scopes := func(full bool) ([]string, url.Values) {
		t.Helper()
		parsed, err := url.Parse(m.GoogleAccountOAuthURL("state", full))
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		return strings.Fields(query.Get("scope")), query
	}
	usual, query := scopes(false)
	if slices.Contains(usual, GoogleCalendarFullScope) || !slices.Contains(usual, GoogleCalendarAppCreatedScope) || query.Get("include_granted_scopes") != "true" {
		t.Fatalf("usual sign-in scopes %v, query %v", usual, query)
	}
	full, query := scopes(true)
	if !slices.Contains(full, GoogleCalendarFullScope) || len(full) != len(usual)+1 || query.Get("include_granted_scopes") != "true" {
		t.Fatalf("full calendar sign-in scopes %v, query %v", full, query)
	}
	if slices.Contains(m.config.GoogleClient.Scopes, GoogleCalendarFullScope) {
		t.Fatal("asking once changed the account's usual scopes")
	}
}
