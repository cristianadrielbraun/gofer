package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// ErrAdministratorUserNotFound means no one has the ID asked about.
var ErrAdministratorUserNotFound = errors.New("administrator user not found")

// AdministratorUserDetail is one person's page in the console: the list's
// summary and how they sign in. It holds sign-in metadata only, never a
// secret, a credential's name or anything from the person's own store.
type AdministratorUserDetail struct {
	AdministratorUserSummary
	DisabledAt        *time.Time
	DeletionStartedAt *time.Time
	// PasswordChangedAt is when the password was last set, when there is one.
	PasswordChangedAt *time.Time
	// Passkeys counts the passkeys they can sign in with.
	Passkeys          int
	PasskeyLastUsedAt *time.Time
	// AuthenticatorAppSince is when the authenticator app was set up, when
	// one is.
	AuthenticatorAppSince      *time.Time
	AuthenticatorAppLastUsedAt *time.Time
	// RecoveryCodesLeft counts the recovery codes not used yet.
	RecoveryCodesLeft int
	Identities        []AdministratorUserIdentity
	// WorkingSessions counts the sessions that still sign them in.
	WorkingSessions int
}

// AdministratorUserIdentity is a sign-in provider account linked to a
// person, with the email address the provider gave when last used.
type AdministratorUserIdentity struct {
	Provider   string
	Email      string
	LinkedAt   time.Time
	LastUsedAt *time.Time
}

// GetAdministratorUser returns one person's page, with invitation action
// references bound to the actor's session as on the list.
func (m *Manager) GetAdministratorUser(ctx context.Context, actorUserID, actorSessionID, userID string) (*AdministratorUserDetail, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, ErrAdministratorUserNotFound
	}
	page, err := m.listAdministratorUsers(ctx, strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID), AdministratorUserQuery{UserIDs: []string{userID}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Users) != 1 {
		return nil, ErrAdministratorUserNotFound
	}
	detail := &AdministratorUserDetail{AdministratorUserSummary: page.Users[0]}

	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin administrator user detail: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, actorUserID, authz.UsersRead); err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	var disabledAt, deletionStartedAt, passwordChangedAt, passkeyLastUsed, appSince, appLastUsed sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT u.disabled_at, u.deletion_started_at,
		       (SELECT p.changed_at FROM password_credentials p WHERE p.user_id = u.id),
		       (SELECT COUNT(*) FROM webauthn_credentials w
		         WHERE w.user_id = u.id AND w.revoked_at IS NULL AND w.credential_ciphertext IS NOT NULL),
		       (SELECT w.last_used_at FROM webauthn_credentials w
		         WHERE w.user_id = u.id AND w.revoked_at IS NULL AND w.credential_ciphertext IS NOT NULL AND w.last_used_at IS NOT NULL
		         ORDER BY w.last_used_at DESC LIMIT 1),
		       (SELECT t.created_at FROM totp_credentials t WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL),
		       (SELECT t.last_used_at FROM totp_credentials t WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM recovery_codes r WHERE r.user_id = u.id AND r.used_at IS NULL AND r.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM sessions s
		         WHERE s.user_id = u.id AND s.revoked_at IS NULL AND s.idle_expires_at > ? AND s.absolute_expires_at > ?)
		FROM users u WHERE u.id = ?`, now, now, userID,
	).Scan(&disabledAt, &deletionStartedAt, &passwordChangedAt, &detail.Passkeys, &passkeyLastUsed,
		&appSince, &appLastUsed, &detail.RecoveryCodesLeft, &detail.WorkingSessions); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAdministratorUserNotFound
		}
		return nil, fmt.Errorf("read administrator user detail: %w", err)
	}
	detail.DisabledAt = nullTimePointer(disabledAt)
	detail.DeletionStartedAt = nullTimePointer(deletionStartedAt)
	if detail.HasPassword {
		detail.PasswordChangedAt = nullTimePointer(passwordChangedAt)
	}
	detail.PasskeyLastUsedAt = nullTimePointer(passkeyLastUsed)
	detail.AuthenticatorAppSince = nullTimePointer(appSince)
	detail.AuthenticatorAppLastUsedAt = nullTimePointer(appLastUsed)

	rows, err := tx.QueryContext(ctx, `
		SELECT provider, email, linked_at, last_used_at FROM auth_identities
		WHERE user_id = ? ORDER BY provider, linked_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list administrator user identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var identity AdministratorUserIdentity
		var lastUsed sql.NullTime
		if err := rows.Scan(&identity.Provider, &identity.Email, &identity.LinkedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("scan administrator user identity: %w", err)
		}
		identity.LastUsedAt = nullTimePointer(lastUsed)
		detail.Identities = append(detail.Identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list administrator user identities: %w", err)
	}
	return detail, nil
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}
