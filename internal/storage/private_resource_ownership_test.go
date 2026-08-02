package storage

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedSecondOwnershipUser(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, email, name) VALUES ('attacker', 'attacker@example.com', 'Attacker')`); err != nil {
		t.Fatalf("insert attacker: %v", err)
	}
}

func TestContactProfileConflictCannotCrossUserBoundary(t *testing.T) {
	db := newContactsTestDB(t)
	seedSecondOwnershipUser(t, db)
	ownerProfile, err := db.SaveContactProfile(t.Context(), "default", models.ContactProfile{
		ID:           "shared-profile-id",
		DisplayName:  "Owner Contact",
		PrimaryEmail: "owner-contact@example.com",
		Cards:        []models.ContactCard{{ID: "owner-card", Kind: "local"}},
		Fields:       []models.ContactField{{ID: "owner-email", Kind: "email", Value: "owner-contact@example.com", IsPrimary: true, Source: "manual"}},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile(owner) error = %v", err)
	}

	_, err = db.SaveContactProfile(t.Context(), "attacker", models.ContactProfile{
		ID:           ownerProfile.ID,
		DisplayName:  "Stolen Contact",
		PrimaryEmail: "attacker@example.com",
		Cards:        []models.ContactCard{{Kind: "local"}},
		Fields:       []models.ContactField{{Kind: "email", Value: "attacker@example.com", IsPrimary: true, Source: "manual"}},
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SaveContactProfile(attacker) error = %v, want sql.ErrNoRows", err)
	}

	unchanged, err := db.GetContactProfile(t.Context(), "default", ownerProfile.ID)
	if err != nil || unchanged == nil {
		t.Fatalf("GetContactProfile(owner) = %#v, %v", unchanged, err)
	}
	if unchanged.DisplayName != "Owner Contact" || unchanged.PrimaryEmail != "owner-contact@example.com" {
		t.Fatalf("owner profile changed = %#v", unchanged)
	}
	if attackerProfile, err := db.GetContactProfile(t.Context(), "attacker", ownerProfile.ID); err != nil || attackerProfile != nil {
		t.Fatalf("GetContactProfile(attacker) = %#v, %v, want nil", attackerProfile, err)
	}

	_, err = db.SaveContact(t.Context(), "attacker", models.Contact{ID: ownerProfile.ID, Name: "Stolen Again", Email: "attacker@example.com"})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SaveContact(attacker) error = %v, want sql.ErrNoRows", err)
	}
}

func TestSignatureAndPushConflictsCannotCrossUserBoundary(t *testing.T) {
	db := newContactsTestDB(t)
	seedSecondOwnershipUser(t, db)
	ownerSignature, err := db.SaveSignature(t.Context(), "default", models.Signature{ID: "shared-signature", Name: "Owner", TextBody: "owner body"})
	if err != nil {
		t.Fatalf("SaveSignature(owner) error = %v", err)
	}
	if _, err := db.SaveSignature(t.Context(), "attacker", models.Signature{ID: ownerSignature.ID, Name: "Attacker", TextBody: "stolen"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SaveSignature(attacker) error = %v, want sql.ErrNoRows", err)
	}
	if err := db.DeleteSignature(t.Context(), "attacker", ownerSignature.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteSignature(attacker) error = %v, want sql.ErrNoRows", err)
	}
	unchangedSignature, err := db.GetSignature(t.Context(), "default", ownerSignature.ID)
	if err != nil || unchangedSignature.Name != "Owner" || unchangedSignature.TextBody != "owner body" {
		t.Fatalf("owner signature changed = %#v, %v", unchangedSignature, err)
	}

	ownerSubscription := WebPushSubscription{Endpoint: "https://push.example/subscription", UserID: "default", P256DH: "owner-p256dh", Auth: "owner-auth", UserAgent: "owner-agent"}
	if err := db.SaveWebPushSubscription(t.Context(), ownerSubscription); err != nil {
		t.Fatalf("SaveWebPushSubscription(owner) error = %v", err)
	}
	foreignSubscription := ownerSubscription
	foreignSubscription.UserID = "attacker"
	foreignSubscription.P256DH = "attacker-p256dh"
	foreignSubscription.Auth = "attacker-auth"
	if err := db.SaveWebPushSubscription(t.Context(), foreignSubscription); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SaveWebPushSubscription(attacker) error = %v, want sql.ErrNoRows", err)
	}
	subscriptions, err := db.ListWebPushSubscriptions(t.Context(), "default")
	if err != nil || len(subscriptions) != 1 {
		t.Fatalf("ListWebPushSubscriptions(owner) = %#v, %v", subscriptions, err)
	}
	if subscriptions[0].P256DH != ownerSubscription.P256DH || subscriptions[0].Auth != ownerSubscription.Auth {
		t.Fatalf("owner subscription changed = %#v", subscriptions[0])
	}
	if attackerSubscriptions, err := db.ListWebPushSubscriptions(t.Context(), "attacker"); err != nil || len(attackerSubscriptions) != 0 {
		t.Fatalf("ListWebPushSubscriptions(attacker) = %#v, %v, want empty", attackerSubscriptions, err)
	}
	ownerSubscription.P256DH = "owner-p256dh-refreshed"
	ownerSubscription.Auth = "owner-auth-refreshed"
	if err := db.SaveWebPushSubscription(t.Context(), ownerSubscription); err != nil {
		t.Fatalf("SaveWebPushSubscription(owner refresh) error = %v", err)
	}
	subscriptions, err = db.ListWebPushSubscriptions(t.Context(), "default")
	if err != nil || len(subscriptions) != 1 || subscriptions[0].P256DH != ownerSubscription.P256DH || subscriptions[0].Auth != ownerSubscription.Auth {
		t.Fatalf("refreshed owner subscription = %#v, %v", subscriptions, err)
	}
}

func TestClearSuppressedContactRequiresOwnership(t *testing.T) {
	db := newContactsTestDB(t)
	seedSecondOwnershipUser(t, db)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO contact_observations (id, user_id, normalized_email, email, is_suppressed, suppress_auto_create)
		VALUES ('owner-observation', 'default', 'owner@example.com', 'owner@example.com', 1, 1)`); err != nil {
		t.Fatalf("insert suppressed contact: %v", err)
	}
	if err := db.ClearSuppressedContact(t.Context(), "attacker", "owner-observation"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ClearSuppressedContact(attacker) error = %v, want sql.ErrNoRows", err)
	}
	var remaining int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM contact_observations WHERE id = 'owner-observation'`).Scan(&remaining); err != nil {
		t.Fatalf("count owner observation: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("owner suppressed contact count = %d, want 1", remaining)
	}
}
