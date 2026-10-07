package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
