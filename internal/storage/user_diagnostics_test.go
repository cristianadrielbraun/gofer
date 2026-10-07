package storage

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestUserDiagnosticsReadsDisabledOwnerAndRejectsStaleAdministrator(t *testing.T) {
	system, _, routing := newAccountRoutingTest(t, 1)
	for _, owner := range []string{"alice", "bob"} {
		createRoutingTestAccount(t, routing, owner)
		if err := routing.WithUser(t.Context(), owner, func(db *DB) error {
			_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-id", Name: owner, Email: owner + "@test.invalid"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := system.SaveContact(t.Context(), "alice", models.Contact{ID: "central", Name: "central sentinel", Email: "central@test.invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	actor := DiagnosticsActor{ID: "admin", AuthVersion: 1}
	result, err := routing.ReadUserDiagnostics(t.Context(), actor, "alice", UserDiagnosticsContacts)
	if err != nil || result.Contacts.Total != 1 || result.Contacts.Manual != 1 {
		t.Fatal("disabled diagnostics", result.Contacts, err)
	}
	if err := routing.WithUser(t.Context(), "alice", func(*DB) error { t.Error("disabled private route admitted"); return nil }); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal("disabled private route", err)
	}
	for _, denied := range []DiagnosticsActor{{ID: "bob", AuthVersion: 1}, {ID: "admin", AuthVersion: 0}, {ID: "missing", AuthVersion: 1}} {
		if _, err := routing.ReadUserDiagnostics(t.Context(), denied, "alice", UserDiagnosticsContacts); !errors.Is(err, ErrUserDiagnosticsAccess) {
			t.Fatal("unauthorized diagnostics", denied, err)
		}
	}
	if _, err := system.Write().Exec(`UPDATE users SET auth_version=2 WHERE id='admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.ReadUserDiagnostics(t.Context(), actor, "bob", UserDiagnosticsLabels); !errors.Is(err, ErrUserDiagnosticsAccess) {
		t.Fatal("stale administrator admitted", err)
	}
	actor.AuthVersion = 2
	if _, err := routing.ReadUserDiagnostics(t.Context(), actor, "bob", UserDiagnosticsLabels); err != nil {
		t.Fatal("current administrator rejected", err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET deletion_pending=1 WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.ReadUserDiagnostics(t.Context(), actor, "alice", UserDiagnosticsContacts); !errors.Is(err, ErrUserDiagnosticsAccess) {
		t.Fatal("draining owner admitted", err)
	}
}

func TestUserDiagnosticsNeverCreatesMissingStores(t *testing.T) {
	_, stores, routing := newAccountRoutingTest(t, 1)
	actor := DiagnosticsActor{ID: "admin", AuthVersion: 1}
	unused := "../../outside"
	if result, err := routing.ReadUserDiagnostics(t.Context(), actor, unused, UserDiagnosticsContacts); err != nil || result.Contacts.Total != 0 {
		t.Fatal("unused owner", result, err)
	}
	if _, err := os.Stat(stores.userPath(unused)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostics created unused owner", err)
	}
	createRoutingTestAccount(t, routing, "alice")
	lease := acquireUserStore(t, stores, "bob")
	lease.Release()
	if err := os.Remove(stores.userPath("alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.ReadUserDiagnostics(t.Context(), actor, "alice", UserDiagnosticsLabels); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing populated store accepted", err)
	}
	if _, err := os.Stat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing populated store replaced", err)
	}
}

func TestUserDiagnosticsRechecksAdministratorAfterStoreOpen(t *testing.T) {
	system, stores, routing := newAccountRoutingTest(t, 1)
	createRoutingTestAccount(t, routing, "alice")
	lease := acquireUserStore(t, stores, "bob")
	lease.Release()
	entered, release := make(chan struct{}), make(chan struct{})
	unlock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unlock)
	stores.openStore = func(ctx context.Context, owner userStoreOwner, create bool) (*DB, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return stores.openUserStore(ctx, owner, create)
	}
	done := make(chan error, 1)
	go func() {
		_, err := routing.ReadUserDiagnostics(t.Context(), DiagnosticsActor{ID: "admin", AuthVersion: 1}, "alice", UserDiagnosticsMail)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics did not reach store open")
	}
	if _, err := system.Write().Exec(`UPDATE users SET auth_version=auth_version+1 WHERE id='admin'`); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUserDiagnosticsAccess) {
			t.Fatal("stale administrator survived delayed open", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics did not drain")
	}
}
