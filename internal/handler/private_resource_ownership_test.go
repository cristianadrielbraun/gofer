package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnerContactSignatureAndPush(t *testing.T, db *storage.DB) (models.ContactProfile, models.Signature, storage.WebPushSubscription) {
	t.Helper()
	profile, err := db.SaveContactProfile(t.Context(), "owner", models.ContactProfile{
		ID:           "owner-private-contact",
		DisplayName:  "Owner Contact",
		PrimaryEmail: "owner-contact@example.com",
		Cards:        []models.ContactCard{{Kind: "local"}},
		Fields:       []models.ContactField{{Kind: "email", Value: "owner-contact@example.com", IsPrimary: true, Source: "manual"}},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile(owner) error = %v", err)
	}
	signature, err := db.SaveSignature(t.Context(), "owner", models.Signature{ID: "owner-private-signature", Name: "Owner Signature", TextBody: "Owner body"})
	if err != nil {
		t.Fatalf("SaveSignature(owner) error = %v", err)
	}
	subscription := storage.WebPushSubscription{Endpoint: "https://push.example/owner", UserID: "owner", P256DH: "owner-p256dh", Auth: "owner-auth", UserAgent: "owner-agent"}
	if err := db.SaveWebPushSubscription(t.Context(), subscription); err != nil {
		t.Fatalf("SaveWebPushSubscription(owner) error = %v", err)
	}
	return profile, signature, subscription
}

func TestPrivateContactMutationsRejectForeignUsersAndAdmins(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[isAdmin], func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			profile, _, _ := seedOwnerContactSignatureAndPush(t, db)

			form := url.Values{"name": {"Stolen"}, "email": {"attacker@example.com"}}
			req := httptest.NewRequest(http.MethodPost, "/api/contacts?id="+profile.ID, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.handleSaveContact(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign save status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}

			req = httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/delete", nil)
			req.SetPathValue("id", profile.ID)
			rec = httptest.NewRecorder()
			h.handleDeleteContact(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign delete status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}

			unchanged, err := db.GetContactProfile(t.Context(), "owner", profile.ID)
			if err != nil || unchanged == nil || unchanged.DisplayName != "Owner Contact" || unchanged.PrimaryEmail != "owner-contact@example.com" {
				t.Fatalf("owner contact changed = %#v, %v", unchanged, err)
			}
		})
	}
}

func TestPrivateSignatureAndPushMutationsRejectForeignUsersAndAdmins(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[isAdmin], func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			_, signature, subscription := seedOwnerContactSignatureAndPush(t, db)

			form := url.Values{"id": {signature.ID}, "name": {"Stolen"}, "text_body": {"stolen body"}}
			req := httptest.NewRequest(http.MethodPost, "/api/signatures", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.handleSaveSignature(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign signature save status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}

			req = httptest.NewRequest(http.MethodDelete, "/api/signatures/"+signature.ID, nil)
			req.SetPathValue("id", signature.ID)
			rec = httptest.NewRecorder()
			h.handleDeleteSignature(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign signature delete status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}

			payload, err := json.Marshal(map[string]any{
				"endpoint": subscription.Endpoint,
				"keys":     map[string]string{"p256dh": "attacker-p256dh", "auth": "attacker-auth"},
			})
			if err != nil {
				t.Fatalf("marshal subscription: %v", err)
			}
			req = httptest.NewRequest(http.MethodPost, "/api/push/subscription", bytes.NewReader(payload))
			rec = httptest.NewRecorder()
			h.handleSavePushSubscription(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign push save status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}

			unchangedSignature, err := db.GetSignature(t.Context(), "owner", signature.ID)
			if err != nil || unchangedSignature.Name != "Owner Signature" || unchangedSignature.TextBody != "Owner body" {
				t.Fatalf("owner signature changed = %#v, %v", unchangedSignature, err)
			}
			subscriptions, err := db.ListWebPushSubscriptions(t.Context(), "owner")
			if err != nil || len(subscriptions) != 1 || subscriptions[0].P256DH != subscription.P256DH || subscriptions[0].Auth != subscription.Auth {
				t.Fatalf("owner subscriptions changed = %#v, %v", subscriptions, err)
			}
		})
	}
}

func TestSignatureSettingsAndSuppressedContactsRejectForeignTargets(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "admin"}[isAdmin], func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			_, signature, _ := seedOwnerContactSignatureAndPush(t, db)
			if _, err := db.Write().ExecContext(t.Context(), `
				INSERT INTO contact_observations (id, user_id, normalized_email, email, is_suppressed, suppress_auto_create)
				VALUES ('owner-suppressed', 'owner', 'owner-suppressed@example.com', 'owner-suppressed@example.com', 1, 1)`); err != nil {
				t.Fatalf("insert suppressed contact: %v", err)
			}

			form := url.Values{
				"new_signature_id": {signature.ID},
				"new_enabled":      {"true"},
			}
			req := httptest.NewRequest(http.MethodPost, "/api/accounts/attacker-account/signature-settings", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", "attacker-account")
			rec := httptest.NewRecorder()
			h.handleSaveAccountSignatureSettings(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign signature setting status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}
			var settingsRows int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM account_signature_settings WHERE account_id = 'attacker-account'`).Scan(&settingsRows); err != nil {
				t.Fatalf("count signature settings: %v", err)
			}
			if settingsRows != 0 {
				t.Fatalf("foreign signature setting wrote %d rows, want 0", settingsRows)
			}

			req = httptest.NewRequest(http.MethodPost, "/api/settings/contacts/suppressed/owner-suppressed/clear", nil)
			req.SetPathValue("id", "owner-suppressed")
			rec = httptest.NewRecorder()
			h.handleClearSuppressedContact(rec, attackerRequestWithAdmin(req, isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("foreign suppressed clear status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}
			var suppressedRows int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM contact_observations WHERE id = 'owner-suppressed'`).Scan(&suppressedRows); err != nil {
				t.Fatalf("count suppressed contacts: %v", err)
			}
			if suppressedRows != 1 {
				t.Fatalf("owner suppressed contact count = %d, want 1", suppressedRows)
			}
		})
	}
}
