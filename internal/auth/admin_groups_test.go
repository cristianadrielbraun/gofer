package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/directory"
)

func newGroupsManager(t *testing.T) (*Manager, *Session) {
	t.Helper()
	now := time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	for _, person := range []string{"ines", "pavel", "noor"} {
		insertActiveUser(t, manager, person, false, now)
	}
	return manager, createPolicyAdministratorSession(t, manager)
}

func groupAuditRecords(t *testing.T, manager *Manager) []audit.Record {
	t.Helper()
	page, err := audit.List(t.Context(), manager.db.Read(), audit.Filter{Action: "group."}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return page.Records
}

func TestGroupsAreChangedOnlyByAdministratorsAndRecorded(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	if _, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "ines", ActorSessionID: "s", Name: "Field team"}); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("webmail user creates a group = %v", err)
	}
	unverified := createPolicyAdministratorSession(t, manager)
	if _, err := manager.db.Write().Exec(`UPDATE sessions SET step_up_at = NULL, step_up_method = '' WHERE id = ?`, unverified.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: unverified.ID, Name: "Field team"}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("create without a recent verification = %v", err)
	}
	if _, err := manager.ListGroups(ctx, "ines"); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("webmail user lists groups = %v", err)
	}
	// Reading needs no recent verification.
	if groups, err := manager.ListGroups(ctx, "administrator"); err != nil || len(groups) != 0 {
		t.Fatalf("groups = %#v, %v", groups, err)
	}

	field, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: " Field team ", Description: "On the road"})
	if err != nil || field.Name != "Field team" || field.CreatedBy != "administrator" || field.ID == "" {
		t.Fatalf("created = %#v, %v", field, err)
	}
	if _, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "FIELD TEAM"}); !errors.Is(err, directory.ErrNameTaken) {
		t.Fatalf("same name = %v", err)
	}
	if _, err := manager.UpdateGroup(ctx, UpdateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID, Version: 1, Name: "Field team", Description: "On the road"}); err != nil {
		t.Fatalf("change nothing = %v", err)
	}
	if _, err := manager.UpdateGroup(ctx, UpdateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID, Version: 1, Name: "Field team", Description: "On the road most days"}); err != nil {
		t.Fatalf("describe = %v", err)
	}
	renamed, err := manager.UpdateGroup(ctx, UpdateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID, Version: 2, Name: "Field crew", Description: "On the road most days"})
	if err != nil || renamed.Name != "Field crew" || renamed.Version != 3 {
		t.Fatalf("rename = %#v, %v", renamed, err)
	}
	if _, err := manager.UpdateGroup(ctx, UpdateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID, Version: 2, Name: "Field people"}); !errors.Is(err, directory.ErrStale) {
		t.Fatalf("rename from an old page = %v", err)
	}

	records := groupAuditRecords(t, manager)
	// Newest first.
	if len(records) != 3 || records[2].Action != "group.created" || records[1].Action != "group.updated" || records[0].Action != "group.renamed" {
		t.Fatalf("group records = %#v", records)
	}
	if records[0].Target.Label != "Field crew" || records[0].Actor.Label != "administrator" ||
		records[0].Changes["name"].From != "Field team" || records[0].Changes["name"].To != "Field crew" {
		t.Fatalf("rename record = %#v", records[0])
	}
	if records[1].Changes["description"].To != "On the road most days" {
		t.Fatalf("description record = %#v", records[1])
	}
}

func TestGroupMembersAreWebmailUsersNotBeingDeleted(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	if _, err := manager.db.Write().Exec(`UPDATE users SET status = 'disabled', deletion_pending = 1 WHERE id = 'noor'`); err != nil {
		t.Fatal(err)
	}
	field, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Field team"})
	if err != nil {
		t.Fatal(err)
	}
	options := ChangeGroupMembersOptions{ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID}
	for _, refused := range [][]string{{"ines", "noor"}, {"administrator"}, {"nobody"}} {
		options.UserIDs = refused
		if _, err := manager.AddGroupMembers(ctx, options); !errors.Is(err, ErrGroupMemberInvalid) {
			t.Fatalf("add %v = %v", refused, err)
		}
	}
	if groups, err := manager.GroupsOfUser(ctx, "administrator", "ines"); err != nil || len(groups) != 0 {
		t.Fatalf("a refused change added Ines: %#v, %v", groups, err)
	}

	options.UserIDs = []string{"pavel", "ines", "pavel", ""}
	added, err := manager.AddGroupMembers(ctx, options)
	if err != nil || len(added.Changed) != 2 || added.Changed[0] != "pavel" || added.Group.MemberCount != 2 {
		t.Fatalf("added = %#v, %v", added, err)
	}
	if again, err := manager.AddGroupMembers(ctx, options); err != nil || len(again.Changed) != 0 {
		t.Fatalf("added again = %#v, %v", again, err)
	}
	detail, err := manager.GetGroup(ctx, "administrator", field.ID)
	if err != nil || len(detail.Members) != 2 || detail.Members[0].Username != "ines" || detail.Members[0].AddedByUsername != "administrator" ||
		detail.Members[0].Status != UserStatusActive {
		t.Fatalf("detail = %#v, %v", detail, err)
	}

	options.UserIDs = []string{"ines", "noor"}
	removed, err := manager.RemoveGroupMembers(ctx, options)
	if err != nil || len(removed.Changed) != 1 || removed.Changed[0] != "ines" || removed.Group.MemberCount != 1 {
		t.Fatalf("removed = %#v, %v", removed, err)
	}

	preview, err := manager.PreviewGroupDeletion(ctx, "administrator", field.ID)
	if err != nil || preview.Group.MemberCount != 1 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	if _, err := manager.DeleteGroup(ctx, "administrator", session.ID, field.ID, 7); !errors.Is(err, directory.ErrStale) {
		t.Fatalf("delete from an old page = %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO policies (id, name, name_normalized, priority, created_at, updated_at) VALUES ('p-field', 'Field rules', 'field rules', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at) VALUES ('p-field', 'group', '` + field.ID + `', CURRENT_TIMESTAMP)`,
	} {
		if _, err := manager.db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if members, err := manager.DeleteGroup(ctx, "administrator", session.ID, field.ID, preview.Group.Version); err != nil || members != 1 {
		t.Fatalf("delete = %d, %v", members, err)
	}
	var assignments, policies int
	if err := manager.db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM policy_assignments), (SELECT COUNT(*) FROM policies)`).Scan(&assignments, &policies); err != nil || assignments != 0 || policies != 1 {
		t.Fatalf("after deleting the group: assignments %d, policies %d, %v", assignments, policies, err)
	}
	if _, err := manager.GetGroup(ctx, "administrator", field.ID); !errors.Is(err, directory.ErrNotFound) {
		t.Fatalf("deleted group = %v", err)
	}

	records := groupAuditRecords(t, manager)
	if len(records) != 4 {
		t.Fatalf("group records = %#v", records)
	}
	deleted, removedRecord, addedRecord := records[0], records[1], records[2]
	if deleted.Action != "group.deleted" || deleted.Target.Label != "Field team" || deleted.Metadata["members"] != float64(1) {
		t.Fatalf("deletion record = %#v", deleted)
	}
	if removedRecord.Action != "group.members_removed" || removedRecord.Metadata["count"] != float64(1) {
		t.Fatalf("removal record = %#v", removedRecord)
	}
	people, _ := addedRecord.Metadata["people"].([]any)
	if addedRecord.Action != "group.members_added" || addedRecord.Metadata["count"] != float64(2) || len(people) != 2 ||
		people[0].(map[string]any)["username"] != "pavel" {
		t.Fatalf("addition record = %#v", addedRecord)
	}
}

// A change whose record can't be written doesn't happen.
func TestGroupChangesRollBackWhenTheRecordFails(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	field, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Field team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().Exec(`
		CREATE TRIGGER reject_group_records BEFORE INSERT ON audit_events
		WHEN NEW.action LIKE 'group.%'
		BEGIN SELECT RAISE(ABORT, 'reject group records'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddGroupMembers(ctx, ChangeGroupMembersOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, GroupID: field.ID, UserIDs: []string{"ines"},
	}); err == nil {
		t.Fatal("membership change without its record went through")
	}
	if _, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Night shift"}); err == nil {
		t.Fatal("group created without its record")
	}
	groups, err := manager.ListGroups(ctx, "administrator")
	if err != nil || len(groups) != 1 || groups[0].MemberCount != 0 {
		t.Fatalf("groups after the refused changes = %#v, %v", groups, err)
	}
	if revision, err := directory.Revision(ctx, manager.db.Read(), directory.RevisionMembership); err != nil || revision != 0 {
		t.Fatalf("membership revision = %d, %v", revision, err)
	}
}
