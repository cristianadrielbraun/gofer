package auth

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

func policyAuditRecords(t *testing.T, manager *Manager) []audit.Record {
	t.Helper()
	page, err := audit.List(t.Context(), manager.db.Read(), audit.Filter{Action: "policy."}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return page.Records
}

func TestPoliciesAreChangedOnlyByAdministratorsAndRecorded(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	if _, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "ines", ActorSessionID: "s", Name: "Contractors"}); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("webmail user creates a policy = %v", err)
	}
	if _, err := manager.ListPolicies(ctx, "ines"); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("webmail user lists policies = %v", err)
	}
	unverified := createPolicyAdministratorSession(t, manager)
	if _, err := manager.db.Write().Exec(`UPDATE sessions SET step_up_at = NULL, step_up_method = '' WHERE id = ?`, unverified.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: unverified.ID, Name: "Contractors"}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("create without a recent verification = %v", err)
	}

	contractors, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Contractors", Description: "Short stays"})
	if err != nil || contractors.Priority != 1 {
		t.Fatalf("created = %#v, %v", contractors, err)
	}
	finance, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Finance"})
	if err != nil || finance.Priority != 2 {
		t.Fatalf("created = %#v, %v", finance, err)
	}
	if _, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "finance"}); !errors.Is(err, policy.ErrNameTaken) {
		t.Fatalf("same name = %v", err)
	}
	renamed, err := manager.UpdatePolicy(ctx, UpdatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: contractors.ID, Version: 1, Name: "Contractors and temps", Description: "Short stays"})
	if err != nil || renamed.Version != 2 {
		t.Fatalf("renamed = %#v, %v", renamed, err)
	}
	if err := manager.ReorderPolicies(ctx, "administrator", session.ID, []string{finance.ID, contractors.ID}); err != nil {
		t.Fatal(err)
	}
	// The same order again isn't recorded.
	if err := manager.ReorderPolicies(ctx, "administrator", session.ID, []string{finance.ID, contractors.ID}); err != nil {
		t.Fatal(err)
	}

	saved, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: finance.ID, Version: 1,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)}},
	})
	if err != nil || saved.Version != 2 {
		t.Fatalf("saved = %#v, %v", saved, err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: finance.ID, Version: 2,
		Changes: []policy.ValueChange{{Key: policy.SecurityRetentionDays.Key, JSON: json.RawMessage(`30`)}},
	}); !errors.Is(err, policy.ErrInstanceOnly) {
		t.Fatalf("whole-server setting on a policy = %v", err)
	}

	added, err := manager.AssignPolicy(ctx, ChangePolicyAssignmentsOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: finance.ID,
		Targets: []policy.Target{{Kind: policy.TargetUser, ID: "ines"}, {Kind: policy.TargetUser, ID: "pavel"}},
	})
	if err != nil || len(added.Changed) != 2 || added.Changed[0].Name != "ines" {
		t.Fatalf("assigned = %#v, %v", added, err)
	}
	if _, err := manager.AssignPolicy(ctx, ChangePolicyAssignmentsOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: finance.ID,
		Targets: []policy.Target{{Kind: policy.TargetUser, ID: "administrator"}},
	}); !errors.Is(err, policy.ErrTargetNotFound) {
		t.Fatalf("assign to an administrator = %v", err)
	}
	removed, err := manager.UnassignPolicy(ctx, ChangePolicyAssignmentsOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: finance.ID,
		Targets: []policy.Target{{Kind: policy.TargetUser, ID: "pavel"}},
	})
	if err != nil || len(removed.Changed) != 1 || removed.Changed[0].Name != "pavel" {
		t.Fatalf("unassigned = %#v, %v", removed, err)
	}

	detail, err := manager.GetPolicy(ctx, "administrator", finance.ID)
	if err != nil || len(detail.Values) != 1 || len(detail.Assignments) != 1 || detail.Assignments[0].Name != "ines" || detail.Assignments[0].CreatedByUsername != "administrator" {
		t.Fatalf("detail = %#v, %v", detail, err)
	}
	summaries, err := manager.ListPolicies(ctx, "administrator")
	if err != nil || len(summaries) != 3 || summaries[0].ID != finance.ID || summaries[0].Settings != 1 || summaries[0].People != 1 || !summaries[2].Default {
		t.Fatalf("summaries = %#v, %v", summaries, err)
	}
	effective, err := policy.ForUser(ctx, manager.db.Read(), "ines")
	if err != nil || !effective.Bool(policy.MFARequired) || effective.Get(policy.MFARequired).Source.PolicyName != "Finance" {
		t.Fatalf("Ines's settings = %v, %v", effective, err)
	}

	if err := manager.DeletePolicy(ctx, "administrator", session.ID, finance.ID, 1); !errors.Is(err, policy.ErrStale) {
		t.Fatalf("delete from an old page = %v", err)
	}
	if err := manager.DeletePolicy(ctx, "administrator", session.ID, finance.ID, 2); err != nil {
		t.Fatal(err)
	}

	records := policyAuditRecords(t, manager)
	var actions []string
	for i := len(records) - 1; i >= 0; i-- {
		actions = append(actions, records[i].Action)
	}
	want := []string{"policy.created", "policy.created", "policy.renamed", "policy.reordered", "policy.settings_changed", "policy.assigned", "policy.unassigned", "policy.deleted"}
	if len(actions) != len(want) {
		t.Fatalf("recorded %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("recorded %v, want %v", actions, want)
		}
	}
	settings := records[3]
	if change := settings.Changes[policy.MFARequired.Key]; change.From != nil || change.To != true || settings.Target.Label != "Finance" {
		t.Fatalf("settings record = %#v", settings)
	}
	reordered := records[4]
	if order := reordered.Changes["order"]; len(order.To.([]any)) != 2 || order.To.([]any)[0] != "Finance" {
		t.Fatalf("order record = %#v", reordered)
	}
	if deleted := records[0]; deleted.Metadata["people"] != float64(1) || deleted.Metadata["settings"] != float64(1) {
		t.Fatalf("deletion record = %#v", deleted)
	}
}

// A change whose record can't be written doesn't happen.
func TestPolicyChangesRollBackWhenTheRecordFails(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	contractors, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Contractors"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().Exec(`
		CREATE TRIGGER reject_policy_records BEFORE INSERT ON audit_events
		WHEN NEW.action LIKE 'policy.%'
		BEGIN SELECT RAISE(ABORT, 'reject policy records'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AssignPolicy(ctx, ChangePolicyAssignmentsOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: contractors.ID,
		Targets: []policy.Target{{Kind: policy.TargetUser, ID: "ines"}},
	}); err == nil {
		t.Fatal("assignment without its record went through")
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: contractors.ID, Version: 1,
		Changes: []policy.ValueChange{{Key: policy.OwnProxies.Key, JSON: json.RawMessage(`true`)}},
	}); err == nil {
		t.Fatal("settings saved without their record")
	}
	detail, err := manager.GetPolicy(ctx, "administrator", contractors.ID)
	if err != nil || len(detail.Values) != 0 || len(detail.Assignments) != 0 || detail.Policy.Version != 1 {
		t.Fatalf("after the refused changes = %#v, %v", detail, err)
	}
}
