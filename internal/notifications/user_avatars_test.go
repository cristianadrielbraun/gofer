package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserAvatarCentralCacheHTTPVisibilityAndHydration(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	email := "alice-contact@example.com"
	hash := avatarresolver.GravatarHash(email)
	if err := f.system.SaveSenderAvatarFound(t.Context(), hash, email, "gravatar", "image/png", "", []byte("central-image"), time.Now().Add(time.Hour), "found", "missing"); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/avatars/"+hash, "")
	if r.Code != 200 || r.Body.String() != "central-image" || r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("owned cached avatar", r.Code, r.Body.String())
	}
	if r := f.request("bob", "GET", "/api/avatars/"+hash, ""); r.Code != 404 {
		t.Fatal("foreign avatar", r.Code)
	}
	if r := f.request("alice", "GET", "/contacts", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "/api/avatars/"+hash) {
		t.Fatal("owned list failed to hydrate central cache", r.Code)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sender_avatars`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("cache copied into local store")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`DELETE FROM sender_avatars WHERE email_hash=?`, hash); err != nil {
		t.Fatal(err)
	}
	if r := f.request("alice", "GET", "/api/avatars/"+hash, ""); r.Code != 404 {
		t.Fatal("missing central avatar fell back", r.Code)
	}
	if r := f.request("alice", "POST", "/api/avatars/warmup", `{"emails":["nobody@not-visible.invalid"]}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"queued":0`) {
		t.Fatal("invisible warmup", r.Code, r.Body.String())
	}
	// A blocked browser response must leave the single local store available.
	if err := f.system.SaveSenderAvatarFound(t.Context(), hash, email, "gravatar", "image/png", "", []byte("central-image"), time.Now().Add(time.Hour), "found", "missing"); err != nil {
		t.Fatal(err)
	}
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("GET", "/api/avatars/"+hash, nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, req) }()
	t.Cleanup(func() { close(w.release); <-done })
	awaitIMAP(t, w.entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal("browser held avatar store", err)
	}
}
