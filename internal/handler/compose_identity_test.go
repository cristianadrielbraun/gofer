package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	// Once the identity is gone its address is a one-time From, the
	// account's way and without the identity's Reply-To.
	if err := db.DeleteAccountIdentityGuarded(t.Context(), "owner", "victim-account", identity.ID, nil); err != nil {
		t.Fatalf("delete identity: %v", err)
	}
	draft := getDraft()
	oneTime, _ := draft["one_time_from"].(map[string]any)
	if draft["identity_id"] != "" || oneTime["email"] != "sales@example.com" || oneTime["send_via"] != "" {
		t.Fatalf("draft after delete = identity %v one-time %v", draft["identity_id"], draft["one_time_from"])
	}
	msg, err = h.outgoingMessageFromDraft(t.Context(), localID)
	if err != nil || msg.FromEmail != "sales@example.com" || msg.IdentityID != "" || len(msg.ReplyTo) != 0 {
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

func TestComposeSourceRepliesAsAddressedIdentity(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	identity := seedSalesIdentity(t, db)
	if _, err := db.SaveAccountIdentityGuarded(t.Context(), "owner", models.Identity{AccountID: "victim-account", Name: "Owner Help", Email: "help@example.com"}, nil); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO folders (id, account_id, remote_id, name, role) VALUES ('victim-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("Hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	type recipient struct{ kind, email string }
	seed := func(id int64, from string, recipients ...recipient) {
		t.Helper()
		if _, err := db.Write().Exec(`INSERT INTO messages (id, account_id, internet_message_id, thread_id, subject, from_email, body_text_path) VALUES (?, 'victim-account', ?, ?, 'Order', ?, ?)`, id, "<m"+strconv.FormatInt(id, 10)+"@example.com>", "t"+strconv.FormatInt(id, 10), from, bodyPath); err != nil {
			t.Fatalf("insert message: %v", err)
		}
		if _, err := db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id, remote_uid) VALUES (?, 'victim-inbox', ?)`, id, id); err != nil {
			t.Fatalf("insert folder state: %v", err)
		}
		for _, r := range recipients {
			if _, err := db.Write().Exec(`INSERT INTO message_recipients (message_id, kind, email) VALUES (?, ?, ?)`, id, r.kind, r.email); err != nil {
				t.Fatalf("insert recipient: %v", err)
			}
		}
	}
	seed(1, "buyer@example.com", recipient{"to", "colleague@example.com"}, recipient{"to", "SALES@example.com"})
	seed(2, "buyer@example.com", recipient{"to", "owner@example.com"}, recipient{"cc", "sales@example.com"})
	seed(3, "buyer@example.com", recipient{"to", "colleague@example.com"}, recipient{"cc", "sales@example.com"})
	seed(4, "sales@example.com", recipient{"to", "buyer@example.com"})
	seed(5, "buyer@example.com", recipient{"to", "list@example.com"})

	source := func(id int64) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/compose/source?email_id="+strconv.FormatInt(id, 10), nil)
		rec := httptest.NewRecorder()
		h.handleComposeSource(rec, ownerRequest(req))
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode source %d: %d %q", id, rec.Code, rec.Body.String())
		}
		return out
	}
	for _, tc := range []struct {
		id       int64
		identity string
		why      string
	}{
		{1, identity.ID, "sent to the identity"},
		{2, "", "the account's own address comes first"},
		{3, identity.ID, "identity only in Cc"},
		{4, identity.ID, "sent from the identity"},
		{5, "", "none of the account's addresses"},
	} {
		if got := source(tc.id)["identity_id"]; got != tc.identity {
			t.Errorf("message %d (%s): identity = %v, want %q", tc.id, tc.why, got, tc.identity)
		}
	}
	own, _ := source(1)["own_emails"].([]any)
	if len(own) != 3 || own[0] != "owner@example.com" || own[1] != "sales@example.com" || own[2] != "help@example.com" {
		t.Fatalf("own_emails = %v", own)
	}
}

func queuedComposeSend(t *testing.T, h *Handler, db *storage.DB, form url.Values) (storage.OutgoingSend, outgoingMessageSnapshot) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleCompose(rec, composeIdentityRequest(t, "/compose", form))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	var response map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&response)
	queued, err := db.GetOutgoingSend(t.Context(), response["send_id"])
	if err != nil {
		t.Fatalf("GetOutgoingSend() error = %v", err)
	}
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(queued.MessageJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	return queued, snapshot
}

func TestHandleComposeSendsOneTimeFrom(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	sales := seedSalesIdentity(t, db)
	shop, err := db.SaveAccountIdentityGuarded(t.Context(), "owner", models.Identity{AccountID: "victim-account", Name: "Shop", Email: "shop@example.org", ReplyTo: "orders@example.org", SMTPHost: "smtp.example.org", SMTPPort: 587, SMTPTLSMode: "starttls", SMTPUsername: "shop-login"}, nil)
	if err != nil {
		t.Fatalf("save shop identity: %v", err)
	}
	base := func(extra url.Values) url.Values {
		form := url.Values{"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {"Once"}, "body": {"Hello"}}
		for key, values := range extra {
			form[key] = values
		}
		return form
	}

	// The account's way: the typed name and address, nothing from an
	// identity, even one named alongside.
	queued, snapshot := queuedComposeSend(t, h, db, base(url.Values{"from_name": {" Owner Tracking "}, "from_email": {" Owner+Tracking@Example.com "}, "identity_id": {sales.ID}}))
	from := mimeAddresses(t, queued.MIMEData, "From")
	if len(from) != 1 || from[0].Address != "Owner+Tracking@Example.com" || from[0].Name != "Owner Tracking" || queued.EnvelopeFrom != "Owner+Tracking@Example.com" {
		t.Fatalf("From = %v envelope %q", from, queued.EnvelopeFrom)
	}
	if len(mimeAddresses(t, queued.MIMEData, "Reply-To")) != 0 || snapshot.IdentityID != "" || snapshot.IdentitySMTP {
		t.Fatalf("account way snapshot = %+v", snapshot)
	}

	// Through an identity's own server: its route, not its name, address or
	// Reply-To.
	queued, snapshot = queuedComposeSend(t, h, db, base(url.Values{"from_email": {"deals@example.org"}, "send_via": {shop.ID}}))
	from = mimeAddresses(t, queued.MIMEData, "From")
	if len(from) != 1 || from[0].Address != "deals@example.org" || from[0].Name != "" || len(mimeAddresses(t, queued.MIMEData, "Reply-To")) != 0 {
		t.Fatalf("identity server From = %v", from)
	}
	if snapshot.IdentityID != shop.ID || !snapshot.IdentitySMTP || queued.Transport != storage.OutgoingTransportSMTP {
		t.Fatalf("identity server snapshot = %+v transport %q", snapshot, queued.Transport)
	}

	for name, extra := range map[string]url.Values{
		"not an address":         {"from_email": {"deals@"}},
		"wildcard":               {"from_email": {"*@example.org"}},
		"identity without smtp":  {"from_email": {"deals@example.org"}, "send_via": {sales.ID}},
		"missing identity":       {"from_email": {"deals@example.org"}, "send_via": {"missing"}},
		"another user's account": {"from_email": {"deals@example.org"}, "account_id": {"attacker-account"}},
	} {
		rec := httptest.NewRecorder()
		h.handleCompose(rec, composeIdentityRequest(t, "/compose", base(extra)))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d body = %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestOneTimeFromFollowsTheAccountRule(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	if _, err := db.Write().Exec(`UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceProviderIdentities(t.Context(), "victim-account", []models.Identity{{ProviderIdentityID: "alias", Email: "alias@example.com"}}); err != nil {
		t.Fatal(err)
	}
	account, err := h.ownedAccount(composeIdentityRequest(t, "/", nil).Context(), "victim-account")
	if err != nil || account == nil {
		t.Fatalf("account = %v, %v", account, err)
	}
	ctx := composeIdentityRequest(t, "/", nil).Context()
	if _, err := h.oneTimeComposeSender(ctx, account, "", "someone@example.net", "", true); !errors.Is(err, errComposeFromNotAllowed) {
		t.Fatalf("unlisted Gmail address err = %v", err)
	}
	// A draft may keep it until the way of sending changes.
	if sender, err := h.oneTimeComposeSender(ctx, account, "", "someone@example.net", "", false); err != nil || sender.Email != "someone@example.net" {
		t.Fatalf("draft sender = %+v, %v", sender, err)
	}
	// Addresses Gmail lists go out under any name.
	for _, email := range []string{"OWNER@example.com", "alias@example.com"} {
		if sender, err := h.oneTimeComposeSender(ctx, account, "Another Name", email, "", true); err != nil || sender.Name != "Another Name" || sender.IdentityID != "" {
			t.Fatalf("listed %s = %+v, %v", email, sender, err)
		}
	}
}

func TestDraftSenderOfOneTimeFrom(t *testing.T) {
	account := &models.Account{ID: "acct", Name: "Ana", Email: "ana@example.com"}
	identities := []models.Identity{
		{ID: "sales", Email: "sales@example.org"},
		{ID: "shop", Email: "shop@example.org", SMTPHost: "smtp.example.org"},
	}
	if sender, oneTime := draftSender(account, identities, "Sales", "sales@example.org"); oneTime || sender.IdentityID != "sales" {
		t.Fatalf("identity = %+v, %v", sender, oneTime)
	}
	if sender, oneTime := draftSender(account, identities, "Other", "ANA@example.com"); oneTime || sender.IdentityID != "" || sender.Email != "ana@example.com" {
		t.Fatalf("account = %+v, %v", sender, oneTime)
	}
	if sender, oneTime := draftSender(account, identities, " Deals ", "deals@Example.org"); !oneTime || sender.Name != "Deals" || sender.IdentityID != "shop" || !sender.OwnSMTP {
		t.Fatalf("same domain as an own-server identity = %+v, %v", sender, oneTime)
	}
	if sender, oneTime := draftSender(account, identities, "", "me+tag@example.com"); !oneTime || sender.IdentityID != "" || sender.OwnSMTP {
		t.Fatalf("other domain = %+v, %v", sender, oneTime)
	}
}

func TestScheduledOneTimeFromKeepsItsWayOfSending(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	shop, err := db.SaveAccountIdentityGuarded(t.Context(), "owner", models.Identity{AccountID: "victim-account", Name: "Shop", Email: "shop@example.org", SMTPHost: "smtp.example.org", SMTPPort: 587, SMTPTLSMode: "starttls", SMTPUsername: "shop-login"}, nil)
	if err != nil {
		t.Fatalf("save shop identity: %v", err)
	}
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Truncate(5 * time.Minute)
	schedule := func(subject string, extra url.Values) *httptest.ResponseRecorder {
		form := url.Values{
			"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {subject}, "body": {"Later"},
			"schedule_timezone": {"UTC"}, "schedule_date": {tomorrow.Format("2006-01-02")}, "schedule_hour": {strconv.Itoa(tomorrow.Hour())}, "schedule_minute": {strconv.Itoa(tomorrow.Minute())},
		}
		for key, values := range extra {
			form[key] = values
		}
		rec := httptest.NewRecorder()
		h.handleComposeSchedule(rec, composeIdentityRequest(t, "/compose/schedule", form))
		return rec
	}
	queuedFor := func(subject string) (storage.OutgoingSend, outgoingMessageSnapshot, int64) {
		t.Helper()
		var localID int64
		if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id = 'victim-account' AND subject = ?`, subject).Scan(&localID); err != nil {
			t.Fatalf("draft row %q: %v", subject, err)
		}
		queued, err := db.OutgoingSendForMessageInternal(t.Context(), localID)
		if err != nil || queued == nil {
			t.Fatalf("queued %q = %v, %v", subject, queued, err)
		}
		var snapshot outgoingMessageSnapshot
		if err := json.Unmarshal(queued.MessageJSON, &snapshot); err != nil {
			t.Fatal(err)
		}
		return *queued, snapshot, localID
	}

	// Chosen the account's way although the identity's server is at the
	// same domain: the choice stands.
	if rec := schedule("Account way", url.Values{"from_name": {"Deals"}, "from_email": {"deals@example.org"}}); rec.Code != http.StatusOK {
		t.Fatalf("schedule status = %d body = %q", rec.Code, rec.Body.String())
	}
	queued, snapshot, _ := queuedFor("Account way")
	if snapshot.FromEmail != "deals@example.org" || snapshot.FromName != "Deals" || snapshot.IdentitySMTP || queued.Transport != storage.OutgoingTransportSMTP || queued.EnvelopeFrom != "deals@example.org" {
		t.Fatalf("account way = %+v transport %q", snapshot, queued.Transport)
	}

	if rec := schedule("Shop way", url.Values{"from_email": {"deals@example.org"}, "send_via": {shop.ID}}); rec.Code != http.StatusOK {
		t.Fatalf("schedule status = %d body = %q", rec.Code, rec.Body.String())
	}
	queued, snapshot, localID := queuedFor("Shop way")
	if !snapshot.IdentitySMTP || snapshot.IdentityID != shop.ID || snapshot.FromEmail != "deals@example.org" {
		t.Fatalf("shop way = %+v", snapshot)
	}
	// Reopened, the draft comes back with the way of sending it waits for.
	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+strconv.FormatInt(localID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(localID, 10))
	rec := httptest.NewRecorder()
	h.handleGetDraft(rec, ownerRequest(req))
	var draft struct {
		IdentityID  string            `json:"identity_id"`
		OneTimeFrom map[string]string `json:"one_time_from"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&draft); err != nil {
		t.Fatal(err)
	}
	if draft.IdentityID != "" || draft.OneTimeFrom["email"] != "deals@example.org" || draft.OneTimeFrom["send_via"] != shop.ID {
		t.Fatalf("draft = %+v", draft)
	}

	// Saving it again the account's way moves the waiting send off the
	// identity's server.
	rec = httptest.NewRecorder()
	h.handleComposeDraft(rec, composeIdentityRequest(t, "/compose/draft", url.Values{
		"account_id": {"victim-account"}, "draft_id": {queued.DraftID}, "to": {"recipient@example.com"}, "subject": {"Shop way"}, "body": {"Edited"},
		"from_email": {"deals@example.org"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft status = %d body = %q", rec.Code, rec.Body.String())
	}
	if _, snapshot, _ = queuedFor("Shop way"); snapshot.IdentitySMTP || snapshot.IdentityID != "" || snapshot.TextBody != "Edited" {
		t.Fatalf("after edit = %+v", snapshot)
	}

	// A From the account's way refuses is refused before any draft is kept.
	if _, err := db.Write().Exec(`UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	if rec := schedule("Refused", url.Values{"from_email": {"deals@example.org"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("refused status = %d body = %q", rec.Code, rec.Body.String())
	}
	var drafts int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE subject = 'Refused'`).Scan(&drafts); err != nil || drafts != 0 {
		t.Fatalf("refused drafts = %d, %v", drafts, err)
	}
}
