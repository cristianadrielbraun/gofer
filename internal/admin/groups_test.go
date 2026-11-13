package admin

import (
	"fmt"
	"strings"
	"testing"
	"time"
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

func TestBulkGroupChangesEveryoneItAppliesToAtOnce(t *testing.T) {
	service, db, actor := newTestService(t, nil)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, id := range []string{"noor-hale", "ines-moreau"} {
		if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, status, auth_version, user_type, is_admin, created_at, updated_at)
			VALUES (?, ?, ?, 'active', 1, 'webmail', 0, ?, ?)`, id, id, id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Write().Exec(`UPDATE users SET status = 'disabled', deletion_pending = 1 WHERE id = 'ines-moreau'`); err != nil {
		t.Fatal(err)
	}
	field, err := service.CreateGroup(ctx, actor, "Field team", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AddToGroup(ctx, actor, field.ID, []string{"milo-rivers"}); err != nil {
		t.Fatal(err)
	}

	result, err := service.BulkGroup(ctx, actor, BulkAddToGroup, field.ID, []string{"milo-rivers", "noor-hale", "head-admin", "ines-moreau", "gone-person"})
	if err != nil || result.Group != "Field team" {
		t.Fatalf("BulkGroup = %+v, %v", result, err)
	}
	want := map[string]Reason{"milo-rivers": ReasonUnchanged, "noor-hale": "", "head-admin": ReasonAdministrator, "ines-moreau": ReasonDeleting, "gone-person": ReasonGone}
	for _, outcome := range result.Outcomes {
		if outcome.Reason != want[outcome.UserID] || outcome.Changed != (outcome.UserID == "noor-hale") {
			t.Errorf("%s: %+v", outcome.UserID, outcome)
		}
	}
	if result.Changed() != 1 {
		t.Fatalf("changed = %d", result.Changed())
	}
	var records int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'group.members_added'`).Scan(&records); err != nil || records != 2 {
		t.Fatalf("addition records = %d, %v", records, err)
	}

	removed, err := service.BulkGroup(ctx, actor, BulkRemoveFromGroup, field.ID, []string{"milo-rivers", "noor-hale"})
	if err != nil || removed.Changed() != 2 {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	// Nobody it applies to still names the group.
	nobody, err := service.BulkGroup(ctx, actor, BulkRemoveFromGroup, field.ID, []string{"head-admin"})
	if err != nil || nobody.Changed() != 0 || nobody.Group != "Field team" {
		t.Fatalf("remove nobody eligible = %+v, %v", nobody, err)
	}

	for _, test := range []struct {
		name string
		err  func() error
		want Code
	}{
		{"no group", func() error {
			_, err := service.BulkGroup(ctx, actor, BulkAddToGroup, " ", []string{"noor-hale"})
			return err
		}, CodeInvalid},
		{"missing group", func() error {
			_, err := service.BulkGroup(ctx, actor, BulkAddToGroup, "no-such-group", []string{"noor-hale"})
			return err
		}, CodeUnavailable},
		{"group change without its group", func() error { _, err := service.Bulk(ctx, actor, BulkAddToGroup, []string{"noor-hale"}); return err }, CodeInvalid},
		{"not a group change", func() error {
			_, err := service.BulkGroup(ctx, actor, BulkDisable, field.ID, []string{"noor-hale"})
			return err
		}, CodeInvalid},
	} {
		if err := test.err(); CodeOf(err) != test.want {
			t.Errorf("%s: %v (%s), want %s", test.name, err, CodeOf(err), test.want)
		}
	}
}
