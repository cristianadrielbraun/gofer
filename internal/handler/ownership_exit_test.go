package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type countingAvatarTransport struct {
	calls atomic.Int32
}

type ownershipExitFixture struct {
	handler      *Handler
	db           *storage.DB
	message      victimMessageFixture
	contact      models.ContactProfile
	signature    models.Signature
	subscription storage.WebPushSubscription
	draftID      int64
	outgoingID   string
}

func newOwnershipExitFixture(t *testing.T) ownershipExitFixture {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	messageFixture := insertVictimReadableMessage(t, h, db)
	contact, signature, subscription := seedOwnerContactSignatureAndPush(t, db)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders(drafts) error = %v", err)
	}
	draftID, err := db.SaveDraftMessage(t.Context(), storage.DraftMessageInput{
		AccountID: "victim-account", FolderID: "victim-drafts", InternetMessageID: "<victim-draft@example.com>",
		Subject: "Victim draft", FromEmail: "owner@example.com", ToRecipients: []storage.Recipient{{Email: "friend@example.com"}},
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	outgoing, err := db.QueueOutgoingSend(t.Context(), storage.QueueOutgoingSendInput{
		ID: "victim-outgoing", AccountID: "victim-account", MessageID: draftID, DraftID: "<victim-draft@example.com>",
		Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: "owner@example.com", EnvelopeRecipients: []string{"friend@example.com"},
		MIMEData:    []byte("Message-ID: <victim-outgoing@example.com>\r\n\r\nVictim outgoing body"),
		MessageJSON: []byte(`{"subject":"Victim outgoing"}`), SendAfter: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO message_mutations (
			id, account_id, message_id, folder_id, provider_type, kind, target_value,
			status, attempt_count, last_error, next_attempt_at
		) VALUES ('victim-operation', 'victim-account', ?, 'victim-inbox', 'imap', 'read', 1,
		          'failed', 1, 'victim operation error', CURRENT_TIMESTAMP)
	`, messageFixture.messageID); err != nil {
		t.Fatalf("insert mail operation: %v", err)
	}
	if err := db.SetUISettings(t.Context(), "owner", map[string]string{"timezone": "Victim/Secret"}); err != nil {
		t.Fatalf("SetUISettings(owner) error = %v", err)
	}
	return ownershipExitFixture{
		handler: h, db: db, message: messageFixture, contact: contact, signature: signature,
		subscription: subscription, draftID: draftID, outgoingID: outgoing.ID,
	}
}

func TestOwnershipExitFixtureKeepsPrivateResourceReadsUserScoped(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[isAdmin], func(t *testing.T) {
			fixture := newOwnershipExitFixture(t)
			tests := []struct {
				name       string
				path       string
				pathValues map[string]string
				handle     http.HandlerFunc
			}{
				{name: "message", path: "/email/" + strconv.FormatInt(fixture.message.messageID, 10), pathValues: map[string]string{"id": strconv.FormatInt(fixture.message.messageID, 10)}, handle: fixture.handler.handleEmailPartial},
				{name: "contact", path: "/api/contacts/" + fixture.contact.ID + "/export", pathValues: map[string]string{"id": fixture.contact.ID}, handle: fixture.handler.handleExportContact},
				{name: "draft", path: "/api/drafts/" + strconv.FormatInt(fixture.draftID, 10), pathValues: map[string]string{"id": strconv.FormatInt(fixture.draftID, 10)}, handle: fixture.handler.handleGetDraft},
				{name: "attachment", path: "/api/attachments/" + strconv.FormatInt(fixture.message.attachmentID, 10) + "/download", pathValues: map[string]string{"id": strconv.FormatInt(fixture.message.attachmentID, 10)}, handle: fixture.handler.handleAttachmentDownload},
				{name: "outgoing send", path: "/api/outgoing-sends/" + fixture.outgoingID, pathValues: map[string]string{"id": fixture.outgoingID}, handle: fixture.handler.handleOutgoingSendGet},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, tt.path, nil)
					for key, value := range tt.pathValues {
						req.SetPathValue(key, value)
					}
					rec := httptest.NewRecorder()
					tt.handle(rec, attackerRequestWithAdmin(req, isAdmin))
					if rec.Code != http.StatusNotFound || strings.Contains(strings.ToLower(rec.Body.String()), "victim") {
						t.Fatalf("status = %d body = %q, want opaque 404", rec.Code, rec.Body.String())
					}
				})
			}

			operationsReq := httptest.NewRequest(http.MethodGet, "/api/mail-operations", nil)
			operationsRec := httptest.NewRecorder()
			fixture.handler.handleMailOperations(operationsRec, attackerRequestWithAdmin(operationsReq, isAdmin))
			if operationsRec.Code != http.StatusOK || strings.Contains(strings.ToLower(operationsRec.Body.String()), "victim") {
				t.Fatalf("foreign operations status = %d body = %q", operationsRec.Code, operationsRec.Body.String())
			}

			settingsReq := httptest.NewRequest(http.MethodGet, "/api/settings/ui", nil)
			settingsRec := httptest.NewRecorder()
			fixture.handler.handleGetUISettings(settingsRec, attackerRequestWithAdmin(settingsReq, isAdmin))
			if settingsRec.Code != http.StatusOK || strings.Contains(settingsRec.Body.String(), "Victim/Secret") {
				t.Fatalf("foreign settings status = %d body = %q", settingsRec.Code, settingsRec.Body.String())
			}

			ownerSignature, err := fixture.db.GetSignature(t.Context(), "owner", fixture.signature.ID)
			if err != nil || ownerSignature.TextBody != "Owner body" {
				t.Fatalf("owner signature changed = %#v, %v", ownerSignature, err)
			}
			ownerSubscriptions, err := fixture.db.ListWebPushSubscriptions(t.Context(), "owner")
			if err != nil || len(ownerSubscriptions) != 1 || ownerSubscriptions[0].Endpoint != fixture.subscription.Endpoint {
				t.Fatalf("owner subscriptions changed = %#v, %v", ownerSubscriptions, err)
			}
			outgoing, err := fixture.db.GetOutgoingSend(t.Context(), fixture.outgoingID)
			if err != nil || outgoing.Status != storage.OutgoingSendPending || outgoing.AttemptCount != 0 {
				t.Fatalf("owner outgoing changed = %#v, %v", outgoing, err)
			}
		})
	}
}

func (transport *countingAvatarTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(strings.NewReader("provider image")),
		Request:    req,
	}, nil
}

func TestAvatarBoundariesRejectForeignUsersAndAdmins(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		role := "user"
		if isAdmin {
			role = "admin"
		}
		t.Run(role, func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			fixture := insertVictimReadableMessage(t, h, db)
			_ = fixture
			const email = "sender@example.com"
			hash := avatarresolver.GravatarHash(email)
			if err := db.SaveSenderAvatarFound(t.Context(), hash, email, "gravatar", "image/png", "", []byte("victim avatar"), time.Now().Add(time.Hour), "found", "unchecked"); err != nil {
				t.Fatalf("SaveSenderAvatarFound() error = %v", err)
			}

			avatarReq := httptest.NewRequest(http.MethodGet, "/api/avatars/"+hash, nil)
			avatarReq.SetPathValue("hash", hash)
			avatarRec := httptest.NewRecorder()
			h.handleAvatarImage(avatarRec, attackerRequestWithAdmin(avatarReq, isAdmin))
			if avatarRec.Code != http.StatusNotFound || strings.Contains(avatarRec.Body.String(), "victim avatar") {
				t.Fatalf("foreign avatar status = %d body = %q, want opaque 404", avatarRec.Code, avatarRec.Body.String())
			}

			const privateWarmupEmail = "private-warmup@example.com"
			if _, err := db.Write().ExecContext(t.Context(), `
				INSERT INTO messages (account_id, internet_message_id, from_email)
				VALUES ('victim-account', '<private-warmup@example.com>', ?)
			`, privateWarmupEmail); err != nil {
				t.Fatalf("insert private warmup sender: %v", err)
			}
			warmupReq := httptest.NewRequest(http.MethodPost, "/api/avatars/warmup", strings.NewReader(`{"emails":["private-warmup@example.com"]}`))
			warmupReq.Header.Set("Content-Type", "application/json")
			warmupRec := httptest.NewRecorder()
			h.handleAvatarWarmup(warmupRec, attackerRequestWithAdmin(warmupReq, isAdmin))
			if warmupRec.Code != http.StatusOK {
				t.Fatalf("foreign warmup status = %d body = %q", warmupRec.Code, warmupRec.Body.String())
			}
			var candidates int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sender_avatars WHERE email_hash = ?`, avatarresolver.GravatarHash(privateWarmupEmail)).Scan(&candidates); err != nil {
				t.Fatalf("count warmup candidates: %v", err)
			}
			if candidates != 0 {
				t.Fatalf("foreign warmup created %d candidates, want 0", candidates)
			}

			const providerURL = "https://lh3.googleusercontent.com/victim-avatar"
			if _, err := db.SaveContactProfile(t.Context(), "owner", models.ContactProfile{
				ID: "owner-avatar-contact", DisplayName: "Owner Avatar", PrimaryEmail: "avatar@example.com",
				AvatarURL: providerURL, Cards: []models.ContactCard{{Kind: "local"}},
				Fields: []models.ContactField{{Kind: "email", Value: "avatar@example.com", IsPrimary: true, Source: "manual"}},
			}); err != nil {
				t.Fatalf("SaveContactProfile() error = %v", err)
			}
			transport := &countingAvatarTransport{}
			h.providerAvatarHTTPClient = &http.Client{Transport: transport}
			providerReq := httptest.NewRequest(http.MethodGet, "/api/provider-avatar?url="+url.QueryEscape(providerURL), nil)
			providerRec := httptest.NewRecorder()
			h.handleProviderAvatarImage(providerRec, attackerRequestWithAdmin(providerReq, isAdmin))
			if providerRec.Code != http.StatusNotFound {
				t.Fatalf("foreign provider avatar status = %d body = %q, want 404", providerRec.Code, providerRec.Body.String())
			}
			if calls := transport.calls.Load(); calls != 0 {
				t.Fatalf("foreign provider avatar made %d outbound calls, want 0", calls)
			}
		})
	}
}

func TestAvatarBoundaryAllowsOwner(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	insertVictimReadableMessage(t, h, db)
	const email = "sender@example.com"
	hash := avatarresolver.GravatarHash(email)
	if err := db.SaveSenderAvatarFound(t.Context(), hash, email, "gravatar", "image/png", "", []byte("owner avatar"), time.Now().Add(time.Hour), "found", "unchecked"); err != nil {
		t.Fatalf("SaveSenderAvatarFound() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/avatars/"+hash, nil)
	req.SetPathValue("hash", hash)
	rec := httptest.NewRecorder()
	h.handleAvatarImage(rec, ownerRequest(req))
	if rec.Code != http.StatusOK || rec.Body.String() != "owner avatar" {
		t.Fatalf("owner avatar status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestContactSyncConfirmationRejectsForeignUsersAndAdminsBeforeMutation(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[isAdmin], func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			profile, err := db.SaveContactProfile(t.Context(), "owner", models.ContactProfile{
				ID: "owner-sync-contact", DisplayName: "Owner Sync", PrimaryEmail: "sync@example.com",
				Cards:  []models.ContactCard{{Kind: "local"}},
				Fields: []models.ContactField{{Kind: "email", Value: "sync@example.com", IsPrimary: true, Source: "manual"}},
			})
			if err != nil {
				t.Fatalf("SaveContactProfile() error = %v", err)
			}
			form := url.Values{"preferred_email": {profile.Fields[0].ID}}
			req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/sync-setup/confirm", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", profile.ID)
			rec := httptest.NewRecorder()
			h.handleConfirmContactSyncSetup(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign confirmation status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}
			unchanged, err := db.GetContactProfile(t.Context(), "owner", profile.ID)
			if err != nil || unchanged == nil || unchanged.SyncEnabled {
				t.Fatalf("owner contact changed = %#v, %v", unchanged, err)
			}
			var operations int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM contact_sync_operations WHERE contact_id = ?`, profile.ID).Scan(&operations); err != nil {
				t.Fatalf("count contact sync operations: %v", err)
			}
			if operations != 0 {
				t.Fatalf("foreign confirmation queued %d operations, want 0", operations)
			}
		})
	}
}

func TestForeignAndMissingAvatarHashesAreIndistinguishable(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	insertVictimReadableMessage(t, h, db)
	foreignHash := avatarresolver.GravatarHash("sender@example.com")
	if err := db.SaveSenderAvatarFound(t.Context(), foreignHash, "sender@example.com", "gravatar", "image/png", "", []byte("victim avatar"), time.Now().Add(time.Hour), "found", "unchecked"); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{foreignHash, strings.Repeat("a", 32)} {
		req := httptest.NewRequest(http.MethodGet, "/api/avatars/"+hash, nil)
		req.SetPathValue("hash", hash)
		rec := httptest.NewRecorder()
		h.handleAvatarImage(rec, attackerRequest(req))
		if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
			t.Fatalf("hash %s status = %d body = %q, want identical 404", strconv.Quote(hash), rec.Code, rec.Body.String())
		}
	}
}
