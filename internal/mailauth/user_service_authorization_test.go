package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarAuthorizationMatchesPurposeOwnerAndAccount(t *testing.T) {
	f := newUserCredentialFixture(t, nil)
	f.grant(t, "alice", "cached", "refresh", false, GoogleCalendarReadOnlyScope+" "+GoogleCalendarEventsScope)
	read, err := f.service.CalendarAccount("alice", f.ids["alice"], false).ServiceAuthorization(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	write, err := f.service.CalendarAccount("alice", f.ids["alice"], true).ServiceAuthorization(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		authority *UserServiceAuthorization
		owner, id string
		write, ok bool
	}{
		{read, "alice", f.ids["alice"], false, true},
		{write, "alice", f.ids["alice"], true, true},
		{read, "alice", f.ids["alice"], true, false},
		{write, "alice", f.ids["alice"], false, false},
		{write, "bob", f.ids["alice"], true, false},
		{write, "alice", f.ids["bob"], true, false},
		{nil, "alice", f.ids["alice"], true, false},
	} {
		err := f.service.ValidateCalendarAuthorization(t.Context(), tc.authority, tc.owner, tc.id, tc.write)
		if (err == nil) != tc.ok || (!tc.ok && !errors.Is(err, ErrMailboxAuthorizationChanged)) {
			t.Fatal("calendar binding accepted wrong purpose/identity", tc, err)
		}
	}
}

func TestUserServiceAuthorizationRefreshRejectsReconnectDuringCredentialGateWait(t *testing.T) {
	for _, ensure := range []bool{false, true} {
		t.Run(fmt.Sprint("ensure=", ensure), func(t *testing.T) {
			var calls atomic.Int32
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				t.Error("old operation reached token endpoint after reconnect")
				http.Error(w, "unexpected refresh", 500)
			}))
			f.grant(t, "alice", "cached", "refresh", false, GoogleCalendarReadOnlyScope)
			first, err := f.service.CalendarAccount("alice", f.ids["alice"], false).ServiceAuthorization(t.Context(), false)
			if ensure {
				first, err = f.service.SnapshotCalendarAuthorization(t.Context(), "alice", f.ids["alice"], "gmail", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			gateDone := make(chan error, 1)
			go func() {
				gateDone <- f.service.operation(t.Context(), "alice", f.ids["alice"], true, func(ctx context.Context) error {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			awaitSignal(t, entered)
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			done := make(chan error, 1)
			go func() {
				var err error
				if ensure {
					_, err = f.service.EnsureServiceAuthorization(t.Context(), first)
				} else {
					_, err = f.service.RefreshServiceAuthorization(t.Context(), first)
				}
				done <- err
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				f.service.mu.Lock()
				gate := f.service.gates[f.ids["alice"]]
				waiting := gate != nil && gate.refs == 2
				f.service.mu.Unlock()
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("refresh did not wait for gate")
				case <-tick.C:
				}
			}
			// Supplying a new refresh token models explicit reconnect; it deliberately
			// does not wait behind an old refresh request to replace central authority.
			f.grant(t, "alice", "reconnected", "new-refresh", false, GoogleCalendarReadOnlyScope)
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, ErrMailboxAuthorizationChanged) {
					t.Fatal("gate-wait reconnect accepted", err)
				}
			case <-ctx.Done():
				t.Fatal("refresh did not finish")
			}
			if err := <-gateDone; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("refresh dispatched against new grant")
			}
		})
	}
}

func TestUserServiceAuthorizationRevisionBindingAndCentralOnlyValidation(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-service", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 3600, "scope": GoogleCalendarReadOnlyScope})
	}))
	f.grant(t, "alice", "cached-service", "alice-refresh", false, GoogleCalendarReadOnlyScope)
	bound := f.service.CalendarAccount("alice", f.ids["alice"], false)
	first, err := bound.ServiceAuthorization(t.Context(), false)
	if err != nil || first.Token() != "cached-service" {
		t.Fatal("cached authority", err)
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(_ *storage.DB) error { return f.service.ValidateServiceAuthorization(t.Context(), first) }); err != nil {
		t.Fatal("validation acquired another local store while sole slot pinned", err)
	}
	next, err := f.service.RefreshServiceAuthorization(t.Context(), first)
	if err != nil || next.Token() != "fresh-service" || calls.Load() != 1 {
		t.Fatal("refresh authority", calls.Load(), err)
	}
	if err := f.service.ValidateServiceAuthorization(t.Context(), first); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("old token revision accepted", err)
	}
	if err := f.service.ValidateServiceAuthorization(t.Context(), next); err != nil {
		t.Fatal("own refresh invalidated advanced authority", err)
	}
	f.grant(t, "alice", "reconnected", "new-refresh", false, GoogleCalendarReadOnlyScope)
	if err := f.service.ValidateServiceAuthorization(t.Context(), next); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("same-subject reconnect kept old authority", err)
	}
	for _, value := range []*UserServiceAuthorization{nil, {}} {
		if err := f.service.ValidateServiceAuthorization(t.Context(), value); !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("nil/zero authorization accepted", err)
		}
	}
	other := &UserCredentials{routing: f.routing, ctx: t.Context()}
	if err := other.ValidateServiceAuthorization(t.Context(), next); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("foreign authority repository accepted", err)
	}
	if _, err := f.service.Account("alice", f.ids["alice"]).ServiceAuthorization(t.Context(), false); err == nil {
		t.Fatal("mailbox facade authorized as a calendar service")
	}
	if _, err := f.service.CalendarAccount("bob", f.ids["alice"], false).ServiceAuthorization(t.Context(), false); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign account authority", err)
	}
}

func TestUserCalendarGrantSnapshotHasNoTokenOrCredentialGateWait(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "local snapshot must not refresh", 500)
	}))
	f.grant(t, "alice", "expired", "refresh", true, GoogleCalendarReadOnlyScope+" "+GoogleCalendarEventsScope)
	f.grant(t, "outlook", "expired-graph", "refresh", true, microsoftGraphCalendarWriteScope)
	entered, release := make(chan struct{}), make(chan struct{})
	gateDone := make(chan error, 1)
	go func() {
		gateDone <- f.service.operation(t.Context(), "alice", f.ids["alice"], true, func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	awaitSignal(t, entered)
	defer func() {
		close(release)
		if err := <-gateDone; err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		key, provider string
		write         bool
	}{
		{"alice", "gmail", false}, {"alice", "gmail", true},
		{"outlook", "outlook", false}, {"outlook", "outlook", true},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		proof, err := f.service.SnapshotCalendarAuthorization(ctx, "alice", f.ids[tc.key], tc.provider, tc.write)
		cancel()
		if err != nil || proof.Token() != "" {
			t.Fatal("local proof waited/refreshed/exposed a token", tc, err)
		}
		if err := f.service.ValidateCalendarAuthorization(t.Context(), proof, "alice", f.ids[tc.key], tc.write); err != nil {
			t.Fatal("local proof invalid", err)
		}
		if err := f.service.ValidateCalendarAuthorization(t.Context(), proof, "alice", f.ids[tc.key], !tc.write); !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("local proof authorized a different purpose", err)
		}
		copy := *proof
		copy.grantOnly = false
		if err := f.service.ValidateServiceAuthorization(t.Context(), &copy); !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("unmarked empty-token authority accepted", err)
		}
		copy = *proof
		copy.token = "injected"
		if err := f.service.ValidateServiceAuthorization(t.Context(), &copy); !errors.Is(err, ErrMailboxAuthorizationChanged) {
			t.Fatal("local proof accepted injected token", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("snapshot contacted token endpoint", calls.Load())
	}
}

func TestUserCalendarGrantSnapshotRejectsWrongIdentityScopeAndShutdown(t *testing.T) {
	f := newUserCredentialFixture(t, nil)
	f.grant(t, "alice", "cached", "refresh", false, GoogleCalendarReadOnlyScope)
	f.grant(t, "outlook", "cached", "refresh", false, microsoftGraphCalendarScope)
	for _, tc := range []struct {
		owner, id, provider string
		write               bool
	}{
		{"bob", f.ids["alice"], "gmail", false},
		{"alice", f.ids["alice"], "outlook", false},
		{"alice", f.ids["alice"], "caldav", false},
		{"alice", f.ids["alice"], "gmail", true},
		{"alice", f.ids["outlook"], "outlook", true},
	} {
		if _, err := f.service.SnapshotCalendarAuthorization(t.Context(), tc.owner, tc.id, tc.provider, tc.write); err == nil {
			t.Fatal("wrong grant accepted", tc)
		}
	}
	proof, err := f.service.SnapshotCalendarAuthorization(t.Context(), "alice", f.ids["alice"], "gmail", false)
	if err != nil {
		t.Fatal(err)
	}
	f.grant(t, "alice", "new", "new-refresh", false, GoogleCalendarReadOnlyScope)
	if err := f.service.ValidateServiceAuthorization(t.Context(), proof); !errors.Is(err, ErrMailboxAuthorizationChanged) {
		t.Fatal("reconnect retained local proof", err)
	}
	f.cancel()
	if _, err := f.service.SnapshotCalendarAuthorization(t.Context(), "alice", f.ids["alice"], "gmail", false); err == nil {
		t.Fatal("local proof authorized after shutdown")
	}
}

func TestUserCalendarGrantEnsureUsesCachedTokenOrScopedRefresh(t *testing.T) {
	for _, tc := range []struct {
		key, provider, granted, cached string
		expired                        bool
		refreshes                      int32
	}{
		{"alice", "gmail", GoogleCalendarEventsScope, GoogleCalendarEventsScope, false, 0},
		{"alice", "gmail", GoogleCalendarEventsScope, GoogleCalendarEventsScope, true, 1},
		{"outlook", "outlook", microsoftGraphCalendarWriteScope, microsoftGraphCalendarWriteScope, false, 0},
		{"outlook", "outlook", microsoftGraphCalendarWriteScope, microsoftGraphCalendarScope, false, 1},
	} {
		t.Run(tc.key+tc.cached+fmt.Sprint(tc.expired), func(t *testing.T) {
			var calls atomic.Int32
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil || r.PostForm.Get("refresh_token") != "refresh" {
					t.Error("wrong refresh identity", err)
				}
				if tc.key == "outlook" && r.PostForm.Get("scope") != microsoftGraphCalendarWriteScope {
					t.Error("refresh was not scoped to Calendar write", r.PostForm.Get("scope"))
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "token_type": "Bearer", "expires_in": 3600, "scope": tc.granted})
			}))
			f.grant(t, tc.key, "cached", "refresh", tc.expired, tc.granted)
			if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET scopes=? WHERE account_id=?`, tc.cached, f.ids[tc.key]); err != nil {
				t.Fatal(err)
			}
			proof, err := f.service.SnapshotCalendarAuthorization(t.Context(), "alice", f.ids[tc.key], tc.provider, true)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := f.service.EnsureServiceAuthorization(t.Context(), proof)
			if err != nil || authority.Token() == "" || authority.grantOnly || calls.Load() != tc.refreshes {
				t.Fatal("ensure token", calls.Load(), err)
			}
			if tc.refreshes == 0 && authority.Token() != "cached" || tc.refreshes == 1 && authority.Token() != "fresh" {
				t.Fatal("wrong token", authority.Token())
			}
			if err := f.service.ValidateCalendarAuthorization(t.Context(), authority, "alice", f.ids[tc.key], true); err != nil {
				t.Fatal("ensured authority invalid", err)
			}
			if tc.refreshes == 1 {
				if err := f.service.ValidateServiceAuthorization(t.Context(), proof); !errors.Is(err, ErrMailboxAuthorizationChanged) {
					t.Fatal("old grant snapshot survived refresh", err)
				}
			}
		})
	}
}
