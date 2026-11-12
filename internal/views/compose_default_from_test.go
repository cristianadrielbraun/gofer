package views

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestComposeDefaultFromUsesSenderNameAndDefaultIdentity(t *testing.T) {
	account := models.Account{ID: "work", Name: "Mailbox", Email: "robin@example.test",
		Identities: []models.Identity{{ID: "idn_sales", AccountID: "work", Email: "sales@example.test"}}}
	if got := composeDefaultFrom([]models.Account{account}); got != "Mailbox <robin@example.test>" || composeDefaultIdentityID([]models.Account{account}) != "" {
		t.Fatalf("plain account = %q", got)
	}
	account.SenderName = "Robin Example"
	if got := composeDefaultFrom([]models.Account{account}); got != "Robin Example <robin@example.test>" {
		t.Fatalf("with a sender name = %q", got)
	}
	// An identity without a name sends under its address alone.
	account.DefaultIdentityID = "idn_sales"
	if got, id := composeDefaultFrom([]models.Account{account}), composeDefaultIdentityID([]models.Account{account}); got != "sales@example.test" || id != "idn_sales" {
		t.Fatalf("with a default identity = %q, %q", got, id)
	}
	// A default identity that is gone falls back to the account.
	account.DefaultIdentityID = "idn_gone"
	if got := composeDefaultFrom([]models.Account{account}); got != "Robin Example <robin@example.test>" {
		t.Fatalf("with a missing default identity = %q", got)
	}
}
