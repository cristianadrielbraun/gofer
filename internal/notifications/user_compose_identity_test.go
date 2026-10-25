package notifications

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserComposeSendsAsIdentity(t *testing.T) {
	f, imap, smtp := newUserComposeFixture(t)
	var identity models.Identity
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var err error
		identity, err = db.SaveAccountIdentityGuarded(t.Context(), "alice", models.Identity{AccountID: f.accounts["alice"].ID, Name: "Alice Sales", Email: "alice-sales@example.com", ReplyTo: "team@example.com"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Bob cannot send as Alice's identity from his own account.
	if rec := f.request("bob", http.MethodPost, "/compose", url.Values{"account_id": {f.accounts["bob"].ID}, "identity_id": {identity.ID}, "to": {"recipient@example.com"}, "body": {"x"}}.Encode()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign identity send = %d %s", rec.Code, rec.Body.String())
	}

	draft := f.compose(t, "alice", "/compose/draft", url.Values{"identity_id": {identity.ID}, "to": {"recipient@example.com"}, "subject": {"Sales draft"}, "body": {"Draft"}})
	waitUserIMAP(t, func() bool { return len(imap.rawCopies("alice", "Drafts")) == 1 })
	if raw := imap.rawCopies("alice", "Drafts")[0]; !strings.Contains(raw, "From: \"Alice Sales\" <alice-sales@example.com>") || !strings.Contains(raw, "Reply-To: <team@example.com>") {
		t.Fatalf("draft copy = %q", raw)
	}

	sent := f.compose(t, "alice", "/compose", url.Values{"draft_id": {draft["draft_id"].(string)}, "identity_id": {identity.ID}, "to": {"recipient@example.com"}, "subject": {"Sales draft"}, "body": {"Sent"}})
	waitUserIMAP(t, func() bool {
		return f.sendStatus(t, "alice", sent["send_id"].(string)).SentCopyStatus == storage.SentCopyComplete
	})
	if smtp.count("alice-sales") != 1 || smtp.count("alice") != 0 {
		t.Fatalf("SMTP senders: identity %d, account %d", smtp.count("alice-sales"), smtp.count("alice"))
	}
	smtp.mu.Lock()
	raw := smtp.accepted["alice-sales"][0]
	smtp.mu.Unlock()
	if !strings.Contains(raw, "From: \"Alice Sales\" <alice-sales@example.com>") || !strings.Contains(raw, "Reply-To: <team@example.com>") {
		t.Fatalf("delivered message = %q", raw)
	}
	copies := imap.rawCopies("alice", "Sent")
	if len(copies) != 1 || !strings.Contains(copies[0], "From: \"Alice Sales\" <alice-sales@example.com>") {
		t.Fatalf("sent copies = %q", copies)
	}
}
