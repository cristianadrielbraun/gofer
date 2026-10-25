package mail

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func TestGmailSendAsBecomesProviderIdentities(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('gmail-identities', 'default', ?, 'robin@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var withAlias atomic.Bool
	withAlias.Store(true)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/settings/sendAs" || r.Header.Get("Authorization") != "Bearer gmail-token" {
			t.Errorf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
		requests.Add(1)
		alias := ""
		if withAlias.Load() {
			alias = `{"sendAsEmail": "Shop@example.org", "displayName": "Robin's Shop", "replyToAddress": "orders@example.org", "verificationStatus": "accepted"},`
		}
		_, _ = fmt.Fprintf(w, `{"sendAs": [
			{"sendAsEmail": "robin@example.com", "displayName": "Robin", "isPrimary": true},
			%s
			{"sendAsEmail": "pending@example.org", "verificationStatus": "pending"},
			{"sendAsEmail": "club@example.net", "displayName": "Book Club"}
		]}`, alias)
	}))
	defer server.Close()
	previous := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previous; providerIdentityChecks.Delete("gmail-identities") })

	o := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	cfg := &models.AccountConfig{AccountID: "gmail-identities", Provider: providers.ProviderGmail}
	o.syncProviderIdentities(ctx, "gmail-identities", cfg)
	list, err := db.ListAccountIdentities(ctx, "default", "gmail-identities")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Email != "Shop@example.org" || list[0].Name != "Robin's Shop" || list[0].ReplyTo != "<orders@example.org>" ||
		list[0].Source != models.IdentitySourceProvider || list[1].Email != "club@example.net" {
		t.Fatalf("identities = %+v", list)
	}

	// A sync within the hour does not ask again; a refresh does, and an alias
	// gone from Gmail is gone here.
	withAlias.Store(false)
	o.syncProviderIdentities(ctx, "gmail-identities", cfg)
	if requests.Load() != 1 {
		t.Fatalf("requests after second sync = %d", requests.Load())
	}
	if err := o.refreshProviderIdentities(ctx, "gmail-identities", cfg); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	list, _ = db.ListAccountIdentities(ctx, "default", "gmail-identities")
	if len(list) != 1 || list[0].Email != "club@example.net" {
		t.Fatalf("identities after refresh = %+v", list)
	}
}

func TestJMAPIdentitiesBecomeProviderIdentities(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jmap":
			_, _ = fmt.Fprintf(w, `{"capabilities": {%q: {}, %q: {}, %q: {}},
				"accounts": {"a1": {"name": "robin@example.test"}}, "primaryAccounts": {%q: "a1"},
				"username": "robin@example.test", "apiUrl": %q, "state": "s1"}`,
				jmap.CapabilityCore, jmap.CapabilityMail, jmap.CapabilitySubmission, jmap.CapabilityMail, server.URL+"/jmap/")
		case "/jmap/":
			_, _ = io.WriteString(w, `{"methodResponses": [["Identity/get", {"accountId": "a1", "list": [
				{"id": "i1", "name": "Robin", "email": "robin@example.test"},
				{"id": "i2", "name": "Robin at Work", "email": "work@example.test", "replyTo": [{"name": "Desk", "email": "desk@example.test"}]},
				{"id": "i3", "name": "", "email": "*@example.test"}
			]}, "0"]], "sessionState": "s1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	identities, err := jmapProviderIdentities(context.Background(), jmapTestConfig(server.URL+"/.well-known/jmap", true, true), "secret")
	if err != nil {
		t.Fatalf("jmapProviderIdentities() error = %v", err)
	}
	// Every identity comes back; storing them leaves out the account's own
	// address and the whole-domain one.
	if len(identities) != 3 || identities[1].ProviderIdentityID != "i2" || identities[1].Name != "Robin at Work" ||
		identities[1].Email != "work@example.test" || identities[1].ReplyTo != `"Desk" <desk@example.test>` {
		t.Fatalf("identities = %+v", identities)
	}
}
