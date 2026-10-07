package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserServiceCredentialsCachedAccessAndLocalGrantChecks(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected refresh", 500) }))
	googleScopes := GoogleCalendarReadOnlyScope + " " + GoogleCalendarEventsScope
	graphScopes := strings.Join(append(microsoftGraphMailScopes(), microsoftGraphContactsScope, microsoftGraphCalendarWriteScope), " ")
	f.grant(t, "alice", "alice-google", "alice-refresh", false, googleScopes)
	f.grant(t, "bob", "bob-google", "bob-refresh", false, GoogleCalendarReadOnlyScope)
	f.grant(t, "outlook", "alice-graph", "graph-refresh", false, graphScopes)
	for _, test := range []struct {
		name, expected string
		get            func() (string, error)
	}{
		{"google-read", "alice-google", func() (string, error) {
			return f.service.GetGoogleCalendarTokenForUser(t.Context(), "alice", f.ids["alice"])
		}},
		{"google-write", "alice-google", func() (string, error) {
			return f.service.GetGoogleCalendarWriteTokenForUser(t.Context(), "alice", f.ids["alice"])
		}},
		{"graph-contacts", "alice-graph", func() (string, error) {
			return f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["outlook"])
		}},
		{"graph-read", "alice-graph", func() (string, error) {
			return f.service.GetMicrosoftGraphCalendarTokenForUser(t.Context(), "alice", f.ids["outlook"])
		}},
		{"graph-write", "alice-graph", func() (string, error) {
			return f.service.GetMicrosoftGraphCalendarWriteTokenForUser(t.Context(), "alice", f.ids["outlook"])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.get()
			if err != nil || got != test.expected {
				t.Fatalf("cached service token: %q %v", got, err)
			}
		})
	}
	if !f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["alice"], providers.ProviderGmail) || !f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) {
		t.Fatal("known write grants were lost")
	}
	if f.service.CalendarWriteAuthorizedForUser(t.Context(), "bob", f.ids["bob"], providers.ProviderGmail) || f.service.CalendarWriteAuthorizedForUser(t.Context(), "bob", f.ids["alice"], providers.ProviderGmail) || f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["alice"], providers.ProviderOutlook) {
		t.Fatal("foreign/read-only/provider-mismatched grant was authorized")
	}
	bound := f.service.CalendarAccount("alice", f.ids["alice"], true)
	if _, err := bound.GetGoogleCalendarWriteTokenForAccount(t.Context(), f.ids["bob"]); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatalf("foreign bound service: %v", err)
	}
	if _, err := f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("Google token used for Graph contacts")
	}
	if _, err := f.service.GetGoogleCalendarTokenForUser(t.Context(), "alice", f.ids["outlook"]); err == nil {
		t.Fatal("Graph token used for Google Calendar")
	}
	if _, err := f.service.GetGoogleCalendarWriteTokenForUser(t.Context(), "bob", f.ids["bob"]); err == nil {
		t.Fatal("read-only Google grant became writable")
	}
	if calls.Load() != 0 {
		t.Fatalf("cached/grant checks made %d provider requests", calls.Load())
	}
}
func TestUserServiceCredentialsGraphRefreshPreservesMailboxAndRequestedScope(t *testing.T) {
	for _, purpose := range []userCredentialPurpose{userCredentialContacts, userCredentialCalendarRead, userCredentialCalendarWrite} {
		t.Run(fmt.Sprint(purpose), func(t *testing.T) {
			var calls atomic.Int32
			expected := microsoftGraphContactsScope
			if purpose == userCredentialCalendarRead {
				expected = microsoftGraphCalendarScope
			}
			if purpose == userCredentialCalendarWrite {
				expected = microsoftGraphCalendarWriteScope
			}
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.FormValue("scope") != expected {
					t.Errorf("requested scope: %q", r.FormValue("scope"))
				}
				call := calls.Add(1)
				refresh := "original-refresh"
				if call > 1 {
					refresh = "rotated-refresh"
				}
				if r.FormValue("refresh_token") != refresh {
					t.Errorf("refresh rotation was lost: %q", r.FormValue("refresh_token"))
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "service-access", "refresh_token": "rotated-refresh", "expires_in": 3600, "scope": strings.TrimPrefix(expected, "https://graph.microsoft.com/")})
			}))
			// A write grant is known, while the cached mailbox token is mail-only.
			f.grant(t, "outlook", "mailbox-access", "original-refresh", false, strings.Join(microsoftGraphMailScopes(), " "))
			if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET granted_scopes=? WHERE account_id=?`, microsoftGraphCalendarWriteScope, f.ids["outlook"]); err != nil {
				t.Fatal(err)
			}
			bound := f.service.ContactsAccount("alice", f.ids["outlook"])
			if purpose != userCredentialContacts {
				bound = f.service.CalendarAccount("alice", f.ids["outlook"], purpose == userCredentialCalendarWrite)
			}
			for _, force := range []bool{false, true} {
				var token string
				var err error
				if force {
					token, err = bound.RefreshOAuthTokenForAccount(t.Context(), f.ids["outlook"])
				} else {
					token, err = bound.GetOAuthTokenForAccount(t.Context(), f.ids["outlook"])
				}
				if err != nil || token != "service-access" {
					t.Fatalf("service token: %q %v", token, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("scope refresh calls: %d", calls.Load())
			}
			record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
			if err != nil {
				t.Fatal(err)
			}
			if record.AccessToken != "mailbox-access" || record.RefreshToken != "rotated-refresh" || !recordHasScopes(record.Scopes, microsoftGraphMailScopes()...) || !recordHasScopes(record.grantedScopes, microsoftGraphCalendarWriteScope) {
				t.Fatal("service refresh changed mailbox cache or lost rotation/grants")
			}
			if token, err := f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil || token != "mailbox-access" || calls.Load() != 2 {
				t.Fatalf("mailbox cache was replaced: %q %v calls=%d", token, err, calls.Load())
			}
		})
	}
}
func TestUserServiceCredentialsMailboxRefreshKeepsKnownGraphCalendarGrant(t *testing.T) {
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("scope") != strings.Join(microsoftGraphMailScopes(), " ") {
			t.Errorf("mail refresh scope: %q", r.FormValue("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mail-only", "expires_in": 3600, "scope": strings.Join(microsoftGraphMailScopes(), " ")})
	}))
	f.grant(t, "outlook", "before", "refresh", true, microsoftGraphCalendarWriteScope+" "+microsoftGraphContactsScope)
	if _, err := f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil {
		t.Fatal(err)
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil {
		t.Fatal(err)
	}
	if recordHasScopes(record.Scopes, microsoftGraphCalendarWriteScope) || !recordHasScopes(record.grantedScopes, microsoftGraphCalendarWriteScope, microsoftGraphContactsScope) {
		t.Fatal("narrowed cached token erased the known grant")
	}
	if !f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) {
		t.Fatal("calendar write access disappeared after mail refresh")
	}
	f.grant(t, "outlook", "read-only-reconnect", "new-refresh", false, microsoftGraphCalendarScope)
	if f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) {
		t.Fatal("explicit read-only reconnect retained old write access")
	}
}
func TestUserServiceCredentialsGrantCheckDoesNotWaitForRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"after","expires_in":3600}`)
	}))
	f.grant(t, "outlook", "before", "refresh", true, microsoftGraphCalendarWriteScope)
	result := asyncCredential(func() (string, error) {
		return f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["outlook"])
	})
	awaitSignal(t, entered)
	checked := make(chan bool, 1)
	go func() {
		checked <- f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook)
	}()
	select {
	case allowed := <-checked:
		if !allowed {
			t.Fatal("known grant check failed during refresh")
		}
	case <-time.After(time.Second):
		t.Fatal("grant check waited on the token endpoint")
	}
	unblock()
	if result := awaitCredential(t, result); result.err != nil {
		t.Fatal(result.err)
	}
}
func TestUserServiceCredentialsRejectLateGraphRotation(t *testing.T) {
	for _, change := range []string{"reconnect", "identity", "disabled", "deletion", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"stale","refresh_token":"stale-rotation","expires_in":3600,"scope":"`+microsoftGraphContactsScope+`"}`)
			}))
			f.grant(t, "outlook", "mail-before", "refresh-before", false, strings.Join(microsoftGraphMailScopes(), " "))
			result := asyncCredential(func() (string, error) {
				return f.service.ContactsAccount("alice", f.ids["outlook"]).GetMicrosoftGraphContactsTokenForAccount(t.Context(), f.ids["outlook"])
			})
			awaitSignal(t, entered)
			f.grant(t, "bob", "bob-cached", "bob-refresh", false, GoogleCalendarReadOnlyScope)
			if token, err := f.service.GetGoogleCalendarTokenForUser(t.Context(), "bob", f.ids["bob"]); err != nil || token != "bob-cached" {
				t.Fatal("provider wait pinned the one-slot user cache")
			}
			switch change {
			case "reconnect":
				f.grant(t, "outlook", "reconnected", "reconnect-refresh", false, microsoftGraphCalendarScope)
			case "identity":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='changed' WHERE id=?`, f.ids["outlook"])
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "deletion":
				if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.ids["outlook"]); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				f.cancel()
			}
			unblock()
			got := awaitCredential(t, result)
			if got.err == nil || got.token != "" {
				t.Fatal("late service token/rotation escaped")
			}
			var revision int64
			if err := f.system.Read().QueryRow(`SELECT revision FROM gofer_mailbox_credentials WHERE account_id=?`, f.ids["outlook"]).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			expected := int64(1)
			if change == "reconnect" {
				expected = 2
			}
			if revision != expected {
				t.Fatalf("late refresh changed revision: %d", revision)
			}
		})
	}
}
func TestUserServiceCredentialsOlderSchemaKeepsOnlyRecordedGrants(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			f := newUserCredentialFixture(t, nil)
			scopes := strings.Join(microsoftGraphMailScopes(), " ")
			if known {
				scopes += " " + microsoftGraphCalendarWriteScope
			}
			f.grant(t, "outlook", "cached", "refresh", false, scopes)
			if _, err := f.system.Write().Exec(`ALTER TABLE gofer_mailbox_credentials DROP COLUMN granted_scopes`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service, err := NewUserCredentials(ctx, f.service.codec.config, f.routing, testMailboxCredentialKey)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); service.Wait() }()
			if service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) {
				t.Fatal("inactive owner was authorized during schema upgrade")
			}
			if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
				t.Fatal(err)
			}
			if service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) != known {
				t.Fatal("upgrade lost recorded grant or invented an unknown grant")
			}
		})
	}
}

func TestUserServiceCredentialsMissingGraphScopeCannotRelabelCachedAccess(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-for-" + r.FormValue("scope"), "expires_in": 3600})
	}))
	f.grant(t, "outlook", "before", "refresh", true, microsoftGraphCalendarWriteScope)
	if _, err := f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil {
		t.Fatal(err)
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil {
		t.Fatal(err)
	}
	if record.Scopes != "" || !recordHasScopes(record.grantedScopes, microsoftGraphCalendarWriteScope) {
		t.Fatal("missing scope reused old token permissions or erased the known grant")
	}
	if token, err := f.service.GetMicrosoftGraphCalendarTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil || token != "fresh-for-"+microsoftGraphCalendarScope || calls.Load() != 2 {
		t.Fatalf("calendar reused an unknown mailbox token: %q %v calls=%d", token, err, calls.Load())
	}
}
func TestUserServiceCredentialsGoogleRefreshPreservesPurposeAndRevokedAccess(t *testing.T) {
	for _, write := range []bool{true, false} {
		t.Run(fmt.Sprint(write), func(t *testing.T) {
			var calls atomic.Int32
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "google-refreshed", "expires_in": 3600, "scope": GoogleCalendarReadOnlyScope})
			}))
			f.grant(t, "alice", "before", "refresh", false, GoogleCalendarReadOnlyScope+" "+GoogleCalendarEventsScope)
			bound := f.service.CalendarAccount("alice", f.ids["alice"], write)
			token, err := bound.RefreshOAuthTokenForAccount(t.Context(), f.ids["alice"])
			if write {
				if err == nil || token != "" {
					t.Fatal("revoked write permission returned a write token")
				}
			} else {
				if err != nil || token != "google-refreshed" {
					t.Fatal(token, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("forced Google refresh count: %d", calls.Load())
			}
			if f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["alice"], providers.ProviderGmail) {
				t.Fatal("explicit refreshed read-only Google scope retained write access")
			}
		})
	}
}
func TestUserServiceCredentialsScopeFailuresAndRetryHints(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"temporarily_unavailable"}`)
			}))
			f.grant(t, "outlook", "before", "refresh", false, strings.Join(microsoftGraphMailScopes(), " "))
			token, err := f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["outlook"])
			var retryErr *OAuthTokenError
			if token != "" || !errors.As(err, &retryErr) || retryErr.Status != status || !retryErr.RetryAt.After(time.Now()) {
				t.Fatalf("scoped failure/retry hint: %q %v", token, err)
			}
			record, loadErr := f.service.load(t.Context(), "alice", f.ids["outlook"])
			if loadErr != nil || record.AccessToken != "before" || record.RefreshToken != "refresh" || record.revision != 1 {
				t.Fatal("failed refresh changed the grant/cache")
			}
		})
	}
}

func TestUserServiceCredentialsGraphScopeAliasesStayResourceBound(t *testing.T) {
	cases := []struct {
		scopes  string
		allowed bool
	}{
		{"Calendars.ReadWrite Mail.ReadWrite Mail.Send MailboxSettings.ReadWrite Contacts.ReadWrite", true},
		{strings.Join(append(microsoftGraphMailScopes(), microsoftGraphContactsScope, microsoftGraphCalendarWriteScope), " "), true},
		{"Calendars.Read", false}, {"https://outlook.office.com/Calendars.ReadWrite", false},
		{"api://custom/Calendars.ReadWrite", false}, {"Calendars.ReadWrite.Shared", false},
	}
	for _, test := range cases {
		t.Run(test.scopes, func(t *testing.T) {
			var calls atomic.Int32
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected refresh", 500) }))
			f.grant(t, "outlook", "cached", "refresh", false, test.scopes)
			if f.service.CalendarWriteAuthorizedForUser(t.Context(), "alice", f.ids["outlook"], providers.ProviderOutlook) != test.allowed {
				t.Fatal("scope alias crossed a resource/permission boundary")
			}
			if test.allowed {
				for _, get := range []func() (string, error){
					func() (string, error) {
						return f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["outlook"])
					},
					func() (string, error) {
						return f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["outlook"])
					},
					func() (string, error) {
						return f.service.GetMicrosoftGraphCalendarTokenForUser(t.Context(), "alice", f.ids["outlook"])
					},
					func() (string, error) {
						return f.service.GetMicrosoftGraphCalendarWriteTokenForUser(t.Context(), "alice", f.ids["outlook"])
					},
				} {
					if token, err := get(); err != nil || token != "cached" {
						t.Fatal(token, err)
					}
				}
			}
			if calls.Load() != 0 {
				t.Fatal("grant/cached scope checks made network requests")
			}
		})
	}
}
func TestUserServiceCredentialsRejectPartialGraphResponseScope(t *testing.T) {
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "read-only", "refresh_token": "rotated", "expires_in": 3600, "scope": "Calendars.Read"})
	}))
	f.grant(t, "outlook", "before", "refresh", true, microsoftGraphCalendarWriteScope)
	if token, err := f.service.GetMicrosoftGraphCalendarWriteTokenForUser(t.Context(), "alice", f.ids["outlook"]); err == nil || token != "" {
		t.Fatal("partial read grant escaped as a write token")
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil || record.AccessToken != "before" || record.RefreshToken != "rotated" {
		t.Fatal("partial response lost rotation or replaced mailbox cache")
	}
}

func TestUserServiceCredentialsFailedPublicationCannotLeakTokenOrRotation(t *testing.T) {
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-contact", "refresh_token": "rotated", "expires_in": 3600, "scope": "Contacts.ReadWrite"})
	}))
	f.grant(t, "outlook", "mailbox-before", "refresh-before", false, strings.Join(microsoftGraphMailScopes(), " "))
	if _, err := f.system.Write().Exec(`CREATE TRIGGER fail_service_rotation BEFORE UPDATE ON gofer_mailbox_credentials BEGIN SELECT RAISE(ABORT,'fixture publication failure');END`); err != nil {
		t.Fatal(err)
	}
	if token, err := f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["outlook"]); err == nil || token != "" {
		t.Fatal("unpublished token escaped")
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil || record.AccessToken != "mailbox-before" || record.RefreshToken != "refresh-before" || record.revision != 1 || ownedGraphHasScopes(record.grantedScopes, microsoftGraphContactsScope) {
		t.Fatal("failed publication changed credentials or known grants")
	}
	if _, err := f.system.Write().Exec(`DROP TRIGGER fail_service_rotation`); err != nil {
		t.Fatal(err)
	}
	if token, err := f.service.GetMicrosoftGraphContactsTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil || token != "fresh-contact" {
		t.Fatal(token, err)
	}
}
func TestUserServiceCredentialsGoogleRetryHintAndEmptyServiceToken(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if empty {
					fmt.Fprint(w, `{"access_token":"","expires_in":3600}`)
				} else {
					w.Header().Set("Retry-After", "90")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":"temporarily_unavailable"}`)
				}
			}))
			f.grant(t, "alice", "before", "refresh", true, GoogleCalendarReadOnlyScope)
			token, err := f.service.GetGoogleCalendarTokenForUser(t.Context(), "alice", f.ids["alice"])
			if err == nil || token != "" {
				t.Fatal("failed Google service refresh returned a token")
			}
			if !empty {
				var retryErr *OAuthTokenError
				if !errors.As(err, &retryErr) || !retryErr.RetryAt.After(time.Now()) {
					t.Fatalf("Google retry hint: %v", err)
				}
			}
			record, loadErr := f.service.load(t.Context(), "alice", f.ids["alice"])
			if loadErr != nil || record.revision != 1 || record.AccessToken != "before" || record.RefreshToken != "refresh" {
				t.Fatal("failed refresh changed cached authorization")
			}
		})
	}
}
