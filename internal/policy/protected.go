package policy

import (
	"context"
	"fmt"
	"strings"
)

// A group is protected when a policy assigned to it sets a security
// setting: who is in it then decides how they sign in, or what their mail
// may reach. It's worked out from the assignments and values each time,
// so nothing has to be kept in step when either changes.

// SetsSecurity reports whether values set any security setting.
func SetsSecurity(values []Value) bool {
	for _, value := range values {
		if setting, ok := Lookup(value.Key); ok && setting.Kind == KindSecurity && !setting.InstanceOnly {
			return true
		}
	}
	return false
}

// securityKeys are the declared security settings a policy for some
// people can set.
func securityKeys() []any {
	var keys []any
	for _, setting := range Settings() {
		if setting.Kind == KindSecurity && !setting.InstanceOnly {
			keys = append(keys, setting.Key)
		}
	}
	return keys
}

// ProtectedGroups returns the IDs of the protected groups.
func ProtectedGroups(ctx context.Context, q Querier) (map[string]bool, error) {
	protected := map[string]bool{}
	keys := securityKeys()
	if len(keys) == 0 {
		return protected, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT a.target_id
		FROM policy_assignments a
		JOIN policy_values v ON v.policy_id = a.policy_id
		WHERE a.target_kind = 'group' AND v.setting_key IN (?`+strings.Repeat(", ?", len(keys)-1)+`)`, keys...)
	if err != nil {
		return nil, fmt.Errorf("find the protected groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read a protected group: %w", err)
		}
		protected[id] = true
	}
	return protected, rows.Err()
}
