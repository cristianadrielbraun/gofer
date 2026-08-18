package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type authenticatorRemoval struct {
	PasskeyID  string
	IdentityID string
	TOTP       bool
}

type federatedLoginAvailability struct {
	Google    bool
	Microsoft bool
}

func (m *Manager) configuredFederatedLoginAvailability() federatedLoginAvailability {
	return federatedLoginAvailability{Google: m.HasGoogleLogin(), Microsoft: m.HasMicrosoftLogin()}
}

// canRemoveAuthenticatorInTransaction evaluates the usable authentication
// methods that remain after one exact factor or identity is removed. Federated
// identities count only while their application-login provider is configured;
// mailbox OAuth accounts are deliberately outside this inventory.
func canRemoveAuthenticatorInTransaction(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	rpID string,
	availability federatedLoginAvailability,
	removal authenticatorRemoval,
) (bool, error) {
	var hasPassword, hasTOTP, passkeyCount, googleIdentityCount, microsoftIdentityCount int
	err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS(SELECT 1 FROM password_credentials p WHERE p.user_id = u.id),
			EXISTS(SELECT 1 FROM totp_credentials t
				WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL),
			(SELECT COUNT(*) FROM webauthn_credentials w
				WHERE w.user_id = u.id AND w.rp_id = ? AND w.revoked_at IS NULL
				  AND w.credential_ciphertext IS NOT NULL AND w.key_version IS NOT NULL
				  AND (? = '' OR w.id != ?)),
			(SELECT COUNT(*) FROM auth_identities identity
				WHERE identity.user_id = u.id AND identity.provider = ? AND identity.issuer = ?
				  AND (? = '' OR identity.id != ?)),
			(SELECT COUNT(*) FROM auth_identities identity
				WHERE identity.user_id = u.id AND identity.provider = ?
				  AND identity.issuer LIKE 'https://login.microsoftonline.com/%/v2.0'
				  AND (? = '' OR identity.id != ?))
		FROM users u
		WHERE u.id = ? AND u.status = 'active'`,
		rpID, removal.PasskeyID, removal.PasskeyID,
		googleIdentityProvider, googleLoginIssuer, removal.IdentityID, removal.IdentityID,
		microsoftIdentityProvider, removal.IdentityID, removal.IdentityID, userID,
	).Scan(&hasPassword, &hasTOTP, &passkeyCount, &googleIdentityCount, &microsoftIdentityCount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrSecuritySessionInvalid
	}
	if err != nil {
		return false, fmt.Errorf("load remaining authenticator inventory: %w", err)
	}
	if removal.TOTP {
		hasTOTP = 0
	}
	if !availability.Google {
		googleIdentityCount = 0
	}
	if !availability.Microsoft {
		microsoftIdentityCount = 0
	}

	policy, err := queryAuthenticationPolicy(ctx, tx, userID, 0)
	if err != nil {
		return false, fmt.Errorf("load authenticator removal policy: %w", err)
	}
	hasPrimary := hasPassword == 1 || passkeyCount > 0 || googleIdentityCount > 0 || microsoftIdentityCount > 0
	hasStrong := hasTOTP == 1 || passkeyCount > 0
	return hasPrimary && (!policy.RequiresMFA || hasStrong), nil
}
