package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// TargetPolicy is a policy assigned to a group or a person: where it is
// in the order, what it sets, and how it reaches them.
type TargetPolicy struct {
	policy.Policy
	// Keys are the settings it sets.
	Keys []string
	// Security: it sets a security setting.
	Security bool
	// Direct: it's assigned to the group or person itself. AssignedAt and
	// AssignedByUsername say when, and who did it.
	Direct             bool
	AssignedAt         time.Time
	AssignedByUsername string
	// Groups are the person's groups it's assigned to, by name.
	Groups []GroupRef
}

// PersonPolicies is what the policies decide for one person.
type PersonPolicies struct {
	// Effective is every setting's value for them, and where it comes
	// from.
	Effective *policy.Effective
	// Instance is the Default policy's values alone, which settings that
	// only read the Default policy give everyone.
	Instance *policy.Effective
	// Policies are the ones that apply to them, in order.
	Policies []TargetPolicy
}

// PoliciesOfGroup returns the policies assigned to a group, in order.
func (m *Manager) PoliciesOfGroup(ctx context.Context, actorUserID, groupID string) ([]TargetPolicy, error) {
	var found []TargetPolicy
	err := m.readPolicies(ctx, actorUserID, func(tx *sql.Tx) error {
		routes, err := policyRoutes(ctx, tx, `a.target_kind = 'group' AND a.target_id = ?`, groupID)
		if err != nil {
			return err
		}
		found, err = targetPolicies(ctx, tx, routes, true)
		return err
	})
	return found, err
}

// PoliciesOfPerson works out every setting's value for someone, and the
// policies that apply to them, directly and through each of their groups.
func (m *Manager) PoliciesOfPerson(ctx context.Context, actorUserID, userID string) (*PersonPolicies, error) {
	result := &PersonPolicies{}
	err := m.readPolicies(ctx, actorUserID, func(tx *sql.Tx) error {
		var err error
		if result.Effective, err = policy.ForUser(ctx, tx, userID); err != nil {
			return err
		}
		if result.Instance, err = policy.ForInstance(ctx, tx); err != nil {
			return err
		}
		routes, err := policyRoutes(ctx, tx, `
			(a.target_kind = 'user' AND a.target_id = ?)
			OR (a.target_kind = 'group' AND a.target_id IN (SELECT group_id FROM directory_group_members WHERE user_id = ?))`,
			userID, userID)
		if err != nil {
			return err
		}
		result.Policies, err = targetPolicies(ctx, tx, routes, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ProtectedGroups returns the IDs of the groups a policy sets security
// settings for.
func (m *Manager) ProtectedGroups(ctx context.Context, actorUserID string) (map[string]bool, error) {
	var protected map[string]bool
	err := m.readPolicies(ctx, actorUserID, func(tx *sql.Tx) (err error) {
		protected, err = policy.ProtectedGroups(ctx, tx)
		return err
	})
	return protected, err
}

// policyRoute is how one policy reaches a group or person: assigned to
// them, or to one of their groups.
type policyRoute struct {
	direct    bool
	at        time.Time
	by        string
	groupID   string
	groupName string
}

// policyRoutes reads the assignments where matches, by policy, the groups
// in name order.
func policyRoutes(ctx context.Context, tx *sql.Tx, where string, args ...any) (map[string][]policyRoute, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.policy_id, a.target_kind, a.created_at, a.created_by, COALESCE(g.id, ''), COALESCE(g.name, '')
		FROM policy_assignments a
		LEFT JOIN directory_groups g ON a.target_kind = 'group' AND g.id = a.target_id
		WHERE `+where+`
		ORDER BY g.name_normalized, g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list policy assignments: %w", err)
	}
	defer rows.Close()
	routes := map[string][]policyRoute{}
	for rows.Next() {
		var policyID, kind string
		var route policyRoute
		if err := rows.Scan(&policyID, &kind, &route.at, &route.by, &route.groupID, &route.groupName); err != nil {
			return nil, fmt.Errorf("read policy assignment: %w", err)
		}
		route.direct = kind == string(policy.TargetUser)
		routes[policyID] = append(routes[policyID], route)
	}
	return routes, rows.Err()
}

// targetPolicies are the policies routes name, in order, with what each
// sets and how it reaches them. For a group's own policies, ofGroup, its
// assignments are direct.
func targetPolicies(ctx context.Context, tx *sql.Tx, routes map[string][]policyRoute, ofGroup bool) ([]TargetPolicy, error) {
	if len(routes) == 0 {
		return nil, nil
	}
	policies, err := policy.List(ctx, tx)
	if err != nil {
		return nil, err
	}
	var found []TargetPolicy
	for _, item := range policies {
		reaches, ok := routes[item.ID]
		if !ok || item.Default {
			continue
		}
		values, err := policy.Values(ctx, tx, item.ID)
		if err != nil {
			return nil, err
		}
		target := TargetPolicy{Policy: item, Security: policy.SetsSecurity(values)}
		for _, value := range values {
			target.Keys = append(target.Keys, value.Key)
		}
		for _, route := range reaches {
			if !route.direct && !ofGroup {
				target.Groups = append(target.Groups, GroupRef{ID: route.groupID, Name: route.groupName})
				continue
			}
			target.Direct = true
			target.AssignedAt = route.at
			if target.AssignedByUsername, err = usernameForAudit(ctx, tx, route.by); err != nil {
				return nil, err
			}
		}
		found = append(found, target)
	}
	return found, nil
}
