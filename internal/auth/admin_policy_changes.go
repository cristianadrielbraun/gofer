package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// auditTargetPolicy is what the administration audit log calls a policy.
const auditTargetPolicy = "policy"

// PolicySummary is a policy on the Policies list: how many settings it
// sets and how many groups and people it's assigned to.
type PolicySummary struct {
	policy.Policy
	Settings int
	Groups   int
	People   int
}

// PolicyAssignment is a group or person a policy is assigned to, named.
type PolicyAssignment struct {
	policy.Target
	Name              string
	CreatedAt         time.Time
	CreatedByUsername string
}

// PolicyDetail is a policy, the settings it sets, and who it's assigned to.
type PolicyDetail struct {
	Policy      policy.Policy
	Values      []policy.Value
	Assignments []PolicyAssignment
}

// PolicyAssignmentChange is who a change to a policy's assignments
// affected, named; targets it already had, or didn't, aren't.
type PolicyAssignmentChange struct {
	Policy  policy.Policy
	Changed []PolicyAssignment
}

// CreatePolicyOptions names a new policy.
type CreatePolicyOptions struct {
	ActorUserID    string
	ActorSessionID string
	Name           string
	Description    string
}

// UpdatePolicyOptions renames a policy or changes its description, from the
// version the administrator loaded.
type UpdatePolicyOptions struct {
	ActorUserID    string
	ActorSessionID string
	PolicyID       string
	Version        int64
	Name           string
	Description    string
}

// SetPolicyValuesOptions changes a policy's settings, from the version
// the administrator loaded.
type SetPolicyValuesOptions struct {
	ActorUserID    string
	ActorSessionID string
	PolicyID       string
	Version        int64
	Changes        []policy.ValueChange
}

// ChangePolicyAssignmentsOptions assigns a policy to groups and people, or
// takes it away from them.
type ChangePolicyAssignmentsOptions struct {
	ActorUserID    string
	ActorSessionID string
	PolicyID       string
	Targets        []policy.Target
}

// ListPolicies returns every policy in order, the Default policy last,
// with what each sets and who it's assigned to.
func (m *Manager) ListPolicies(ctx context.Context, actorUserID string) ([]PolicySummary, error) {
	var summaries []PolicySummary
	err := m.readPolicies(ctx, actorUserID, func(tx *sql.Tx) error {
		policies, err := policy.List(ctx, tx)
		if err != nil {
			return err
		}
		for _, item := range policies {
			summary := PolicySummary{Policy: item}
			if err := tx.QueryRowContext(ctx, `
				SELECT (SELECT COUNT(*) FROM policy_values WHERE policy_id = ?),
				       (SELECT COUNT(*) FROM policy_assignments WHERE policy_id = ? AND target_kind = 'group'),
				       (SELECT COUNT(*) FROM policy_assignments WHERE policy_id = ? AND target_kind = 'user')`,
				item.ID, item.ID, item.ID,
			).Scan(&summary.Settings, &summary.Groups, &summary.People); err != nil {
				return fmt.Errorf("count what a policy sets: %w", err)
			}
			summaries = append(summaries, summary)
		}
		return nil
	})
	return summaries, err
}

// GetPolicy returns a policy, its settings and who it's assigned to.
func (m *Manager) GetPolicy(ctx context.Context, actorUserID, policyID string) (*PolicyDetail, error) {
	var detail *PolicyDetail
	err := m.readPolicies(ctx, actorUserID, func(tx *sql.Tx) error {
		found, err := policy.Get(ctx, tx, policyID)
		if err != nil {
			return err
		}
		values, err := policy.Values(ctx, tx, policyID)
		if err != nil {
			return err
		}
		assignments, err := policy.Assignments(ctx, tx, policyID)
		if err != nil {
			return err
		}
		detail = &PolicyDetail{Policy: found, Values: values}
		for _, assignment := range assignments {
			named, err := namePolicyAssignment(ctx, tx, assignment.Target)
			if err != nil {
				return err
			}
			named.CreatedAt = assignment.CreatedAt
			if named.CreatedByUsername, err = usernameForAudit(ctx, tx, assignment.CreatedBy); err != nil {
				return err
			}
			detail.Assignments = append(detail.Assignments, named)
		}
		return nil
	})
	return detail, err
}

func (m *Manager) readPolicies(ctx context.Context, actorUserID string, read func(*sql.Tx) error) error {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin policy read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, strings.TrimSpace(actorUserID), authz.PoliciesRead); err != nil {
		return err
	}
	return read(tx)
}

// CreatePolicy adds a policy, last in the order before the Default one.
// It sets nothing and applies to nobody until it's given settings and
// assigned.
func (m *Manager) CreatePolicy(ctx context.Context, options CreatePolicyOptions) (*policy.Policy, error) {
	var created policy.Policy
	err := m.changePolicies(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		id, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate policy ID: %w", err)
		}
		if created, err = policy.Create(ctx, tx, policy.NewPolicy{ID: id, Name: options.Name, Description: options.Description, By: actorUserID, At: now}); err != nil {
			return err
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: "policy.created", Target: policyAuditTarget(created),
			Changes:  map[string]audit.Change{"name": {To: created.Name}, "description": {To: created.Description}},
			Metadata: map[string]any{"priority": created.Priority},
		})
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// UpdatePolicy renames a policy or changes its description. Renaming is
// recorded as policy.renamed, a new description alone as policy.updated;
// a change that changes nothing isn't recorded.
func (m *Manager) UpdatePolicy(ctx context.Context, options UpdatePolicyOptions) (*policy.Policy, error) {
	var updated policy.Policy
	err := m.changePolicies(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		before, err := policy.Get(ctx, tx, options.PolicyID)
		if err != nil {
			return err
		}
		updated, err = policy.Update(ctx, tx, policy.PolicyChange{
			ID: options.PolicyID, Version: options.Version, Name: options.Name, Description: options.Description, By: actorUserID, At: now,
		})
		if err != nil || updated.Version == before.Version {
			return err
		}
		changes := map[string]audit.Change{}
		action := "policy.updated"
		if updated.Name != before.Name {
			action = "policy.renamed"
			changes["name"] = audit.Change{From: before.Name, To: updated.Name}
		}
		if updated.Description != before.Description {
			changes["description"] = audit.Change{From: before.Description, To: updated.Description}
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: action, Target: policyAuditTarget(updated), Changes: changes,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DeletePolicy removes a policy, its settings and its assignments, from
// the version the administrator loaded. Deleting one that sets security
// settings for someone takes the security permission too.
func (m *Manager) DeletePolicy(ctx context.Context, actorUserID, actorSessionID, policyID string, version int64) error {
	return m.changePolicies(ctx, actorUserID, actorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		found, err := policy.Get(ctx, tx, policyID)
		if err != nil {
			return err
		}
		values, err := policy.Values(ctx, tx, policyID)
		if err != nil {
			return err
		}
		assignments, err := policy.Assignments(ctx, tx, policyID)
		if err != nil {
			return err
		}
		if len(assignments) > 0 && setsSecurity(values) {
			if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.PoliciesManageSecurity, now); err != nil {
				return err
			}
		}
		if err := policy.Delete(ctx, tx, policyID, version); err != nil {
			return err
		}
		groups, people := 0, 0
		for _, assignment := range assignments {
			if assignment.Kind == policy.TargetGroup {
				groups++
			} else {
				people++
			}
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: "policy.deleted", Target: policyAuditTarget(found),
			Metadata: map[string]any{"settings": len(values), "groups": groups, "people": people},
		})
	})
}

// ReorderPolicies puts the policies in the order ids gives: every policy
// but the Default one, exactly once. An earlier policy's setting wins.
func (m *Manager) ReorderPolicies(ctx context.Context, actorUserID, actorSessionID string, ids []string) error {
	return m.changePolicies(ctx, actorUserID, actorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		before, err := policy.List(ctx, tx)
		if err != nil {
			return err
		}
		if err := policy.Reorder(ctx, tx, ids); err != nil {
			return err
		}
		after, err := policy.List(ctx, tx)
		if err != nil {
			return err
		}
		if policyOrder(before) == policyOrder(after) {
			return nil
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:  "policy.reordered",
			Target:  audit.Target{Kind: auditTargetPolicy},
			Changes: map[string]audit.Change{"order": {From: policyNames(before), To: policyNames(after)}},
		})
	})
}

// SetPolicyValues changes a policy's settings as one change. A security
// setting takes the security permission too.
func (m *Manager) SetPolicyValues(ctx context.Context, options SetPolicyValuesOptions) (*policy.Policy, error) {
	var saved policy.Policy
	err := m.changePolicies(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		for _, change := range options.Changes {
			if setting, ok := policy.Lookup(change.Key); ok && setting.Kind == policy.KindSecurity {
				if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.PoliciesManageSecurity, now); err != nil {
					return err
				}
				break
			}
		}
		before, err := policy.Values(ctx, tx, options.PolicyID)
		if err != nil {
			return err
		}
		previous, err := policy.Get(ctx, tx, options.PolicyID)
		if err != nil {
			return err
		}
		if saved, err = policy.SetValues(ctx, tx, options.PolicyID, options.Version, options.Changes, actorUserID, now); err != nil {
			return err
		}
		if saved.Version == previous.Version {
			return nil
		}
		old := make(map[string]policy.Value, len(before))
		for _, value := range before {
			old[value.Key] = value
		}
		changes := map[string]audit.Change{}
		for _, change := range options.Changes {
			was, had := old[change.Key]
			from, to := any(nil), any(nil)
			if had {
				from = decodeAuditValue(was.JSON)
			}
			if change.JSON != nil {
				to = decodeAuditValue(change.JSON)
			}
			if had == (change.JSON != nil) && fmt.Sprint(from) == fmt.Sprint(to) && was.Locked == change.Locked && was.Enforced == change.Enforced {
				continue
			}
			changes[change.Key] = audit.Change{From: from, To: to}
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: "policy.settings_changed", Target: policyAuditTarget(saved), Changes: changes,
		})
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// AssignPolicy gives a policy to groups and people. Assigning one that
// sets security settings takes the security permission too.
func (m *Manager) AssignPolicy(ctx context.Context, options ChangePolicyAssignmentsOptions) (*PolicyAssignmentChange, error) {
	return m.changePolicyAssignments(ctx, options, true)
}

// UnassignPolicy takes a policy away from groups and people.
func (m *Manager) UnassignPolicy(ctx context.Context, options ChangePolicyAssignmentsOptions) (*PolicyAssignmentChange, error) {
	return m.changePolicyAssignments(ctx, options, false)
}

func (m *Manager) changePolicyAssignments(ctx context.Context, options ChangePolicyAssignmentsOptions, assign bool) (*PolicyAssignmentChange, error) {
	result := &PolicyAssignmentChange{}
	err := m.changePolicies(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		found, err := policy.Get(ctx, tx, options.PolicyID)
		if err != nil {
			return err
		}
		values, err := policy.Values(ctx, tx, options.PolicyID)
		if err != nil {
			return err
		}
		if setsSecurity(values) {
			if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.PoliciesManageSecurity, now); err != nil {
				return err
			}
		}
		var changed []policy.Target
		if assign {
			changed, err = policy.Assign(ctx, tx, options.PolicyID, options.Targets, actorUserID, now)
		} else {
			changed, err = policy.Unassign(ctx, tx, options.PolicyID, options.Targets)
		}
		if err != nil {
			return err
		}
		result.Policy = found
		if len(changed) == 0 {
			return nil
		}
		targets := make([]map[string]string, 0, len(changed))
		for _, target := range changed {
			named, err := namePolicyAssignment(ctx, tx, target)
			if err != nil {
				return err
			}
			result.Changed = append(result.Changed, named)
			targets = append(targets, map[string]string{"kind": string(target.Kind), "id": target.ID, "name": named.Name})
		}
		action := "policy.unassigned"
		if assign {
			action = "policy.assigned"
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: action, Target: policyAuditTarget(found),
			Metadata: map[string]any{"count": len(changed), "targets": targets},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// changePolicies runs change in a transaction once the actor may manage
// policies from a recently verified session.
func (m *Manager) changePolicies(
	ctx context.Context, actorUserID, actorSessionID string,
	change func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error,
) error {
	actorUserID = strings.TrimSpace(actorUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	if actorUserID == "" {
		return ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return ErrRecentStepUpRequired
	}
	now := m.clock.Now().UTC()
	return m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.PoliciesManage, now); err != nil {
			return err
		}
		return change(tx, actorUserID, actorSessionID, now)
	})
}

// namePolicyAssignment names a group or person a policy is assigned to,
// as they're called now.
func namePolicyAssignment(ctx context.Context, tx *sql.Tx, target policy.Target) (PolicyAssignment, error) {
	named := PolicyAssignment{Target: target}
	var query string
	switch target.Kind {
	case policy.TargetGroup:
		query = `SELECT name FROM directory_groups WHERE id = ?`
	case policy.TargetUser:
		query = `SELECT username FROM users WHERE id = ?`
	default:
		return named, nil
	}
	err := tx.QueryRowContext(ctx, query, target.ID).Scan(&named.Name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return named, fmt.Errorf("name a policy's %s: %w", target.Kind, err)
	}
	return named, nil
}

// setsSecurity reports whether values set any security setting.
func setsSecurity(values []policy.Value) bool {
	for _, value := range values {
		if setting, ok := policy.Lookup(value.Key); ok && setting.Kind == policy.KindSecurity {
			return true
		}
	}
	return false
}

func decodeAuditValue(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return value
}

func policyOrder(policies []policy.Policy) string {
	ids := make([]string, len(policies))
	for i, item := range policies {
		ids[i] = item.ID
	}
	return strings.Join(ids, " ")
}

func policyNames(policies []policy.Policy) []string {
	var names []string
	for _, item := range policies {
		if !item.Default {
			names = append(names, item.Name)
		}
	}
	return names
}

func policyAuditTarget(found policy.Policy) audit.Target {
	return audit.Target{Kind: auditTargetPolicy, ID: found.ID, Label: found.Name}
}
