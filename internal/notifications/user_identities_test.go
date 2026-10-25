package notifications

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserIdentitiesHTTPEditingAndIsolation(t *testing.T) {
	f := newUserSignatureFixture(t)
	ids := map[string]string{}
	for _, owner := range []string{"alice", "bob"} {
		account := f.accounts[owner].ID
		form := url.Values{"name": {owner + " sales"}, "email": {owner + "-sales@example.com"}, "signature_id": {"same-signature"}}
		response := f.request(owner, http.MethodPost, "/api/accounts/"+account+"/identities", form.Encode())
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), owner+"-sales@example.com") || strings.Contains(response.Body.String(), "data-account-identity-error") {
			t.Fatal("add identity", owner, response.Code, response.Body.String())
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			identities, err := db.ListAccountIdentities(t.Context(), owner, account)
			if err != nil {
				return err
			}
			if len(identities) != 1 || identities[0].SignatureID != "same-signature" {
				t.Fatalf("%s identities = %+v", owner, identities)
			}
			ids[owner] = identities[0].ID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		other := map[string]string{"alice": "bob", "bob": "alice"}[owner]
		account := f.accounts[owner].ID
		response := f.request(owner, http.MethodGet, "/api/accounts/"+account+"/identities/manage", "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), owner+"-sales@example.com") || strings.Contains(response.Body.String(), other+"-sales@example.com") || !strings.Contains(response.Body.String(), owner+" signature") {
			t.Fatal("manage identities", owner, response.Code, response.Body.String())
		}
		if response := f.request(owner, http.MethodGet, "/api/accounts/"+f.accounts[other].ID+"/identities/manage", ""); response.Code == http.StatusOK {
			t.Fatal("foreign account dialog opened", owner, response.Body.String())
		}
		// The other owner's identity id under this owner's own account.
		form := url.Values{"id": {ids[other]}, "email": {"stolen@example.com"}}
		if response := f.request(owner, http.MethodPost, "/api/accounts/"+account+"/identities", form.Encode()); response.Code != http.StatusNotFound {
			t.Fatal("foreign identity edit", owner, response.Code, response.Body.String())
		}
		if response := f.request(owner, http.MethodDelete, "/api/accounts/"+account+"/identities/"+ids[other], ""); response.Code != http.StatusNotFound {
			t.Fatal("foreign identity delete", owner, response.Code, response.Body.String())
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			identities, err := db.ListAccountIdentities(t.Context(), owner, f.accounts[owner].ID)
			if err != nil {
				return err
			}
			if len(identities) != 1 || identities[0].Email != owner+"-sales@example.com" || identities[0].Source != models.IdentitySourceManual {
				t.Fatalf("%s identities after foreign attempts = %+v", owner, identities)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing reached the central database.
	if identities, err := f.system.ListUserIdentities(t.Context(), "alice"); err != nil || len(identities) != 0 {
		t.Fatalf("central identities = %+v, %v", identities, err)
	}
}
