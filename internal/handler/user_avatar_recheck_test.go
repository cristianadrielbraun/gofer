package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func avatarRecheckRequest(f *ownedAdminDiagnosticFixture, ctx context.Context, email string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/avatars/senders/"+avatarresolver.GravatarHash(email)+"/recheck", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, r)
	return w
}

func awaitAvatarRecheckQueue(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		h.avatarWarmupMu.Lock()
		remaining := len(h.avatarWarmupQueued)
		h.avatarWarmupMu.Unlock()
		if remaining == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("avatar jobs did not finish")
		case <-tick.C:
		}
	}
}

func TestOwnedAvatarSenderRecheckHTTPAndLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "shutdown", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedAdminDiagnosticFixture(t)
			seedOwnedAdminAvatarCache(t, f)
			root, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.h.userStorageContext = root
			f.admin.avatarRouting = f.h.userStorage
			f.admin.startAvatarWarmupWorkers(root)
			if err := f.h.userIMAP.StartBackgroundService(root, func(ctx context.Context) { <-ctx.Done(); f.admin.WaitAvatarWorkers() }); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unlock := sync.OnceFunc(func() { close(release) })
			previous := http.DefaultTransport
			http.DefaultTransport = calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "www.gravatar.com" {
					return nil, fmt.Errorf("unexpected provider %s", r.URL.Host)
				}
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader("forced-avatar")), Request: r}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = previous })
			t.Cleanup(func() { unlock(); cancel(); f.admin.WaitAvatarWorkers() })
			if err := f.h.userStorage.RecordUserAvatarInterests(t.Context(), "alice", []string{"sender@example.com"}); err != nil {
				t.Fatal(err)
			}
			requestCtx, requestCancel := context.WithCancel(t.Context())
			defer requestCancel()
			w := avatarRecheckRequest(f, requestCtx, "sender@example.com")
			if w.Code != 200 || w.Body.String() != "{\"started\":true}\n" {
				t.Fatal(w.Code, w.Body.String())
			}
			requestCancel() // Accepted work belongs to the runtime, not the browser.
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				t.Fatal("no provider request")
			}
			w = avatarRecheckRequest(f, t.Context(), "sender@example.com")
			if w.Code != http.StatusConflict || w.Body.String() != "{\"started\":false}\n" {
				t.Fatal("duplicate", w.Code, w.Body.String())
			}
			ctx, closeCtx := context.WithTimeout(t.Context(), 30*time.Second)
			defer closeCtx()
			if _, err := f.h.userStorage.ReadUserDiagnostics(ctx, storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsAvatars); err != nil {
				t.Fatal("provider retained owner store", err)
			}
			if mode == "revoked" {
				if _, err := f.system.Write().Exec(`UPDATE users SET auth_version=auth_version+1 WHERE id='administrator'`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "shutdown" {
				cancel()
				f.cancel()
				done := make(chan struct{})
				go func() { defer close(done); f.h.userIMAP.Wait() }()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("runtime did not join avatar provider")
				}
			} else {
				unlock()
				awaitAvatarRecheckQueue(t, f.admin)
			}
			rec, err := f.system.GetSenderAvatarByHash(t.Context(), avatarresolver.GravatarHash("sender@example.com"))
			if err != nil || rec == nil {
				t.Fatal(rec, err)
			}
			if mode == "success" && rec.Status != "found" {
				t.Fatal("force failed to replace future retry record", rec.Status)
			}
			if mode != "success" && rec.Status != "error" {
				t.Fatal("stale/canceled result published", rec.Status)
			}
			f.admin.WaitAvatarWorkers()
			if w = avatarRecheckRequest(f, t.Context(), "orphan@example.com"); w.Code == 200 {
				t.Fatal("closed workers admitted recheck")
			}
		})
	}
}

func TestOwnedAvatarSenderRecheckRejectsQueuedRevokedActor(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	seedOwnedAdminAvatarCache(t, f)
	f.admin.startAvatarWarmupWorkers(t.Context())
	entered := make(chan string, 3)
	release := make(chan struct{})
	unlock := sync.OnceFunc(func() { close(release) })
	previous := http.DefaultTransport
	http.DefaultTransport = calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		entered <- r.URL.Path
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader("revoked-avatar")), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	t.Cleanup(func() { unlock(); f.admin.WaitAvatarWorkers() })
	for _, email := range []string{"sender@example.com", "orphan@example.com"} {
		if w := avatarRecheckRequest(f, t.Context(), email); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for i := 0; i < avatarWarmupWorkers; i++ {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("workers not occupied")
		}
	}
	if w := avatarRecheckRequest(f, t.Context(), "central@invalid.test"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET auth_version=auth_version+1 WHERE id='administrator'`); err != nil {
		t.Fatal(err)
	}
	unlock()
	awaitAvatarRecheckQueue(t, f.admin)
	select {
	case path := <-entered:
		t.Fatal("revoked queued work reached provider", path)
	default:
	}
	for _, email := range []string{"sender@example.com", "orphan@example.com", "central@invalid.test"} {
		rec, err := f.system.GetSenderAvatarByHash(t.Context(), avatarresolver.GravatarHash(email))
		if err != nil || rec == nil || rec.Status != "error" {
			t.Fatal(email, rec, err)
		}
	}
	if w := avatarRecheckRequest(f, t.Context(), "alice@test.invalid"); w.Code == 200 {
		t.Fatal("stale session admitted")
	}
	r := httptest.NewRequest(http.MethodPost, "/ignored", nil)
	r.SetPathValue("hash", avatarresolver.GravatarHash("alice@test.invalid"))
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "administrator", AuthVersion: 1}))
	w := httptest.NewRecorder()
	f.admin.handleRecheckAvatarSender(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("stale captured administrator", w.Code)
	}
}

func TestOwnedAvatarSenderRecheckQueueSaturationAndDrain(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.h.userStorageContext = root
	f.admin.startAvatarWarmupWorkers(root)
	entered := make(chan struct{}, avatarWarmupWorkers+1)
	previous := http.DefaultTransport
	http.DefaultTransport = calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	t.Cleanup(func() { cancel(); f.admin.WaitAvatarWorkers() })
	submit := func(n int) *httptest.ResponseRecorder {
		email := fmt.Sprintf("queued%d@test.invalid", n)
		if err := f.system.UpsertSenderAvatarCandidate(t.Context(), email); err != nil {
			t.Fatal(err)
		}
		return avatarRecheckRequest(f, t.Context(), email)
	}
	for i := 0; i < avatarWarmupWorkers; i++ {
		if w := submit(i); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for i := 0; i < avatarWarmupWorkers; i++ {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("workers not occupied")
		}
	}
	for i := 0; i < avatarWarmupQueueSize; i++ {
		if w := submit(i + avatarWarmupWorkers); w.Code != 200 {
			t.Fatal("queue admitted fewer jobs than capacity", i, w.Code, w.Body.String())
		}
	}
	if w := submit(avatarWarmupQueueSize + avatarWarmupWorkers); w.Code != http.StatusConflict {
		t.Fatal("saturated queue spawned work", w.Code)
	}
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.admin.WaitAvatarWorkers() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not drain queued jobs")
	}
	if len(f.admin.avatarWarmupQueue) != 0 || len(f.admin.avatarWarmupQueued) != 0 {
		t.Fatal("closed worker retained queued authority")
	}
	select {
	case <-entered:
		t.Fatal("queued job reached provider during shutdown")
	default:
	}
}
