package admin

import (
	"fmt"
	"strings"
	"testing"
)

func TestGroupChangesSayWhyTheyWereRefused(t *testing.T) {
	service, _, actor := newTestService(t, nil)
	ctx := t.Context()
	field, err := service.CreateGroup(ctx, actor, "Field team", "")
	if err != nil {
		t.Fatal(err)
	}
	if change, err := service.AddToGroup(ctx, actor, field.ID, []string{"milo-rivers"}); err != nil || len(change.Changed) != 1 {
		t.Fatalf("AddToGroup = %+v, %v", change, err)
	}
	tooMany := make([]string, MaxBulkPeople+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("person-%03d", i)
	}
	for _, test := range []struct {
		name string
		err  func() error
		want Code
	}{
		{"same name", func() error { _, err := service.CreateGroup(ctx, actor, "field TEAM", ""); return err }, CodeNameTaken},
		{"empty name", func() error { _, err := service.CreateGroup(ctx, actor, "  ", ""); return err }, CodeInvalid},
		{"long description", func() error {
			_, err := service.CreateGroup(ctx, actor, "Night shift", strings.Repeat("d", 501))
			return err
		}, CodeInvalid},
		{"old page", func() error { _, err := service.UpdateGroup(ctx, actor, field.ID, 9, "Field crew", ""); return err }, CodeStale},
		{"missing group", func() error { _, err := service.DeleteGroup(ctx, actor, "no-such-group", 1); return err }, CodeUnavailable},
		{"an administrator", func() error { _, err := service.AddToGroup(ctx, actor, field.ID, []string{"head-admin"}); return err }, CodeUnavailable},
		{"nobody", func() error { _, err := service.RemoveFromGroup(ctx, actor, field.ID, nil); return err }, CodeInvalid},
		{"too many", func() error { _, err := service.AddToGroup(ctx, actor, field.ID, tooMany); return err }, CodeTooMany},
		{"webmail user", func() error {
			_, err := service.ListGroups(ctx, Actor{UserID: "milo-rivers", SessionID: "milo-session"})
			return err
		}, CodeForbidden},
	} {
		if err := test.err(); CodeOf(err) != test.want {
			t.Errorf("%s: %v (%s), want %s", test.name, err, CodeOf(err), test.want)
		}
	}
	groups, err := service.GroupsOf(ctx, actor, "milo-rivers")
	if err != nil || len(groups) != 1 || groups[0].Name != "Field team" || groups[0].MemberCount != 1 {
		t.Fatalf("Milo's groups = %+v, %v", groups, err)
	}
}
