package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAccountOrderSavesTheOrderOrSaysTheAccountsChanged(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized) VALUES ('owner', 'owner', 'owner');
 INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc_a', 'owner', 'imap', 'ana@example.test'), ('acc_b', 'owner', 'imap', 'ben@example.test')`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	post := func(ids ...string) int {
		t.Helper()
		w := httptest.NewRecorder()
		h.handleAccountOrder(w, contactSelectionRequest(http.MethodPost, "/api/accounts/order", "owner", url.Values{"account_id": ids}))
		return w.Code
	}
	if code := post("acc_b"); code != http.StatusConflict {
		t.Fatalf("order missing an account = %d, want %d", code, http.StatusConflict)
	}
	if code := post("acc_b", "acc_a"); code != http.StatusNoContent {
		t.Fatalf("full order = %d, want %d", code, http.StatusNoContent)
	}
	accounts, err := db.GetAccounts(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != "acc_b" || accounts[1].ID != "acc_a" {
		t.Fatalf("accounts after reordering = %+v", accounts)
	}
}
