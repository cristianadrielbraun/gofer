package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestResolveAvatarWithRetryRetriesOnce(t *testing.T) {
	attempts := 0
	image, found, err := resolveAvatarWithRetry(context.Background(), func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		if attempts == 1 {
			return avatarresolver.Image{}, false, errors.New("temporary failure")
		}
		return avatarresolver.Image{Source: "gravatar"}, true, nil
	})
	if err != nil {
		t.Fatalf("resolveAvatarWithRetry() error = %v", err)
	}
	if !found || image.Source != "gravatar" {
		t.Fatalf("resolveAvatarWithRetry() = (%+v, %v), want found gravatar", image, found)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestResolveAvatarWithRetryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, _, err := resolveAvatarWithRetry(ctx, func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		cancel()
		return avatarresolver.Image{}, false, errors.New("temporary failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveAvatarWithRetry() error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestAvatarProviderSpecsCheckLibravatarBeforeBIMI(t *testing.T) {
	providers := avatarProviderSpecs()
	if len(providers) < 3 {
		t.Fatalf("provider count = %d, want at least 3", len(providers))
	}
	want := []string{"gravatar", "libravatar", "bimi"}
	for i, name := range want {
		if providers[i].name != name {
			t.Fatalf("provider[%d] = %q, want %q", i, providers[i].name, name)
		}
	}
}

func TestLibravatarAttemptMessages(t *testing.T) {
	hash := "0bc83cb571cd1c50ba6f3e8a78ef1346"
	if got := libravatarMissingAttemptMessage(hash); !strings.Contains(got, "seccdn.libravatar.org") || !strings.Contains(got, "no Libravatar image") {
		t.Fatalf("libravatarMissingAttemptMessage() = %q", got)
	}
	if got := libravatarErrorAttemptMessage(hash, errors.New("boom")); !strings.Contains(got, "seccdn.libravatar.org") || !strings.Contains(got, "boom") {
		t.Fatalf("libravatarErrorAttemptMessage() = %q", got)
	}
}

func TestHandleAvatarImageAddsStrictHeadersForSVG(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	defer db.Close()

	blobs := store.NewBlobStore(filepath.Join(dir, "blobs"))
	h := &Handler{db: db, blobStore: blobs}
	email := "brand@example.com"
	hash := avatarresolver.GravatarHash(email)
	data := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1v1z"/></svg>`)
	storagePath, err := blobs.StoreAvatar(hash, "image/svg+xml", data)
	if err != nil {
		t.Fatalf("StoreAvatar() error = %v", err)
	}
	if err := db.SaveSenderAvatarFound(ctx, hash, email, "bimi", "image/svg+xml", storagePath, nil, time.Now().Add(time.Hour), "missing", "found"); err != nil {
		t.Fatalf("SaveSenderAvatarFound() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/avatars/"+hash, nil)
	req.SetPathValue("hash", hash)
	rec := httptest.NewRecorder()
	h.handleAvatarImage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'none'") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q, want strict SVG policy", csp)
	}
}
