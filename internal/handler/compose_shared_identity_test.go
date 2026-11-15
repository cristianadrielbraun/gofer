package handler

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestReplyIdentityFollowsTheSharedMailboxFolder(t *testing.T) {
	account := &models.Account{ID: "acc", Email: "ana@example.com"}
	identities := []models.Identity{
		{ID: "support", Email: "support@example.com", SharedMailbox: "support@example.com", SharedState: "active"},
		{ID: "sales", Email: "sales@example.com", SharedMailbox: "sales@example.com", SharedState: "active"},
		{ID: "gone", Email: "old@example.com", SharedMailbox: "old@example.com", SharedState: "inactive"},
		{ID: "manual", Email: "team@example.com"},
	}
	// Sent to the person, Bcc'd to the group: To alone would answer as Ana.
	toAna := &models.Email{From: models.Contact{Email: "customer@example.net"}, To: []models.Contact{{Email: "ana@example.com"}}}

	cases := []struct {
		name, folderMailbox, want string
		email                     *models.Email
	}{
		{"support inbox", "support@example.com", "support", toAna},
		{"sales inbox", "sales@example.com", "sales", toAna},
		{"own inbox keeps the account", "", "", toAna},
		{"own inbox, sent to the group", "", "sales", &models.Email{From: models.Contact{Email: "x@example.net"}, To: []models.Contact{{Email: "sales@example.com"}}}},
		// A mailbox whose identity the user made by hand is answered as it.
		{"mailbox with a manual identity", "team@example.com", "manual", toAna},
		// An identity put aside is never chosen.
		{"removed mailbox", "old@example.com", "", toAna},
		{"sent to a removed mailbox", "", "", &models.Email{From: models.Contact{Email: "x@example.net"}, To: []models.Contact{{Email: "old@example.com"}}}},
	}
	for _, c := range cases {
		got, ok := replyIdentity(account, identities, c.email, c.folderMailbox)
		if c.want == "" {
			if ok {
				t.Errorf("%s: answered as %s, want the account", c.name, got.ID)
			}
			continue
		}
		if !ok || got.ID != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, got.ID, ok, c.want)
		}
	}
}

func TestDefaultIdentitySkipsAnIdentityPutAside(t *testing.T) {
	account := models.Account{DefaultIdentityID: "support", Identities: []models.Identity{{ID: "support", SharedState: "disabled"}}}
	if _, ok := account.DefaultIdentity(); ok {
		t.Fatal("a turned-off identity is still the default sender")
	}
	account.Identities[0].SharedState = "active"
	if _, ok := account.DefaultIdentity(); !ok {
		t.Fatal("an active identity is not the default sender")
	}
}
