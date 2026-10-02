package mailauth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarWriteGrantsAreExplicitAndReadAccessRemainsCompatible(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "calendar-write.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES ('owner','owner','owner');
		INSERT INTO accounts(id,user_id,provider,email_address,provider_account_id) VALUES ('google','owner','gmail','g@example.com','google-subject'),('microsoft','owner','outlook','m@example.com','microsoft-subject');`); err != nil {
		t.Fatal(err)
	}
	m := New(nil, db, testMailboxCredentialKey)
	expires := time.Now().Add(time.Hour)
	for _, tc := range []struct{ id, provider, readScope, writeScope string }{
		{"google", providers.OAuthGoogle, GoogleCalendarReadOnlyScope, GoogleCalendarReadOnlyScope + " " + GoogleCalendarEventsScope},
		{"microsoft", providers.OAuthMicrosoft, microsoftGraphCalendarScope, microsoftGraphCalendarWriteScope},
	} {
		t.Run(tc.id, func(t *testing.T) {
			accountProvider := providers.ProviderGmail
			if tc.id == "microsoft" {
				accountProvider = providers.ProviderOutlook
			}
			if err := m.UpsertOAuthAccount(t.Context(), tc.id, tc.provider, tc.id+"-subject", "cached", "refresh", "Bearer", &expires, tc.readScope); err != nil {
				t.Fatal(err)
			}
			if m.CalendarWriteAuthorized(t.Context(), tc.id, accountProvider) {
				t.Fatal("read-only grant was promoted to write")
			}
			if tc.id == "google" {
				if _, err := m.GetGoogleCalendarWriteTokenForAccount(t.Context(), tc.id); err == nil {
					t.Fatal("read-only Google token allowed creation")
				}
			} else {
				if _, err := m.GetMicrosoftGraphCalendarWriteTokenForAccount(t.Context(), tc.id); err == nil {
					t.Fatal("read-only Graph token allowed creation")
				}
			}
			if err := m.UpsertOAuthAccount(t.Context(), tc.id, tc.provider, tc.id+"-subject", "cached", "refresh", "Bearer", &expires, tc.writeScope); err != nil {
				t.Fatal(err)
			}
			if !m.CalendarWriteAuthorized(t.Context(), tc.id, accountProvider) {
				t.Fatal("explicit write grant was not recognized")
			}
			var read, write string
			var readErr, writeErr error
			if tc.id == "google" {
				read, readErr = m.GetGoogleCalendarTokenForAccount(t.Context(), tc.id)
				write, writeErr = m.GetGoogleCalendarWriteTokenForAccount(t.Context(), tc.id)
			} else {
				read, readErr = m.GetMicrosoftGraphCalendarTokenForAccount(t.Context(), tc.id)
				write, writeErr = m.GetMicrosoftGraphCalendarWriteTokenForAccount(t.Context(), tc.id)
			}
			if readErr != nil || writeErr != nil || read != "cached" || write != "cached" {
				t.Fatalf("read=%s/%v write=%s/%v", read, readErr, write, writeErr)
			}
		})
	}
	if m.CalendarWriteAuthorized(t.Context(), "google", providers.ProviderOutlook) {
		t.Fatal("cross-provider token accepted")
	}
}
