package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserMailboxAuthorizationPurposeAndOwnRefresh(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600, "token_type": "Bearer", "scope": "https://mail.google.com/ " + GoogleCalendarReadOnlyScope})
	}))
	f.grant(t, "alice", "cached", "refresh", false, "https://mail.google.com/ "+GoogleCalendarReadOnlyScope)
	a, err := f.service.MailboxAuthorization(t.Context(), "alice", f.ids["alice"])
	if err != nil || a.Token() != "cached" {
		t.Fatal(a, err)
	}
	calendar, err := f.service.CalendarAccount("alice", f.ids["alice"], false).ServiceAuthorization(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ValidateMailboxAuthorization(t.Context(), calendar, "alice", f.ids["alice"]); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("calendar grant used as mailbox", err)
	}
	if err := f.service.ValidateCalendarAuthorization(t.Context(), a, "alice", f.ids["alice"], false); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("mail grant used as calendar", err)
	}
	for _, bad := range []*UserServiceAuthorization{nil, {}, calendar} {
		if _, err := f.service.RefreshMailboxAuthorization(t.Context(), bad); !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("wrong purpose refreshed", err)
		}
	}
	if _, err := f.service.RefreshServiceAuthorization(t.Context(), a); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("mail grant refreshed as another service", err)
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(*storage.DB) error {
		return f.service.ValidateMailboxAuthorization(t.Context(), a, "alice", f.ids["alice"])
	}); err != nil {
		t.Fatal("validation retained local lease", err)
	}
	next, err := f.service.RefreshMailboxAuthorization(t.Context(), a)
	if err != nil || next.Token() != "fresh" || calls.Load() != 1 {
		t.Fatal("own refresh", calls.Load(), err)
	}
	if err := f.service.ValidateMailboxAuthorization(t.Context(), a, "alice", f.ids["alice"]); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("old revision current", err)
	}
	if err := f.service.ValidateMailboxAuthorization(t.Context(), next, "alice", f.ids["alice"]); err != nil {
		t.Fatal("fresh revision invalid", err)
	}
}

func TestUserMailboxAuthorizationRefreshRejectsReconnectAfterGateWait(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) }))
	f.grant(t, "alice", "cached", "refresh", false, "https://mail.google.com/")
	a, err := f.service.MailboxAuthorization(t.Context(), "alice", f.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer unblock()
	gateDone := make(chan error, 1)
	go func() {
		gateDone <- f.service.operation(t.Context(), "alice", f.ids["alice"], true, func(context.Context) error { close(entered); <-release; return nil })
	}()
	awaitSignal(t, entered)
	done := make(chan error, 1)
	go func() { _, err := f.service.RefreshMailboxAuthorization(t.Context(), a); done <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		f.service.mu.Lock()
		g := f.service.gates[f.ids["alice"]]
		waiting := g != nil && g.refs == 2
		f.service.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("refresh did not wait")
		case <-time.After(time.Millisecond):
		}
	}
	f.grant(t, "alice", "new-access", "new-refresh", false, "https://mail.google.com/")
	unblock()
	if err := <-gateDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("late refresh adopted reconnect", err)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	if calls.Load() != 0 {
		t.Fatal("token HTTP reached after reconnect")
	}
}
