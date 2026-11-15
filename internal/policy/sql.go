package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Some checks need one on/off setting's value for many people at once, or
// for one person inside a query: sign-in, and the Users page's filters.
// These queries take, for each person a named policy reaches, the value of
// the first one in priority order, directly or through their groups, that
// sets the setting to a valid value: the resolver's rule (resolve) for that
// setting, without the Default policy, which the caller combines.

// namedValuesSQL selects (user_id, value_json, policy_id, policy_name) for
// the people the first named policy that sets setting reaches, and that
// policy; forUser keeps only the person in the query's two ? arguments.
func namedValuesSQL(setting *Setting, valid []string, forUser bool) string {
	if !validKey(setting.Key) {
		panic(fmt.Sprintf("policy: %q can't be put in a query", setting.Key))
	}
	direct, grouped := "", ""
	if forUser {
		direct, grouped = " AND a.target_id = ?", " AND m.user_id = ?"
	}
	quoted := make([]string, len(valid))
	for i, value := range valid {
		quoted[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return `SELECT ranked.user_id, ranked.value_json, ranked.policy_id, ranked.policy_name FROM (
		SELECT reach.user_id, v.value_json, p.id AS policy_id, p.name AS policy_name,
		       ROW_NUMBER() OVER (PARTITION BY reach.user_id ORDER BY p.priority) AS place
		FROM (
			SELECT a.policy_id, a.target_id AS user_id FROM policy_assignments a WHERE a.target_kind = 'user'` + direct + `
			UNION
			SELECT a.policy_id, m.user_id FROM policy_assignments a
			JOIN directory_group_members m ON a.target_kind = 'group' AND m.group_id = a.target_id` + grouped + `
		) reach
		JOIN policies p ON p.id = reach.policy_id AND p.is_default = 0
		JOIN policy_values v ON v.policy_id = p.id AND v.setting_key = '` + setting.Key + `'
		  AND v.value_json IN (` + strings.Join(quoted, ", ") + `)
	) ranked WHERE ranked.place = 1`
}

// Two-factor sign-in: the policy that decides MFARequired for someone also
// says whether a passkey is enough (MFAPasskeyCounts).
func twoFactorSQL(forUser bool) string {
	return `SELECT named.user_id, named.policy_id, named.policy_name,
		COALESCE(passkey.value_json = 'true', 0) AS passkey_counts
		FROM (` + namedValuesSQL(MFARequired, []string{"true", "false"}, forUser) + `) named
		LEFT JOIN policy_values passkey ON passkey.policy_id = named.policy_id AND passkey.setting_key = '` + MFAPasskeyCounts.Key + `'
		WHERE named.value_json = 'true'`
}

// TwoFactorPoliciesSQL selects (user_id, policy_id, policy_name,
// passkey_counts) for everyone a named policy requires two-factor sign-in
// of: the policy, and whether it lets a passkey be the second step.
func TwoFactorPoliciesSQL() string {
	return twoFactorSQL(false)
}

// TwoFactorForUserSQL selects one row (required, passkey_counts) for the
// person given twice as the query's arguments: whether a named policy
// requires two-factor sign-in of them, and whether it lets a passkey count.
func TwoFactorForUserSQL() string {
	return `SELECT COUNT(*) > 0, COALESCE(MAX(passkey_counts), 0) FROM (` + twoFactorSQL(true) + `)`
}

// OwnProxiesAllowed reports whether the person may save proxies of their
// own and connect through them: the first named policy reaching them that
// says, or else the Default policy's switch. It's read with q, inside the
// caller's transaction when it has one.
func OwnProxiesAllowed(ctx context.Context, q Querier, userID string) (bool, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT named.value_json FROM (`+namedValuesSQL(OwnProxies, ownProxyValues, true)+`) named`, userID, userID).Scan(&value)
	if err == nil {
		return value == "true", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check whether a policy lets someone use their own proxies: %w", err)
	}
	var allowed bool
	if err := q.QueryRowContext(ctx, `SELECT `+peopleAddSQL).Scan(&allowed); err != nil {
		return false, fmt.Errorf("read whether people may add proxies: %w", err)
	}
	return allowed, nil
}

// OwnProxyOwners reports, for everyone who saved proxies of their own,
// whether they may use them, as OwnProxiesAllowed does for one person.
func OwnProxyOwners(ctx context.Context, q Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT owners.owner_user_id, COALESCE(named.value_json = 'true', `+peopleAddSQL+`)
		FROM (SELECT DISTINCT owner_user_id FROM outbound_proxies WHERE owner_user_id IS NOT NULL) owners
		LEFT JOIN (`+namedValuesSQL(OwnProxies, ownProxyValues, false)+`) named ON named.user_id = owners.owner_user_id`)
	if err != nil {
		return nil, fmt.Errorf("check whose policies let them use their own proxies: %w", err)
	}
	defer rows.Close()
	owners := map[string]bool{}
	for rows.Next() {
		var owner string
		var allowed bool
		if err := rows.Scan(&owner, &allowed); err != nil {
			return nil, err
		}
		owners[owner] = allowed
	}
	return owners, rows.Err()
}

// ownProxyValues are OwnProxies' valid values; peopleAddSQL is the Default
// policy's switch for it (outbound_proxy_policy.people_add).
var ownProxyValues = []string{"true", "false"}

const peopleAddSQL = `EXISTS (SELECT 1 FROM outbound_proxy_policy WHERE singleton = 1 AND people_add = 1)`
