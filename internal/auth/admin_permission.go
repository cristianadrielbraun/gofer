package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// requireAdministratorPermission checks, inside the change's transaction,
// that the actor may use permission (internal/authz decides), and that
// their session verified recently when the permission needs it.
func requireAdministratorPermission(
	ctx context.Context, tx *sql.Tx, actorUserID, actorSessionID string, permission authz.Permission, now time.Time,
) error {
	if err := authz.Require(ctx, tx, actorUserID, permission); err != nil {
		return err
	}
	if !authz.NeedsStepUp(permission) {
		return nil
	}
	return requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now)
}

// RequireAdministratorPermission checks that the actor may use permission
// now, for administrative changes made outside auth's own transactions,
// such as the mail security settings.
func (m *Manager) RequireAdministratorPermission(ctx context.Context, actorUserID, actorSessionID string, permission authz.Permission) error {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin administrator permission check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return requireAdministratorPermission(
		ctx, tx, strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID), permission, m.clock.Now().UTC(),
	)
}

// enrollmentTokenPermission is what issuing or revoking a token of purpose
// takes: invitations are part of inviting people, reset tokens part of
// managing their credentials.
func enrollmentTokenPermission(purpose EnrollmentTokenPurpose) authz.Permission {
	if purpose == EnrollmentTokenPurposeEnrollment {
		return authz.UsersInvite
	}
	return authz.UsersCredentials
}
