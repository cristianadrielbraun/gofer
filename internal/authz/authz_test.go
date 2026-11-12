package authz_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCatalogDescribesEveryPermission(t *testing.T) {
	permissions := authz.All()
	if len(permissions) == 0 {
		t.Fatal("empty permission catalog")
	}
	for index, permission := range permissions {
		if _, ok := authz.Lookup(permission); !ok {
			t.Errorf("%s has no description", permission)
		}
		if index > 0 && permissions[index-1] >= permission {
			t.Errorf("All() is not sorted at %s", permission)
		}
	}
	// Reading is open to administrators; every change needs a recent
	// verification, as it did before the catalog existed.
	for permission, stepUp := range map[authz.Permission]bool{
		authz.UsersRead: false, authz.SecurityRead: false, authz.DiagnosticsRead: false,
		authz.UsersInvite: true, authz.UsersAccess: true, authz.UsersCredentials: true,
		authz.UsersMFAPolicy: true, authz.UsersDelete: true, authz.UsersExport: true, authz.InstancePolicy: true,
		authz.SecurityChange: true, authz.ActivityRead: true, authz.ActivityRetention: true, authz.ActivityExport: true, authz.AdminsManage: true,
	} {
		if got := authz.NeedsStepUp(permission); got != stepUp {
			t.Errorf("NeedsStepUp(%s) = %v, want %v", permission, got, stepUp)
		}
	}
	if !authz.NeedsStepUp("users.unknown") {
		t.Error("an unknown permission must fail closed")
	}
}

func TestRequireAllowsOnlyActiveManagementAdministrators(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	for _, person := range []struct {
		id, status, kind string
		admin            int
	}{
		{"head-admin", "active", "management", 1},
		{"resting-admin", "disabled", "management", 1},
		{"reader", "active", "webmail", 0},
	} {
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO users (id, username, username_normalized, name, status, user_type, is_admin, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			person.id, person.id, person.id, person.id, person.status, person.kind, person.admin, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, permission := range authz.All() {
		if err := authz.Require(ctx, db.Read(), "head-admin", permission); err != nil {
			t.Errorf("active management administrator refused %s: %v", permission, err)
		}
		for _, id := range []string{"resting-admin", "reader", "nobody", "", "  "} {
			if err := authz.Require(ctx, db.Read(), id, permission); !errors.Is(err, authz.ErrForbidden) {
				t.Errorf("Require(%q, %s) = %v, want ErrForbidden", id, permission, err)
			}
		}
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, "head-admin", authz.UsersDelete); err != nil {
		t.Fatalf("Require inside a transaction: %v", err)
	}
	if err := authz.Require(ctx, tx, "head-admin", "users.unknown"); err == nil || errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("unknown permission = %v, want a programming error", err)
	}
}
