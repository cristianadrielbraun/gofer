package mail

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserIDLEDiagnosticsKeepsAdministratorAndOwnerScopes(t *testing.T) {
	f := newUserGmailFixture(t)
	if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('administrator','administrator','administrator','management',1); UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	// Isolate the runtime snapshot from provider scheduling: these statuses use
	// overlapping folder IDs, as they can in distinct owner databases.
	s := &UserIMAP{accounts: f.accounts, watches: map[userIMAPWatchKey]*userIMAPWatch{
		{account: f.ids["alice"], folder: "same-folder"}: {owner: "alice", status: IDLEFolderRuntimeStatus{AccountID: f.ids["alice"], FolderID: "same-folder", Healthy: true}},
		{account: f.ids["bob"], folder: "same-folder"}:   {owner: "bob", status: IDLEFolderRuntimeStatus{AccountID: f.ids["bob"], FolderID: "same-folder", Reason: "polling fallback"}},
	}}
	actor := storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}
	for _, owner := range []string{"alice", "bob"} {
		states, err := s.IdleStatusesForDiagnostics(t.Context(), actor, owner)
		if err != nil || len(states) != 1 || states[0].AccountID != f.ids[owner] || states[0].Healthy != (owner == "alice") {
			t.Fatal("IDLE diagnostic ownership", owner, states, err)
		}
	}
	if _, err := s.IdleStatusesForUser(t.Context(), "bob"); err == nil {
		t.Fatal("administrator diagnostic access enabled disabled private access")
	}
	if _, err := s.IdleStatusesForDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "alice", AuthVersion: 1}, "bob"); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("webmail owner admitted to IDLE diagnostics", err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET auth_version=2 WHERE id='administrator'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IdleStatusesForDiagnostics(t.Context(), actor, "alice"); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("old administrator snapshot survived version change", err)
	}
}
