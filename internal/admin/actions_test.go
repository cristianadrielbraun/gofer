package admin

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestActionsFor(t *testing.T) {
	active := auth.AdministratorUserSummary{ID: "milo-rivers", Status: auth.UserStatusActive, UserType: auth.UserTypeWebmail, HasPassword: true}
	disabled := active
	disabled.Status = auth.UserStatusDisabled
	deleting := disabled
	deleting.DeletionPending = true
	protected := disabled
	protected.DeletionProtected = true
	pending := auth.AdministratorUserSummary{ID: "nora-vale", Status: auth.UserStatusPending, UserType: auth.UserTypeWebmail, InvitationActionReference: "sample-reference"}
	administrator := auth.AdministratorUserSummary{ID: "ada-admin", Status: auth.UserStatusActive, UserType: auth.UserTypeManagement, IsAdmin: true}
	noPassword := active
	noPassword.HasPassword = false
	required := active
	required.MFARequired = true

	for _, test := range []struct {
		name   string
		user   auth.AdministratorUserSummary
		policy auth.InstanceMFAPolicy
		want   UserActions
	}{
		{"active webmail user", active, auth.InstanceMFAPolicyAdministrators, UserActions{SetStatus: true, IssueCredentialReset: true, RequirePasswordChange: true, SetMFARequirement: true, SignOut: true}},
		{"no password to change", noPassword, auth.InstanceMFAPolicyAdministrators, UserActions{SetStatus: true, IssueCredentialReset: true, SetMFARequirement: true, SignOut: true}},
		{"disabled can be deleted", disabled, auth.InstanceMFAPolicyAdministrators, UserActions{SetStatus: true, IssueCredentialReset: true, RequirePasswordChange: true, Delete: true, SetMFARequirement: true}},
		{"a stopped deletion resumes", deleting, auth.InstanceMFAPolicyAdministrators, UserActions{Delete: true}},
		{"the instance owner stays", protected, auth.InstanceMFAPolicyAdministrators, UserActions{SetStatus: true, IssueCredentialReset: true, RequirePasswordChange: true, SetMFARequirement: true}},
		{"pending has an invitation", pending, auth.InstanceMFAPolicyAdministrators, UserActions{ManageInvitation: true, SetMFARequirement: true}},
		{"everyone needs 2FA already", active, auth.InstanceMFAPolicyAllUsers, UserActions{SetStatus: true, IssueCredentialReset: true, RequirePasswordChange: true, SignOut: true}},
		{"a requirement can be cleared under any policy", required, auth.InstanceMFAPolicyAllUsers, UserActions{SetStatus: true, IssueCredentialReset: true, RequirePasswordChange: true, SetMFARequirement: true, SignOut: true}},
		{"another administrator", administrator, auth.InstanceMFAPolicyAdministrators, UserActions{SetStatus: true, SignOut: true}},
		{"oneself", auth.AdministratorUserSummary{ID: "head-admin", Status: auth.UserStatusActive, UserType: auth.UserTypeManagement, IsAdmin: true}, auth.InstanceMFAPolicyAdministrators, UserActions{}},
	} {
		if got := ActionsFor("head-admin", test.user, test.policy); got != test.want {
			t.Errorf("%s: ActionsFor = %+v, want %+v", test.name, got, test.want)
		}
	}
}
