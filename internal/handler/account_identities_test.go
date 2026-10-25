package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func identityRequest(t *testing.T, h *Handler, handle func(http.ResponseWriter, *http.Request), method, path string, values map[string]string, form url.Values, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for key, value := range values {
		req.SetPathValue(key, value)
	}
	if owner {
		req = ownerRequest(req)
	} else {
		req = attackerRequestWithAdmin(req, true)
	}
	rec := httptest.NewRecorder()
	handle(rec, req)
	return rec
}

func TestAccountIdentityRoutes(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	account := map[string]string{"id": "victim-account"}

	rec := identityRequest(t, h, h.handleManageAccountIdentities, http.MethodGet, "/api/accounts/victim-account/identities/manage", account, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "account-identities-dialog") || !strings.Contains(rec.Body.String(), "No further identities yet") {
		t.Fatalf("manage = %d %q", rec.Code, rec.Body.String())
	}

	rec = identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities", account, url.Values{"name": {"Owner Sales"}, "email": {"sales@example.com"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Owner Sales &lt;sales@example.com&gt;") {
		t.Fatalf("add = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities", account, url.Values{"email": {"help@example.com"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("add second = %d", rec.Code)
	}
	rec = identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities", account, url.Values{"email": {"not an address"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-account-identity-error") || !strings.Contains(rec.Body.String(), `value="not an address"`) {
		t.Fatalf("invalid add = %d %q", rec.Code, rec.Body.String())
	}

	identities, err := db.ListAccountIdentities(ctx, "owner", "victim-account")
	if err != nil || len(identities) != 2 {
		t.Fatalf("identities = %+v, %v", identities, err)
	}
	sales, help := identities[0], identities[1]

	rec = identityRequest(t, h, h.handleAccountIdentitiesPanel, http.MethodGet, "/api/accounts/victim-account/identities/panel?edit="+sales.ID, account, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Edit sales@example.com") || !strings.Contains(rec.Body.String(), `value="`+sales.ID+`"`) {
		t.Fatalf("edit panel = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities", account, url.Values{"id": {sales.ID}, "name": {"Owner Sales Team"}, "email": {"sales@example.com"}, "reply_to": {"team@example.com"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Replies to &lt;team@example.com&gt;") {
		t.Fatalf("edit = %d %q", rec.Code, rec.Body.String())
	}

	move := map[string]string{"id": "victim-account", "identity": help.ID}
	rec = identityRequest(t, h, h.handleMoveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities/"+help.ID+"/move", move, url.Values{"direction": {"up"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("move = %d", rec.Code)
	}
	if identities, _ = db.ListAccountIdentities(ctx, "owner", "victim-account"); identities[0].ID != help.ID {
		t.Fatalf("after move = %+v", identities)
	}

	rec = identityRequest(t, h, h.handleDeleteAccountIdentity, http.MethodDelete, "/api/accounts/victim-account/identities/"+help.ID, move, nil, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "help@example.com") {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}

	if err := db.ReplaceProviderIdentities(ctx, "victim-account", []models.Identity{{ProviderIdentityID: "p1", Name: "Owner News", Email: "news@example.com"}}); err != nil {
		t.Fatalf("provider identity: %v", err)
	}
	identities, _ = db.ListAccountIdentities(ctx, "owner", "victim-account")
	provider := identities[1]
	signature, err := db.SaveSignature(ctx, "owner", models.Signature{Name: "News", TextBody: "Owner News"})
	if err != nil {
		t.Fatalf("save signature: %v", err)
	}
	// The form sends no address for a provider identity; only the signature changes.
	rec = identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/api/accounts/victim-account/identities", account, url.Values{"id": {provider.ID}, "signature_id": {signature.ID}}, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "data-account-identity-error") {
		t.Fatalf("provider signature = %d %q", rec.Code, rec.Body.String())
	}
	if got, _ := db.GetAccountIdentity(ctx, "owner", "victim-account", provider.ID); got.SignatureID != signature.ID || got.Email != "news@example.com" || got.Name != "Owner News" {
		t.Fatalf("provider identity = %+v", got)
	}
	providerRoute := map[string]string{"id": "victim-account", "identity": provider.ID}
	rec = identityRequest(t, h, h.handleDeleteAccountIdentity, http.MethodDelete, "/api/accounts/victim-account/identities/"+provider.ID, providerRoute, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "only be removed at the provider") {
		t.Fatalf("provider delete = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAccountIdentityRoutesRejectForeignAccounts(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	identity, err := db.SaveAccountIdentityGuarded(ctx, "owner", models.Identity{AccountID: "victim-account", Email: "sales@example.com"}, nil)
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	account := map[string]string{"id": "victim-account"}
	route := map[string]string{"id": "victim-account", "identity": identity.ID}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"manage": identityRequest(t, h, h.handleManageAccountIdentities, http.MethodGet, "/", account, nil, false),
		"panel":  identityRequest(t, h, h.handleAccountIdentitiesPanel, http.MethodGet, "/", account, nil, false),
		"add":    identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/", account, url.Values{"email": {"stolen@example.com"}}, false),
		"edit":   identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/", account, url.Values{"id": {identity.ID}, "email": {"stolen@example.com"}}, false),
		"move":   identityRequest(t, h, h.handleMoveAccountIdentity, http.MethodPost, "/", route, url.Values{"direction": {"down"}}, false),
		"delete": identityRequest(t, h, h.handleDeleteAccountIdentity, http.MethodDelete, "/", route, nil, false),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("foreign %s = %d %q, want 404", name, rec.Code, rec.Body.String())
		}
	}
	// The attacker's own account cannot reach the owner's identity either.
	rec := identityRequest(t, h, h.handleSaveAccountIdentity, http.MethodPost, "/", map[string]string{"id": "attacker-account"}, url.Values{"id": {identity.ID}, "email": {"stolen@example.com"}}, false)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-account edit = %d %q, want 404", rec.Code, rec.Body.String())
	}
	if got, err := db.GetAccountIdentity(ctx, "owner", "victim-account", identity.ID); err != nil || got.Email != "sales@example.com" {
		t.Fatalf("owner identity changed = %+v, %v", got, err)
	}
	if list, _ := db.ListAccountIdentities(ctx, "attacker", "attacker-account"); len(list) != 0 {
		t.Fatalf("attacker gained identities = %+v", list)
	}
}

func TestRefreshAccountIdentities(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	account := map[string]string{"id": "victim-account"}
	// An IMAP account offers no refresh, and asking anyway says why.
	rec := identityRequest(t, h, h.handleManageAccountIdentities, http.MethodGet, "/api/accounts/victim-account/identities/manage", account, nil, true)
	if strings.Contains(rec.Body.String(), "data-account-identities-refresh") {
		t.Fatal("IMAP account offers a provider refresh")
	}
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	rec = identityRequest(t, h, h.handleRefreshAccountIdentities, http.MethodPost, "/api/accounts/victim-account/identities/refresh", account, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "does not list the addresses") {
		t.Fatalf("refresh IMAP = %d %q", rec.Code, rec.Body.String())
	}
	if rec := identityRequest(t, h, h.handleRefreshAccountIdentities, http.MethodPost, "/api/accounts/victim-account/identities/refresh", account, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("refresh another user's account = %d", rec.Code)
	}

	if _, err := db.Write().Exec(`UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(t, h, h.handleManageAccountIdentities, http.MethodGet, "/api/accounts/victim-account/identities/manage", account, nil, true)
	if !strings.Contains(rec.Body.String(), "Refresh from Gmail") {
		t.Fatal("Gmail account offers no provider refresh")
	}
}
