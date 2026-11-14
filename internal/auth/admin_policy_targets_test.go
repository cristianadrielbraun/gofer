package auth

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

func TestPoliciesOfGroupsAndPeopleSayHowEachReachesThem(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	actor := func() (string, string) { return "administrator", session.ID }
	newGroup := func(name string, people ...string) string {
		t.Helper()
		user, sessionID := actor()
		group, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: user, ActorSessionID: sessionID, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AddGroupMembers(ctx, ChangeGroupMembersOptions{ActorUserID: user, ActorSessionID: sessionID, GroupID: group.ID, UserIDs: people}); err != nil {
			t.Fatal(err)
		}
		return group.ID
	}
	newPolicy := func(name string, changes ...policy.ValueChange) string {
		t.Helper()
		user, sessionID := actor()
		created, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: user, ActorSessionID: sessionID, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) > 0 {
			if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: created.ID, Version: created.Version, Changes: changes}); err != nil {
				t.Fatal(err)
			}
		}
		return created.ID
	}
	assign := func(policyID string, targets ...policy.Target) {
		t.Helper()
		user, sessionID := actor()
		if _, err := manager.AssignPolicy(ctx, ChangePolicyAssignmentsOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: policyID, Targets: targets}); err != nil {
			t.Fatal(err)
		}
	}

	field := newGroup("Field team", "ines", "pavel")
	auditors := newGroup("Auditors", "ines")
	office := newGroup("Office", "noor")
	finance := newPolicy("Finance", policy.ValueChange{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)})
	contractors := newPolicy("Contractors")
	assign(finance, policy.Target{Kind: policy.TargetGroup, ID: field}, policy.Target{Kind: policy.TargetGroup, ID: auditors}, policy.Target{Kind: policy.TargetUser, ID: "ines"})
	assign(contractors, policy.Target{Kind: policy.TargetGroup, ID: office})

	if _, err := manager.PoliciesOfPerson(ctx, "ines", "ines"); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("a webmail user reads someone's policies = %v", err)
	}
	ines, err := manager.PoliciesOfPerson(ctx, "administrator", "ines")
	if err != nil || len(ines.Policies) != 1 {
		t.Fatalf("Ines's policies = %#v, %v", ines, err)
	}
	got := ines.Policies[0]
	wantGroups := []GroupRef{{ID: auditors, Name: "Auditors"}, {ID: field, Name: "Field team"}}
	if got.ID != finance || !got.Direct || got.AssignedByUsername != "administrator" || !got.Security ||
		!reflect.DeepEqual(got.Groups, wantGroups) || !reflect.DeepEqual(got.Keys, []string{policy.MFARequired.Key}) {
		t.Fatalf("Finance for Ines = %#v", got)
	}
	// The resolver credits her direct assignment; what applies yet is the
	// Default policy's.
	if source := ines.Effective.Get(policy.MFARequired).Source; source.Kind != policy.SourcePolicy || source.PolicyID != finance || source.GroupID != "" {
		t.Fatalf("Ines's two-factor source = %#v", source)
	}
	if applied := ines.Instance.Get(policy.MFARequired); applied.Value != false {
		t.Fatalf("the Default policy's two-factor = %#v", applied)
	}

	pavel, err := manager.PoliciesOfPerson(ctx, "administrator", "pavel")
	if err != nil || len(pavel.Policies) != 1 || pavel.Policies[0].Direct || !reflect.DeepEqual(pavel.Policies[0].Groups, []GroupRef{{ID: field, Name: "Field team"}}) {
		t.Fatalf("Pavel's policies = %#v, %v", pavel, err)
	}
	noor, err := manager.PoliciesOfPerson(ctx, "administrator", "noor")
	if err != nil || len(noor.Policies) != 1 || noor.Policies[0].ID != contractors || noor.Policies[0].Security || len(noor.Policies[0].Keys) != 0 {
		t.Fatalf("Noor's policies = %#v, %v", noor, err)
	}

	ofField, err := manager.PoliciesOfGroup(ctx, "administrator", field)
	if err != nil || len(ofField) != 1 || !ofField[0].Direct || len(ofField[0].Groups) != 0 || ofField[0].AssignedByUsername != "administrator" {
		t.Fatalf("Field team's policies = %#v, %v", ofField, err)
	}
	protected, err := manager.ProtectedGroups(ctx, "administrator")
	if want := map[string]bool{field: true, auditors: true}; err != nil || !reflect.DeepEqual(protected, want) {
		t.Fatalf("protected groups = %#v, %v; want %#v", protected, err, want)
	}
	if _, err := manager.ProtectedGroups(ctx, "pavel"); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("a webmail user reads the protected groups = %v", err)
	}
}
