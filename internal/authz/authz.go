// Package authz decides what an administrator may do. Every check that an
// administrative change makes goes through here, so delegated
// administration only has to change this package: today each permission
// belongs to the active management administrators, as it always has.
//
// It reads the central tables with plain SQL and imports nothing of Gofer's,
// so auth can call it inside its own transactions.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Permission names one thing an administrator may do.
type Permission string

const (
	// UsersRead lists people and their sign-in state.
	UsersRead Permission = "users.read"
	// UsersInvite creates, rotates and revokes invitations.
	UsersInvite Permission = "users.invite"
	// UsersAccess enables and disables people.
	UsersAccess Permission = "users.access"
	// UsersCredentials issues password-reset tokens and requires password
	// changes.
	UsersCredentials Permission = "users.credentials"
	// UsersMFAPolicy requires or clears two-step sign-in for one person.
	UsersMFAPolicy Permission = "users.mfa_policy"
	// UsersDelete deletes people and their local data.
	UsersDelete Permission = "users.delete"
	// InstancePolicy changes instance-wide sign-in rules.
	InstancePolicy Permission = "instance.policy"
	// SecurityRead shows mail security settings.
	SecurityRead Permission = "security.read"
	// SecurityChange changes mail security: exceptions, proxies and the
	// allowed mail lists.
	SecurityChange Permission = "security.change"
	// ActivityRead shows sign-in and security activity.
	ActivityRead Permission = "activity.read"
	// ActivityRetention sets how long activity is kept.
	ActivityRetention Permission = "activity.retention"
	// DiagnosticsRead shows the system diagnostics pages.
	DiagnosticsRead Permission = "diagnostics.read"
)

// Spec describes a permission.
type Spec struct {
	// StepUp says using the permission needs a session that verified
	// recently with a passkey or authenticator app.
	StepUp bool
}

var catalog = map[Permission]Spec{
	UsersRead:         {},
	UsersInvite:       {StepUp: true},
	UsersAccess:       {StepUp: true},
	UsersCredentials:  {StepUp: true},
	UsersMFAPolicy:    {StepUp: true},
	UsersDelete:       {StepUp: true},
	InstancePolicy:    {StepUp: true},
	SecurityRead:      {},
	SecurityChange:    {StepUp: true},
	ActivityRead:      {StepUp: true},
	ActivityRetention: {StepUp: true},
	DiagnosticsRead:   {},
}

// ErrForbidden means the person may not use the permission.
var ErrForbidden = errors.New("active administrator required")

// Lookup returns permission's description, and whether it exists.
func Lookup(permission Permission) (Spec, bool) {
	spec, ok := catalog[permission]
	return spec, ok
}

// NeedsStepUp reports whether permission needs a recently verified
// session. Unknown permissions do, so a mistake fails closed.
func NeedsStepUp(permission Permission) bool {
	spec, ok := catalog[permission]
	return !ok || spec.StepUp
}

// All lists every permission, sorted.
func All() []Permission {
	permissions := make([]Permission, 0, len(catalog))
	for permission := range catalog {
		permissions = append(permissions, permission)
	}
	sort.Slice(permissions, func(i, j int) bool { return permissions[i] < permissions[j] })
	return permissions
}

// Querier is what Require reads with: a transaction or a database.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Require checks that the person with userID may use permission. Inside a
// transaction, pass the transaction, so the answer holds for the change it
// guards. Until delegated administration, every permission belongs to the
// active management administrators.
func Require(ctx context.Context, q Querier, userID string, permission Permission) error {
	if _, ok := catalog[permission]; !ok {
		return fmt.Errorf("authz: unknown permission %q", permission)
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrForbidden
	}
	var allowed int
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM users
			WHERE id = ? AND status = 'active' AND user_type = 'management' AND is_admin = 1
		)`, userID).Scan(&allowed); err != nil {
		return fmt.Errorf("authz: check %s for administrator: %w", permission, err)
	}
	if allowed != 1 {
		return ErrForbidden
	}
	return nil
}
