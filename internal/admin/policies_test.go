package admin

import (
	"encoding/json"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

func TestPolicyChangesSayWhyTheyWereRefused(t *testing.T) {
	service, _, actor := newTestService(t, nil)
	ctx := t.Context()
	contractors, err := service.CreatePolicy(ctx, actor, "Contractors", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		err  func() error
		want Code
	}{
		{"same name", func() error { _, err := service.CreatePolicy(ctx, actor, "CONTRACTORS", ""); return err }, CodeNameTaken},
		{"the Default policy's name", func() error { _, err := service.CreatePolicy(ctx, actor, "Default policy", ""); return err }, CodeNameTaken},
		{"empty name", func() error { _, err := service.CreatePolicy(ctx, actor, " ", ""); return err }, CodeInvalid},
		{"old page", func() error {
			_, err := service.UpdatePolicy(ctx, actor, contractors.ID, 7, "Temps", "")
			return err
		}, CodeStale},
		{"rename the Default policy", func() error {
			_, err := service.UpdatePolicy(ctx, actor, policy.DefaultID, 0, "Everyone", "")
			return err
		}, CodeInvalid},
		{"wrong order", func() error { return service.ReorderPolicies(ctx, actor, []string{"p-nope"}) }, CodeInvalid},
		{"value out of bounds", func() error {
			_, err := service.SetPolicyValues(ctx, actor, contractors.ID, 1, []policy.ValueChange{{Key: policy.OwnProxies.Key, JSON: json.RawMessage(`"yes"`)}})
			return err
		}, CodeInvalid},
		{"a setting that lives elsewhere", func() error {
			_, err := service.SetPolicyValues(ctx, actor, policy.DefaultID, 0, []policy.ValueChange{{Key: policy.OwnProxies.Key, JSON: json.RawMessage(`true`)}})
			return err
		}, CodeInvalid},
		{"missing policy", func() error { return service.DeletePolicy(ctx, actor, "p-gone", 1) }, CodeUnavailable},
		{"an administrator", func() error {
			_, err := service.AssignPolicy(ctx, actor, contractors.ID, []policy.Target{{Kind: policy.TargetUser, ID: "head-admin"}})
			return err
		}, CodeUnavailable},
		{"nobody", func() error { _, err := service.AssignPolicy(ctx, actor, contractors.ID, nil); return err }, CodeInvalid},
		{"webmail user", func() error {
			_, err := service.ListPolicies(ctx, Actor{UserID: "milo-rivers", SessionID: "milo-session"})
			return err
		}, CodeForbidden},
	} {
		if err := test.err(); CodeOf(err) != test.want {
			t.Errorf("%s: %v (%s), want %s", test.name, err, CodeOf(err), test.want)
		}
	}
	if change, err := service.AssignPolicy(ctx, actor, contractors.ID, []policy.Target{{Kind: policy.TargetUser, ID: "milo-rivers"}}); err != nil || len(change.Changed) != 1 {
		t.Fatalf("assign Milo = %+v, %v", change, err)
	}
}
