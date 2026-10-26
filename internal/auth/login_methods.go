package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const loginMethodsThrottleAction = "local_login_methods"

// HasPasskeyLogin reports whether the sign-in page should offer a passkey for
// identifier, after the username step. A username that doesn't exist, isn't
// active, or has no passkey looks the same: only the password is offered.
//
// Every lookup counts against its source, so the page can't be used to list
// who has a passkey any faster than passkey sign-in already allows. A source
// over its limit is offered only the password.
func (m *Manager) HasPasskeyLogin(ctx context.Context, identifier, origin, source string) (bool, error) {
	identifier = normalizeLoginIdentifier(identifier)
	if identifier == "" {
		return false, nil
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return false, fmt.Errorf("validate login methods relying party: %w", err)
	}
	hash, err := m.authenticationThrottleBucketHash(loginMethodsThrottleAction, loginThrottleBucketSource, strings.TrimSpace(source))
	if err != nil {
		return false, err
	}
	bucket := loginThrottleBucket{
		hash: hash, action: loginMethodsThrottleAction, policy: loginThrottlePolicies[loginThrottleBucketSource],
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, []loginThrottleBucket{bucket})
	if err != nil {
		return false, fmt.Errorf("check login methods throttle: %w", err)
	}
	if decision.Throttled {
		return false, nil
	}
	if err := m.runSecurityTransition(ctx, SecurityTransitionLoginThrottle, func(tx *sql.Tx) error {
		_, err := recordLoginThrottleFailure(ctx, tx, bucket, m.clock.Now().UTC())
		return err
	}); err != nil {
		return false, fmt.Errorf("record login methods lookup: %w", err)
	}
	userID, err := m.findPasskeyLoginUser(ctx, identifier, rpID)
	if err != nil {
		return false, err
	}
	return userID != "", nil
}
