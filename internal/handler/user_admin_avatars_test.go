package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedAdminAvatarCache(t *testing.T, f *ownedAdminDiagnosticFixture) {
	t.Helper()
	for _, email := range []string{"sender@example.com", "alice@test.invalid", "bob@test.invalid", "second@test.invalid", "central@invalid.test", "orphan@example.com"} {
		hash := avatarresolver.GravatarHash(email)
		if err := f.system.SaveSenderAvatarError(t.Context(), hash, email, "gravatar", "provider timeout", time.Now().Add(time.Hour), "error", "missing"); err != nil {
			t.Fatal(err)
		}
		if err := f.system.RecordSenderAvatarAttempt(t.Context(), hash, email, "gravatar", "error", "provider timeout"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnedAdminAvatarVisibilityUsesMailboxStores(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	seedOwnedAdminAvatarCache(t, f)
	for _, selected := range []string{"", "alice", "bob", "unused"} {
		want := map[string]int{"": 6, "alice": 2, "bob": 3, "unused": 0}[selected]
		for _, path := range []string{"/api/admin/avatars/senders", "/api/admin/avatars/attempts"} {
			response := f.request(path + "?user_id=" + selected + "&limit=1&offset=1&provider=gravatar&status=error&q=example")
			var data struct {
				Total int               `json:"total_count"`
				Items []json.RawMessage `json:"items"`
			}
			// Only sender@example.com and orphan@example.com match q=example.
			queryTotal := map[string]int{"": 2, "alice": 1, "bob": 1, "unused": 0}[selected]
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Total != queryTotal || len(data.Items) != map[bool]int{true: 1, false: 0}[queryTotal > 1] {
				t.Fatal(path, selected, response.Code, response.Body.String())
			}
			response = f.request(path + "?user_id=" + selected)
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Total != want {
				t.Fatal(path, selected, response.Code, response.Body.String())
			}
			if selected != "" && (strings.Contains(response.Body.String(), "central@invalid.test") || strings.Contains(response.Body.String(), "orphan@example.com")) {
				t.Fatal("central cache bypassed visibility", response.Body.String())
			}
		}
		response := f.request("/api/admin/avatars/status?user_id=" + selected)
		var status struct {
			Cache struct {
				Total int `json:"total"`
			}
			Recent []json.RawMessage `json:"recent_attempts"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Cache.Total != want || len(status.Recent) != want {
			t.Fatal("status", selected, response.Code, response.Body.String())
		}
		if page := f.request("/admin/avatars/?user_id=" + selected); page.Code != 200 {
			t.Fatal("page", page.Code, page.Body.String())
		}
	}
	if r := f.request("/api/admin/avatars/senders?user_id=administrator"); r.Code != 404 {
		t.Fatal("management owner admitted", r.Code)
	}
	if r := f.request("/api/admin/avatars/senders?user_id=missing"); r.Code != 404 {
		t.Fatal("unknown owner admitted", r.Code)
	}
}

func TestOwnedAdminAvatarReadRejectsExpiredActorAndReleasesStore(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	seedOwnedAdminAvatarCache(t, f)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/avatars/senders?user_id=alice", nil)
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	w := &diagnosticBrowserWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, r) }()
	unlock := sync.OnceFunc(func() { close(w.release) })
	t.Cleanup(func() { unlock(); <-done })
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("browser not reached")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := f.h.userStorage.ReadUserDiagnostics(ctx, storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsAvatars); err != nil {
		t.Fatal("browser retained user store", err)
	}
	unlock()
	<-done
	read, err := f.admin.beginAdminAvatarRead(auth.ContextWithUser(t.Context(), &auth.User{ID: "administrator", AuthVersion: 1}), models.AdminWebmailScope{SelectedUserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer read.close()
	// Revoked sessions must not regain access to central cache diagnostics.
	if _, err := f.system.Write().Exec(`UPDATE users SET auth_version=auth_version+1 WHERE id='administrator'`); err != nil {
		t.Fatal(err)
	}
	if err := read.validate(); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("actor revoked after snapshot still admitted", err)
	}
	if response := f.request("/api/admin/avatars/senders?user_id=alice"); response.Code == 200 {
		t.Fatal("stale session admitted")
	}
}

func TestOwnedAdminAvatarReadMissingStoreAndCanceledRoot(t *testing.T) {
	for _, action := range []string{"missing", "root"} {
		t.Run(action, func(t *testing.T) {
			f := newOwnedAdminDiagnosticFixture(t)
			seedOwnedAdminAvatarCache(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			f.h.userStorageContext = ctx
			if action == "root" {
				cancel()
			} else {
				var path string
				if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
					t.Fatal(err)
				}
				if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsAvatars); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"/api/admin/avatars/senders", "/api/admin/avatars/attempts", "/api/admin/avatars/status"} {
				response := f.request(path + "?user_id=alice")
				if response.Code != 500 {
					t.Fatal("failed snapshot used legacy cache visibility", action, path, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestOwnedAdminProviderAvatarsMatchSharedRepository(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	oracle, err := storage.New(filepath.Join(t.TempDir(), "shared-oracle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := oracle.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := oracle.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice'),('bob','bob','bob')`); err != nil {
		t.Fatal(err)
	}
	// Set up while Bob is active, then inspect his retained photos while disabled.
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	type profile struct {
		owner, id, email, url, remote, updated string
		deleted                                bool
	}
	profiles := []profile{
		{"alice", "direct", "direct@provider-fixture.invalid", "https://photos.example/alice-direct", "", "2020-01-01 00:00:00", false},
		{"alice", "fallback", "fallback@provider-fixture.invalid", "", "people/one", "2023-01-01 00:00:00", false},
		{"alice", "priority", "priority@provider-fixture.invalid", "https://photos.example/alice-priority", "people/two", "2021-01-01 00:00:00", false},
		{"alice", "deleted-target", "deleted@provider-fixture.invalid", "", "people/three", "2023-01-01 00:00:00", false},
		{"bob", "direct", "direct@provider-fixture.invalid", "https://photos.example/bob-direct", "", "2022-01-01 00:00:00", false},
		{"bob", "donor", "donor@fixture.invalid", "https://photos.example/bob-fallback", "people/one", "2024-01-01 00:00:00", false},
		{"bob", "priority-donor", "priority-donor@fixture.invalid", "https://photos.example/newer-fallback", "people/two", "2030-01-01 00:00:00", false},
		{"bob", "deleted-donor", "deleted-donor@fixture.invalid", "https://photos.example/deleted", "people/three", "2040-01-01 00:00:00", true},
	}
	for _, owner := range []string{"alice", "bob"} {
		account := f.accounts[owner].ID
		if _, err := oracle.Write().Exec(`INSERT INTO accounts(id,user_id,email_address,provider,provider_account_id) VALUES(?,?,'mail@example.com','gmail','same-provider-account')`, account, owner); err != nil {
			t.Fatal(err)
		}
		seed := func(db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',provider_account_id='same-provider-account' WHERE id=?`, account); err != nil {
				return err
			}
			for _, p := range profiles {
				if p.owner != owner {
					continue
				}
				id := owner + "-" + p.id
				if _, err := db.Write().Exec(`INSERT INTO contact_profiles(id,user_id,primary_email,avatar_url,updated_at,is_deleted) VALUES(?,?,?,?,?,?)`, id, owner, p.email, p.url, p.updated, p.deleted); err != nil {
					return err
				}
				if _, err := db.Write().Exec(`INSERT INTO contact_identities(user_id,profile_id,kind,normalized_value) VALUES(?,?,'email',?)`, owner, id, p.email); err != nil {
					return err
				}
				if p.remote != "" {
					if _, err := db.Write().Exec(`INSERT INTO contact_cards(id,profile_id,user_id,kind,provider,account_id,remote_id) VALUES(?,?,?,'provider','gmail',?,?)`, id+"-card", id, owner, account, p.remote); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := f.h.withUserDB(t.Context(), owner, seed); err != nil {
			t.Fatal(err)
		}
		if err := seed(oracle); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'; UPDATE contact_profiles SET avatar_url='https://photos.example/central-sentinel'`); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"direct@provider-fixture.invalid", "fallback@provider-fixture.invalid", "priority@provider-fixture.invalid", "deleted@provider-fixture.invalid"} {
		if err := f.system.SaveSenderAvatarMissing(t.Context(), avatarresolver.GravatarHash(email), email, "none", time.Now().Add(time.Hour), "missing", "missing"); err != nil {
			t.Fatal(err)
		}
	}
	check := func(stage string) {
		t.Helper()
		for _, owner := range []string{"", "alice", "bob", "unused"} {
			for _, page := range []string{"", "&limit=2&offset=2"} {
				response := f.request("/api/admin/avatars/senders?user_id=" + owner + "&q=provider-fixture" + page)
				var data struct {
					Items []models.AvatarSenderRow `json:"items"`
					Total int                      `json:"total_count"`
				}
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil {
					t.Fatal(stage, owner, response.Code, response.Body.String())
				}
				wantCount := map[string]int{"": 4, "alice": 4, "bob": 1, "unused": 0}[owner]
				wantItems := wantCount
				if page != "" {
					wantItems = wantCount - 2
					if wantItems < 0 {
						wantItems = 0
					}
				}
				if data.Total != wantCount || len(data.Items) != wantItems {
					t.Fatal("provider page count", stage, owner, page, data.Total, len(data.Items))
				}
				emails := []string{}
				for _, row := range data.Items {
					emails = append(emails, row.Email)
				}
				var expected map[string]string
				if owner == "" {
					expected, err = oracle.GetInstanceProviderContactAvatarsByEmail(t.Context(), emails)
				} else {
					expected, err = oracle.GetProviderContactAvatarsByEmail(t.Context(), owner, emails)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range data.Items {
					want := expected[row.Email]
					if row.InUse.AvatarURL != want || (want != "" && row.InUse.Source != "provider_contact") || (want == "" && row.InUse.Source == "provider_contact") {
						t.Fatal(stage, owner, row.Email, row.InUse, "wanted", want)
					}
				}
			}
		}
	}
	check("matching-provider")
	// Remote contact IDs alone are insufficient: a different provider subject
	// must not supply a fallback photo, while direct-email photos remain valid.
	actor := storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "bob", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='different-provider-account'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := oracle.Write().Exec(`UPDATE accounts SET provider_account_id='different-provider-account' WHERE user_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	check("different-provider")
	if _, err := f.h.userStorage.ReadAdminProviderAvatarContacts(t.Context(), actor, "alice", make([]string, 201)); err == nil {
		t.Fatal("unbounded admin page admitted")
	}
	if _, err := f.h.userStorage.ReadAdminProviderAvatarContacts(t.Context(), storage.DiagnosticsActor{ID: "alice", AuthVersion: 1}, "bob", []string{"direct@provider-fixture.invalid"}); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("private actor admitted", err)
	}
	if _, err := f.h.userStorage.ReadAdminProviderAvatarDonors(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 0}, "bob", []storage.ProviderAvatarIdentity{{Provider: "gmail", Account: "same-provider-account", RemoteID: "people/one"}}); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("stale donor actor admitted", err)
	}
	// A missing donor file is an integrity failure, never permission to use the
	// stale central contact cache or silently discard the provider fallback.
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	var donorPath string
	if err := f.h.withUserDB(t.Context(), "bob", func(db *storage.DB) error { donorPath = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(donorPath); err != nil {
		t.Fatal(err)
	}
	if response := f.request("/api/admin/avatars/senders?user_id=alice&q=provider-fixture"); response.Code != 500 {
		t.Fatal("missing donor file accepted", response.Code, response.Body.String())
	}
	if _, err := os.Stat(donorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("donor store recreated", err)
	}
}
