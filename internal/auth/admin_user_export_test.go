package auth

import (
	"errors"
	"testing"
	"time"
)

func TestExportAdministratorUsersRecordsTheExportFirst(t *testing.T) {
	now := time.Date(2026, time.November, 12, 9, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	for _, id := range []string{"marlow", "marisol", "teodora", "ordinary"} {
		insertActiveUser(t, manager, id, false, now)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'disabled' WHERE id = 'marisol'`); err != nil {
		t.Fatal(err)
	}
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySession, err := manager.CreateAuthenticatedSession(t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}

	// Paging and exact IDs don't narrow an export: it's the whole list the
	// search and filters keep.
	query := AdministratorUserQuery{Search: "MAR", Sort: AdministratorUserSortUsername, Offset: 1, Limit: 1, UserIDs: []string{"teodora"}}
	people, err := manager.ExportAdministratorUsers(t.Context(), "administrator", adminSession.ID, query)
	if err != nil || len(people) != 2 || people[0].ID != "marisol" || people[1].ID != "marlow" {
		t.Fatalf("ExportAdministratorUsers() = %+v, %v", people, err)
	}
	for _, person := range people {
		if person.InvitationActionReference != "" {
			t.Fatalf("an export carries an invitation action reference: %+v", person)
		}
	}
	records := auditRecords(t, manager)
	if len(records) != 1 || records[0].Action != "user.list_exported" || records[0].Target.Kind != "instance" ||
		records[0].Metadata["count"] != float64(2) || records[0].Metadata["search"] != "MAR" ||
		records[0].Metadata["sort"] != "username" || records[0].Metadata["format"] != "csv" {
		t.Fatalf("export audit records = %+v", records)
	}
	if _, ok := records[0].Metadata["status"]; ok {
		t.Fatalf("an export without a status filter recorded one: %+v", records[0].Metadata)
	}

	// Without the permission or a recent verification, nobody is handed
	// over and nothing is exported.
	if people, err := manager.ExportAdministratorUsers(t.Context(), "ordinary", ordinarySession.ID, AdministratorUserQuery{}); people != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary export = %+v, %v", people, err)
	}
	if people, err := manager.ExportAdministratorUsers(t.Context(), "administrator", staleAdmin.ID, AdministratorUserQuery{}); people != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale export = %+v, %v", people, err)
	}
	for _, record := range auditRecords(t, manager) {
		if record.Action == "user.list_exported" && record.ID != records[0].ID {
			t.Fatalf("a refused export was recorded: %+v", record)
		}
	}
}
