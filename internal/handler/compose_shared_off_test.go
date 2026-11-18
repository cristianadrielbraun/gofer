package handler

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
)

// A draft written as a shared mailbox the user later turned off is still
// saved, but not sent from that mailbox, whether its From is the mailbox's
// identity or typed by hand.
func TestNoMailIsSentFromATurnedOffSharedMailbox(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	if err := db.MergeUISettings(ctx, "owner", map[string]string{sharedmailbox.KeyAutoIdentities: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	account, err := h.ownedAccount(ctx, "victim-account")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListAccountIdentitiesInternal(ctx, "victim-account")
	if err != nil {
		t.Fatal(err)
	}
	var identityID string
	for _, identity := range identities {
		if identity.Email == "support@example.com" {
			identityID = identity.ID
		}
	}
	if identityID == "" {
		t.Fatal("no identity made for the shared mailbox")
	}
	sender := func(values url.Values, sending bool) error {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/compose/send", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(ctx)
		_, err := h.composeSenderFor(ctx, account, req, sending)
		return err
	}
	byIdentity := url.Values{"identity_id": {identityID}}
	typed := url.Values{"from_name": {"Support"}, "from_email": {"Support@example.com"}}
	if err := sender(byIdentity, true); err != nil {
		t.Fatalf("sending from the shared mailbox while on: %v", err)
	}

	if err := db.SetSharedMailboxEnabled(ctx, "victim-account", "support@example.com", false); err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string]url.Values{"identity": byIdentity, "typed": typed} {
		if err := sender(values, true); !errors.Is(err, errComposeSharedMailboxOff) {
			t.Errorf("%s: sending from the turned-off mailbox = %v", name, err)
		}
		if err := sender(values, false); err != nil {
			t.Errorf("%s: saving the draft = %v", name, err)
		}
	}
}
