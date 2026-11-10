package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
)

func TestRequireAdministratorPermissionFollowsTheCatalog(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	ordinarySessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)

	for _, permission := range authz.All() {
		if err := manager.RequireAdministratorPermission(t.Context(), "administrator", adminSessionID, permission); err != nil {
			t.Errorf("verified administrator refused %s: %v", permission, err)
		}
		if err := manager.RequireAdministratorPermission(t.Context(), "ordinary", ordinarySessionID, permission); !errors.Is(err, ErrAdministratorRequired) {
			t.Errorf("ordinary user given %s: %v", permission, err)
		}
	}

	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-securityStepUpMaximumAge-time.Second), adminSessionID,
	); err != nil {
		t.Fatal(err)
	}
	for _, permission := range authz.All() {
		err := manager.RequireAdministratorPermission(t.Context(), "administrator", adminSessionID, permission)
		switch {
		case authz.NeedsStepUp(permission) && !errors.Is(err, ErrRecentStepUpRequired):
			t.Errorf("stale session given %s: %v", permission, err)
		case !authz.NeedsStepUp(permission) && err != nil:
			t.Errorf("stale session refused %s, which needs no verification: %v", permission, err)
		}
	}
	if err := manager.RequireAdministratorPermission(t.Context(), "administrator", "another-session", authz.SecurityChange); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("someone else's session = %v", err)
	}
}
