package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// The settings policies decide. Each is declared once, here; the console,
// the resolver and the audit log all read these declarations.

// MFARequired requires two-factor sign-in. Administrators need it
// whatever this says. The Default policy's value is the instance's
// "require two-factor sign-in of everyone" (auth_system_state.mfa_policy),
// which no other policy can turn off for anyone.
var MFARequired = Bool(Spec{
	Key: "auth.mfa_required", Category: CategorySignIn, Kind: KindSecurity,
	Label: "admin.policy.setting.mfa_required", Help: "admin.policy.setting.mfa_required.help",
}, false).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var policy string
	err := q.QueryRowContext(ctx, `SELECT mfa_policy FROM auth_system_state WHERE id = 1`).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read the instance two-factor policy: %w", err)
	}
	everyone := policy == "all_users"
	return json.RawMessage(strconv.FormatBool(everyone)), everyone, true, nil
})

// SecurityRetentionDays is how long sign-in and security events are kept
// (auth_system_state.auth_event_retention_days).
var SecurityRetentionDays = Int(Spec{
	Key: "audit.security_retention_days", Category: CategoryActivity, Kind: KindSecurity, InstanceOnly: true,
	Label: "admin.policy.setting.security_retention_days", Help: "admin.policy.setting.security_retention_days.help",
}, 180, 1, 365).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var days int
	err := q.QueryRowContext(ctx, `SELECT auth_event_retention_days FROM auth_system_state WHERE id = 1`).Scan(&days)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read the security event retention: %w", err)
	}
	return json.RawMessage(strconv.Itoa(days)), false, true, nil
})

// OwnProxies lets people add proxies of their own for their mailboxes
// (outbound_proxy_policy.people_add).
var OwnProxies = Bool(Spec{
	Key: "mail.own_proxies", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.own_proxies", Help: "admin.policy.setting.own_proxies.help",
}, false).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT people_add = 1 FROM outbound_proxy_policy WHERE singleton = 1`).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read whether people may add proxies: %w", err)
	}
	return json.RawMessage(strconv.FormatBool(allowed)), false, true, nil
})
