package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The account name only labels the account once a sender name is set: mail
// from the account's own address goes out under the sender name.
func TestAccountSenderNameIsTheFromName(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	account := map[string]string{"id": "victim-account"}

	rec := identityRequest(t, h, h.handleAccountIdentitiesPanel, http.MethodGet, "/api/accounts/victim-account/identities/panel?sender=1", account, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-account-sender-name-form") || !strings.Contains(rec.Body.String(), `placeholder="Owner"`) {
		t.Fatalf("sender name form = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleSaveAccountSenderName, http.MethodPost, "/api/accounts/victim-account/identities/sender-name", account, url.Values{"sender_name": {" Robin Example "}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Robin Example &lt;owner@example.com&gt;") || strings.Contains(rec.Body.String(), "data-account-sender-name-form") {
		t.Fatalf("save sender name = %d %q", rec.Code, rec.Body.String())
	}

	send := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.handleCompose(rec, composeIdentityRequest(t, "/compose", url.Values{
			"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {"Hello"}, "body": {"Hello"},
		}))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("compose = %d %q", rec.Code, rec.Body.String())
		}
		var response map[string]string
		_ = json.NewDecoder(rec.Body).Decode(&response)
		queued, err := db.GetOutgoingSend(t.Context(), response["send_id"])
		if err != nil {
			t.Fatal(err)
		}
		from := mimeAddresses(t, queued.MIMEData, "From")
		if len(from) != 1 {
			t.Fatalf("From = %v", from)
		}
		return []string{from[0].Name, from[0].Address}
	}
	if from := send(); from[0] != "Robin Example" || from[1] != "owner@example.com" {
		t.Fatalf("From = %v, want the sender name", from)
	}

	// Clearing it sends under the account name again.
	rec = identityRequest(t, h, h.handleSaveAccountSenderName, http.MethodPost, "/api/accounts/victim-account/identities/sender-name", account, url.Values{"sender_name": {""}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear sender name = %d", rec.Code)
	}
	if from := send(); from[0] != "Owner" {
		t.Fatalf("From = %v, want the account name", from)
	}
}

func TestAccountDefaultIdentityRoute(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	identity := seedSalesIdentity(t, db)
	account := map[string]string{"id": "victim-account"}

	rec := identityRequest(t, h, h.handleManageAccountIdentities, http.MethodGet, "/", account, nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Count(body, "data-account-identity-default") != 1 || !strings.Contains(body, `data-account-identity-make-default="`+identity.ID+`"`) {
		t.Fatalf("dialog before = %d %q", rec.Code, body)
	}

	rec = identityRequest(t, h, h.handleSetAccountDefaultIdentity, http.MethodPost, "/", account, url.Values{"identity_id": {identity.ID}}, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `data-account-identity-make-default=""`) || strings.Contains(body, `data-account-identity-make-default="`+identity.ID+`"`) {
		t.Fatalf("set default = %d %q", rec.Code, body)
	}
	owned, err := h.ownedAccount(ownerRequest(httptest.NewRequest(http.MethodGet, "/", nil)).Context(), "victim-account")
	if err != nil || owned.DefaultIdentityID != identity.ID {
		t.Fatalf("account = %+v, %v", owned, err)
	}

	rec = identityRequest(t, h, h.handleSetAccountDefaultIdentity, http.MethodPost, "/", account, url.Values{"identity_id": {"idn_missing"}}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown identity = %d", rec.Code)
	}

	rec = identityRequest(t, h, h.handleSetAccountDefaultIdentity, http.MethodPost, "/", account, url.Values{"identity_id": {""}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-account-identity-make-default="`+identity.ID+`"`) {
		t.Fatalf("back to own address = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAccountSenderRoutesRejectForeignAccounts(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	identity := seedSalesIdentity(t, db)
	account := map[string]string{"id": "victim-account"}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"sender name": identityRequest(t, h, h.handleSaveAccountSenderName, http.MethodPost, "/", account, url.Values{"sender_name": {"Mallory"}}, false),
		"default":     identityRequest(t, h, h.handleSetAccountDefaultIdentity, http.MethodPost, "/", account, url.Values{"identity_id": {identity.ID}}, false),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("foreign %s = %d %q, want 404", name, rec.Code, rec.Body.String())
		}
	}
	// The attacker's own account can't take the owner's identity as default.
	rec := identityRequest(t, h, h.handleSetAccountDefaultIdentity, http.MethodPost, "/", map[string]string{"id": "attacker-account"}, url.Values{"identity_id": {identity.ID}}, false)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-account default = %d, want 404", rec.Code)
	}
	owned, err := h.ownedAccount(ownerRequest(httptest.NewRequest(http.MethodGet, "/", nil)).Context(), "victim-account")
	if err != nil || owned.SenderName != "" || owned.DefaultIdentityID != "" {
		t.Fatalf("owner account changed = %+v, %v", owned, err)
	}
}
