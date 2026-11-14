package admin

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// Policies are read with policies.read and changed with policies.manage;
// security settings, and assigning or deleting a policy that sets any,
// take policies.manage_security too. Every change needs a recently
// verified session and is recorded in the Activity log in the transaction
// that makes it.

// ListPolicies returns every policy in order, the Default policy last.
func (s *Service) ListPolicies(ctx context.Context, actor Actor) ([]auth.PolicySummary, error) {
	return s.auth.ListPolicies(ctx, actor.UserID)
}

// Policy returns a policy, its settings and who it's assigned to.
func (s *Service) Policy(ctx context.Context, actor Actor, policyID string) (*auth.PolicyDetail, error) {
	return s.auth.GetPolicy(ctx, actor.UserID, policyID)
}

// CreatePolicy adds a policy, last before the Default one.
func (s *Service) CreatePolicy(ctx context.Context, actor Actor, name, description string) (*policy.Policy, error) {
	return s.auth.CreatePolicy(ctx, auth.CreatePolicyOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Name: name, Description: description,
	})
}

// UpdatePolicy renames a policy or changes its description, from the
// version the administrator loaded.
func (s *Service) UpdatePolicy(ctx context.Context, actor Actor, policyID string, version int64, name, description string) (*policy.Policy, error) {
	return s.auth.UpdatePolicy(ctx, auth.UpdatePolicyOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, PolicyID: policyID, Version: version, Name: name, Description: description,
	})
}

// DeletePolicy removes a policy, from the version the administrator loaded.
func (s *Service) DeletePolicy(ctx context.Context, actor Actor, policyID string, version int64) error {
	return s.auth.DeletePolicy(ctx, actor.UserID, actor.SessionID, policyID, version)
}

// ReorderPolicies puts the policies in the order ids gives.
func (s *Service) ReorderPolicies(ctx context.Context, actor Actor, ids []string) error {
	return s.auth.ReorderPolicies(ctx, actor.UserID, actor.SessionID, ids)
}

// SetPolicyValues changes a policy's settings, from the version the
// administrator loaded.
func (s *Service) SetPolicyValues(ctx context.Context, actor Actor, policyID string, version int64, changes []policy.ValueChange) (*policy.Policy, error) {
	return s.auth.SetPolicyValues(ctx, auth.SetPolicyValuesOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, PolicyID: policyID, Version: version, Changes: changes,
	})
}

// AssignPolicy gives a policy to up to MaxBulkPeople groups and people.
func (s *Service) AssignPolicy(ctx context.Context, actor Actor, policyID string, targets []policy.Target) (*auth.PolicyAssignmentChange, error) {
	if err := checkPolicyTargets(targets); err != nil {
		return nil, err
	}
	return s.auth.AssignPolicy(ctx, auth.ChangePolicyAssignmentsOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, PolicyID: policyID, Targets: targets,
	})
}

// UnassignPolicy takes a policy away from up to MaxBulkPeople groups and
// people.
func (s *Service) UnassignPolicy(ctx context.Context, actor Actor, policyID string, targets []policy.Target) (*auth.PolicyAssignmentChange, error) {
	if err := checkPolicyTargets(targets); err != nil {
		return nil, err
	}
	return s.auth.UnassignPolicy(ctx, auth.ChangePolicyAssignmentsOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, PolicyID: policyID, Targets: targets,
	})
}

func checkPolicyTargets(targets []policy.Target) error {
	switch {
	case len(targets) == 0:
		return errBulkNobody
	case len(targets) > MaxBulkPeople:
		return errBulkTooMany
	}
	return nil
}

// PoliciesOfGroup returns the policies assigned to a group, in order.
func (s *Service) PoliciesOfGroup(ctx context.Context, actor Actor, groupID string) ([]auth.TargetPolicy, error) {
	return s.auth.PoliciesOfGroup(ctx, actor.UserID, groupID)
}

// PoliciesOfPerson works out every setting's value for someone, and the
// policies that apply to them.
func (s *Service) PoliciesOfPerson(ctx context.Context, actor Actor, userID string) (*auth.PersonPolicies, error) {
	return s.auth.PoliciesOfPerson(ctx, actor.UserID, userID)
}

// ProtectedGroups returns the IDs of the groups a policy sets security
// settings for.
func (s *Service) ProtectedGroups(ctx context.Context, actor Actor) (map[string]bool, error) {
	return s.auth.ProtectedGroups(ctx, actor.UserID)
}
