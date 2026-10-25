package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gomail "github.com/emersion/go-message/mail"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func composeIdentityRequest(t *testing.T, path string, form url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Username: "owner"}))
}

func mimeAddresses(t *testing.T, raw []byte, field string) []*gomail.Address {
	t.Helper()
	reader, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read MIME: %v", err)
	}
	defer reader.Close()
	addresses, _ := reader.Header.AddressList(field)
	return addresses
}

func seedSalesIdentity(t *testing.T, db *storage.DB) models.Identity {
	t.Helper()
	identity, err := db.SaveAccountIdentityGuarded(t.Context(), "owner", models.Identity{AccountID: "victim-account", Name: "Owner Sales", Email: "sales@example.com", ReplyTo: "Sales Team <team@example.com>"}, nil)
	if err != nil {
		t.Fatalf("save identity: %v", err)
	}
	return identity
}

func TestHandleComposeSendsAsIdentity(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	identity := seedSalesIdentity(t, db)
	rec := httptest.NewRecorder()
	h.handleCompose(rec, composeIdentityRequest(t, "/compose", url.Values{
		"account_id": {"victim-account"}, "identity_id": {identity.ID},
		"to": {"recipient@example.com"}, "subject": {"From sales"}, "body": {"Hello"},
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	var response map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&response)
	queued, err := db.GetOutgoingSend(t.Context(), response["send_id"])
	if err != nil {
		t.Fatalf("GetOutgoingSend() error = %v", err)
	}
	if queued.EnvelopeFrom != "sales@example.com" {
		t.Fatalf("envelope from = %q", queued.EnvelopeFrom)
	}
	from := mimeAddresses(t, queued.MIMEData, "From")
	replyTo := mimeAddresses(t, queued.MIMEData, "Reply-To")
	if len(from) != 1 || from[0].Address != "sales@example.com" || from[0].Name != "Owner Sales" || len(replyTo) != 1 || replyTo[0].Address != "team@example.com" {
		t.Fatalf("From = %v, Reply-To = %v", from, replyTo)
	}
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(queued.MessageJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	restored := snapshot.outgoingMessage()
	if restored.FromEmail != "sales@example.com" || len(restored.ReplyTo) != 1 || restored.ReplyTo[0].Address != "team@example.com" {
		t.Fatalf("restored snapshot = %+v", restored)
	}

	// Without an identity the account's own address is used, with no Reply-To.
	rec = httptest.NewRecorder()
	h.handleCompose(rec, composeIdentityRequest(t, "/compose", url.Values{
		"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {"From owner"}, "body": {"Hello"},
	}))
	_ = json.NewDecoder(rec.Body).Decode(&response)
	queued, _ = db.GetOutgoingSend(t.Context(), response["send_id"])
	if queued.EnvelopeFrom != "owner@example.com" || len(mimeAddresses(t, queued.MIMEData, "Reply-To")) != 0 {
		t.Fatalf("account send = from %q MIME %q", queued.EnvelopeFrom, queued.MIMEData)
	}
}

func TestHandleComposeRefusesIdentityOfAnotherAccount(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	foreign, err := db.SaveAccountIdentityGuarded(t.Context(), "attacker", models.Identity{AccountID: "attacker-account", Email: "spoof@example.com"}, nil)
	if err != nil {
		t.Fatalf("save foreign identity: %v", err)
	}
	for _, form := range []url.Values{
		{"account_id": {"victim-account"}, "identity_id": {foreign.ID}, "to": {"recipient@example.com"}, "body": {"x"}},
		{"account_id": {"victim-account"}, "identity_id": {"missing"}, "to": {"recipient@example.com"}, "body": {"x"}},
	} {
		rec := httptest.NewRecorder()
		h.handleCompose(rec, composeIdentityRequest(t, "/compose", form))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
		}
	}
	var queued int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued sends = %d, %v", queued, err)
	}
}

func TestComposeDraftKeepsIdentity(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	identity := seedSalesIdentity(t, db)
	rec := httptest.NewRecorder()
	h.handleComposeDraft(rec, composeIdentityRequest(t, "/compose/draft", url.Values{
		"account_id": {"victim-account"}, "identity_id": {identity.ID},
		"to": {"recipient@example.com"}, "subject": {"Draft from sales"}, "body": {"Draft body"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft status = %d body = %q", rec.Code, rec.Body.String())
	}
	var operationMIME []byte
	if err := db.Read().QueryRow(`SELECT mime_data FROM imap_draft_operations WHERE account_id = 'victim-account'`).Scan(&operationMIME); err != nil {
		t.Fatalf("queued draft: %v", err)
	}
	if from := mimeAddresses(t, operationMIME, "From"); len(from) != 1 || from[0].Address != "sales@example.com" {
		t.Fatalf("draft MIME From = %v", from)
	}

	var localID int64
	if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id = 'victim-account' AND subject = 'Draft from sales'`).Scan(&localID); err != nil {
		t.Fatalf("draft row: %v", err)
	}
	getDraft := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+strconv.FormatInt(localID, 10), nil)
		req.SetPathValue("id", strconv.FormatInt(localID, 10))
		rec := httptest.NewRecorder()
		h.handleGetDraft(rec, ownerRequest(req))
		var draft map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&draft); err != nil {
			t.Fatalf("decode draft %d %q: %v", rec.Code, rec.Body.String(), err)
		}
		return draft
	}
	if draft := getDraft(); draft["identity_id"] != identity.ID {
		t.Fatalf("draft identity = %v", draft["identity_id"])
	}

	msg, err := h.outgoingMessageFromDraft(t.Context(), localID)
	if err != nil {
		t.Fatalf("outgoingMessageFromDraft() error = %v", err)
	}
	if msg.FromEmail != "sales@example.com" || msg.FromName != "Owner Sales" || len(msg.ReplyTo) != 1 || msg.ReplyTo[0].Address != "team@example.com" {
		t.Fatalf("draft sender = %q %q %v", msg.FromName, msg.FromEmail, msg.ReplyTo)
	}

	// Once the identity is gone the draft goes out as the account.
	if err := db.DeleteAccountIdentityGuarded(t.Context(), "owner", "victim-account", identity.ID, nil); err != nil {
		t.Fatalf("delete identity: %v", err)
	}
	if draft := getDraft(); draft["identity_id"] != "" {
		t.Fatalf("draft identity after delete = %v", draft["identity_id"])
	}
	msg, err = h.outgoingMessageFromDraft(t.Context(), localID)
	if err != nil || msg.FromEmail != "owner@example.com" || len(msg.ReplyTo) != 0 {
		t.Fatalf("draft sender after delete = %+v, %v", msg, err)
	}
}

func TestAccountSignaturesUseIdentitySignature(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	accountSig, _ := db.SaveSignature(ctx, "owner", models.Signature{Name: "Owner", TextBody: "Owner"})
	salesSig, _ := db.SaveSignature(ctx, "owner", models.Signature{Name: "Sales", TextBody: "Owner Sales"})
	if err := db.SaveAccountSignatureSettings(ctx, "owner", models.AccountSignatureSettings{AccountID: "victim-account", NewSignatureID: accountSig.ID, NewEnabled: true}); err != nil {
		t.Fatal(err)
	}
	plain, err := db.SaveAccountIdentityGuarded(ctx, "owner", models.Identity{AccountID: "victim-account", Email: "help@example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := db.SaveAccountIdentityGuarded(ctx, "owner", models.Identity{AccountID: "victim-account", Email: "sales@example.com", SignatureID: salesSig.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defaultSignature := func(query string) (string, int) {
		req := httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/signatures?"+query, nil)
		req.SetPathValue("id", "victim-account")
		rec := httptest.NewRecorder()
		h.handleAccountSignatures(rec, ownerRequest(req))
		var body struct {
			Default *models.Signature `json:"default_signature"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&body)
		if body.Default == nil {
			return "", rec.Code
		}
		return body.Default.ID, rec.Code
	}
	for query, want := range map[string]string{
		"mode=new":                         accountSig.ID,
		"mode=new&identity=" + plain.ID:    accountSig.ID,
		"mode=new&identity=" + signed.ID:   salesSig.ID,
		"mode=reply&identity=" + signed.ID: salesSig.ID,
		"mode=reply&identity=" + plain.ID:  "",
	} {
		if got, code := defaultSignature(query); got != want || code != http.StatusOK {
			t.Errorf("%s: default signature = %q (%d), want %q", query, got, code, want)
		}
	}
	if _, code := defaultSignature("mode=new&identity=missing"); code != http.StatusNotFound {
		t.Errorf("unknown identity status = %d, want 404", code)
	}
}

func TestMatchSenderIdentity(t *testing.T) {
	account := &models.Account{Name: "Owner", Email: "owner@example.com"}
	identities := []models.Identity{
		{ID: "sales", Name: "Owner Sales", Email: "sales@example.com"},
		{ID: "sales-team", Name: "Sales Team", Email: "sales@example.com"},
		{ID: "alias", Name: "Owner Alias", Email: "owner@example.com"},
	}
	for _, test := range []struct{ name, email, want string }{
		{"Sales Team", "SALES@example.com", "sales-team"},
		{"Someone", "sales@example.com", "sales"},
		{"Owner", "owner@example.com", ""},
		{"Owner Alias", "owner@example.com", "alias"},
		{"Nobody", "nobody@example.com", ""},
		{"", "", ""},
	} {
		identity, ok := matchSenderIdentity(account, identities, test.name, test.email)
		if identity.ID != test.want || ok != (test.want != "") {
			t.Errorf("%q <%s> = %q, %t; want %q", test.name, test.email, identity.ID, ok, test.want)
		}
	}
}
