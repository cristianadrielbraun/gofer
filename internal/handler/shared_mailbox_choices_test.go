package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestSharedMailboxChoices(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com", "sales@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	as := func(user string, req *http.Request) *httptest.ResponseRecorder {
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: user, Username: user}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := as("owner", httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/shared-mailboxes", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-shared-mailbox="sales@example.com"`) || !strings.Contains(rec.Body.String(), `data-shared-mailbox="support@example.com"`) {
		t.Fatalf("panel = %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := as("attacker", httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/shared-mailboxes", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's panel = %d", rec.Code)
	}

	turnOff := func(user, email string) *httptest.ResponseRecorder {
		form := url.Values{"email": {email}, "enabled": {"false"}}
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/victim-account/shared-mailboxes/enabled", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return as(user, req)
	}
	if rec := turnOff("attacker", "support@example.com"); rec.Code != http.StatusNotFound {
		t.Fatalf("another user turned a shared mailbox off: %d", rec.Code)
	}
	if rec := turnOff("owner", "nobody@example.com"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown shared mailbox = %d", rec.Code)
	}
	if rec := turnOff("owner", "support@example.com"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-shared-mailbox-enabled="false"`) {
		t.Fatalf("turn off = %d\n%s", rec.Code, rec.Body.String())
	}
	off, err := db.DisabledSharedMailboxes(ctx, "victim-account")
	if err != nil || !off["support@example.com"] || off["sales@example.com"] {
		t.Fatalf("turned off = %v, %v", off, err)
	}
	// Its identity is no longer offered.
	identities, err := db.ListAccountIdentitiesInternal(ctx, "victim-account")
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range identities {
		if identity.Email == "support@example.com" && (identity.Usable() || identity.SharedState != "mailbox_off") {
			t.Fatalf("identity of a turned off mailbox = %+v", identity)
		}
	}

	rec = as("owner", httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/shared-mailboxes/summary", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-account-shared-mailbox="support@example.com" data-enabled="false"`) {
		t.Fatalf("summary = %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestSharedMailboxPanelSaysWhenNoneAreFound(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	get := func(url string) string {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Username: "owner"}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", url, rec.Code)
		}
		return rec.Body.String()
	}
	// Not looked for yet: it searches, then says none was looked for.
	if body := get("/api/accounts/victim-account/shared-mailboxes?poll=3"); !strings.Contains(body, "hx-trigger=\"load delay:2s\"") {
		t.Fatalf("panel before any look does not keep searching:\n%s", body)
	}
	if body := get("/api/accounts/victim-account/shared-mailboxes?poll=0"); strings.Contains(body, "hx-trigger") || !strings.Contains(body, "data-shared-mailboxes-empty") {
		t.Fatalf("panel after its polls:\n%s", body)
	}
	// Looked for, and none found: it says so at once.
	if _, _, err := db.ApplySharedMailboxDiscovery(t.Context(), "victim-account", nil, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if body := get("/api/accounts/victim-account/shared-mailboxes?poll=3"); strings.Contains(body, "hx-trigger=\"load delay:2s\"") {
		t.Fatalf("panel keeps searching after a look found none:\n%s", body)
	}
	// Asked to look again, it searches until a newer look.
	if body := get(fmt.Sprintf("/api/accounts/victim-account/shared-mailboxes?poll=3&since=%d", time.Now().Add(time.Minute).UnixNano())); !strings.Contains(body, "hx-trigger=\"load delay:2s\"") {
		t.Fatalf("panel does not wait for a newer look:\n%s", body)
	}
}
