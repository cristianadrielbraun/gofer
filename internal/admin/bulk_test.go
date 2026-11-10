package admin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestBulkReason(t *testing.T) {
	active := auth.AdministratorUserSummary{ID: "milo-rivers", Status: auth.UserStatusActive, UserType: auth.UserTypeWebmail, HasPassword: true}
	with := func(change func(*auth.AdministratorUserSummary)) auth.AdministratorUserSummary {
		user := active
		change(&user)
		return user
	}
	disabled := with(func(u *auth.AdministratorUserSummary) { u.Status = auth.UserStatusDisabled })
	pending := with(func(u *auth.AdministratorUserSummary) { u.Status, u.HasPassword = auth.UserStatusPending, false })
	deleting := with(func(u *auth.AdministratorUserSummary) { u.Status, u.DeletionPending = auth.UserStatusDisabled, true })
	required := with(func(u *auth.AdministratorUserSummary) { u.MFARequired = true })
	noPassword := with(func(u *auth.AdministratorUserSummary) { u.HasPassword = false })
	mustChange := with(func(u *auth.AdministratorUserSummary) { u.PasswordChangeRequired = true })
	administrator := auth.AdministratorUserSummary{ID: "ada-admin", Status: auth.UserStatusActive, UserType: auth.UserTypeManagement, IsAdmin: true, HasPassword: true}
	self := administrator
	self.ID = "head-admin"
	everyone := auth.InstanceMFAPolicyAllUsers
	administrators := auth.InstanceMFAPolicyAdministrators

	for _, test := range []struct {
		action BulkAction
		user   auth.AdministratorUserSummary
		policy auth.InstanceMFAPolicy
		want   Reason
	}{
		{BulkDisable, active, administrators, ""},
		{BulkDisable, disabled, administrators, ReasonUnchanged},
		{BulkDisable, pending, administrators, ReasonPending},
		{BulkDisable, deleting, administrators, ReasonDeleting},
		{BulkDisable, self, administrators, ReasonYou},
		{BulkDisable, administrator, administrators, ""},
		{BulkEnable, disabled, administrators, ""},
		{BulkEnable, active, administrators, ReasonUnchanged},
		{BulkEnable, self, administrators, ReasonYou},
		{BulkRequireMFA, active, administrators, ""},
		{BulkRequireMFA, pending, administrators, ""},
		{BulkRequireMFA, required, administrators, ReasonUnchanged},
		{BulkRequireMFA, active, everyone, ReasonEveryoneRequired},
		{BulkRequireMFA, administrator, administrators, ReasonAdministrator},
		{BulkRequireMFA, self, administrators, ReasonAdministrator},
		{BulkRequireMFA, deleting, administrators, ReasonDeleting},
		{BulkClearMFA, required, everyone, ""},
		{BulkClearMFA, active, administrators, ReasonUnchanged},
		{BulkRequirePasswordChange, active, administrators, ""},
		{BulkRequirePasswordChange, disabled, administrators, ""},
		{BulkRequirePasswordChange, mustChange, administrators, ReasonUnchanged},
		{BulkRequirePasswordChange, noPassword, administrators, ReasonNoPassword},
		{BulkRequirePasswordChange, pending, administrators, ReasonPending},
		{BulkRequirePasswordChange, administrator, administrators, ReasonAdministrator},
		{BulkSignOut, active, administrators, ""},
		{BulkSignOut, administrator, administrators, ""},
		{BulkSignOut, disabled, administrators, ReasonUnchanged},
		{BulkSignOut, pending, administrators, ReasonPending},
		{BulkSignOut, self, administrators, ReasonYou},
		{BulkAction("promote"), active, administrators, Reason(CodeInvalid)},
	} {
		if got := BulkReason("head-admin", test.action, test.user, test.policy); got != test.want {
			t.Errorf("BulkReason(%s, %s %s) = %q, want %q", test.action, test.user.ID, test.user.Status, got, test.want)
		}
	}
}

// addPerson adds a webmail user with status, and a password when asked.
func addPerson(t *testing.T, db *storage.DB, id string, status auth.UserStatus, password bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, 'webmail', 0, ?, ?)`, id, id, id, "Sample "+id, status, now, now); err != nil {
		t.Fatal(err)
	}
	if password {
		if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO password_credentials (user_id, password_hash) VALUES (?, 'sample-password-hash')`, id); err != nil {
			t.Fatal(err)
		}
	}
}

func outcomesOf(result *BulkResult) string {
	var lines []string
	for _, outcome := range result.Outcomes {
		line := fmt.Sprintf("%s changed=%t", outcome.UserID, outcome.Changed)
		if outcome.Reason != "" {
			line += " " + string(outcome.Reason)
		}
		if outcome.Sessions != 0 {
			line += fmt.Sprintf(" sessions=%d", outcome.Sessions)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func TestBulkChangesEachPersonUnderOneCorrelation(t *testing.T) {
	hooks := &recordedHooks{}
	service, db, actor := newTestService(t, hooks)
	ctx := t.Context()
	addPerson(t, db, "nora-vale", auth.UserStatusActive, true)
	addPerson(t, db, "otto-lind", auth.UserStatusDisabled, true)

	result, err := service.Bulk(ctx, actor, BulkDisable, []string{"milo-rivers", " nora-vale ", "otto-lind", "head-admin", "missing-person", "milo-rivers"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"milo-rivers changed=true sessions=1",
		"nora-vale changed=true",
		"otto-lind changed=false unchanged",
		"head-admin changed=false you",
		"missing-person changed=false gone",
	}, "\n")
	if got := outcomesOf(result); got != want || result.Changed() != 2 || result.Correlation == "" {
		t.Fatalf("Bulk(disable) =\n%s\nchanged %d, correlation %q; want\n%s", got, result.Changed(), result.Correlation, want)
	}
	if strings.Join(hooks.calls, ",") != "milo-rivers=disabled,nora-vale=disabled" {
		t.Fatalf("hooks = %v", hooks.calls)
	}
	page, err := audit.List(context.Background(), db.Read(), audit.Filter{}, 0, audit.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("audit records = %+v", page.Records)
	}
	for _, record := range page.Records {
		if record.Action != "user.disabled" || record.CorrelationID != result.Correlation {
			t.Fatalf("audit record %s for %s has correlation %q, want %q", record.Action, record.Target.ID, record.CorrelationID, result.Correlation)
		}
	}

	// Another bulk change is a change of its own.
	enabled, err := service.Bulk(ctx, actor, BulkEnable, []string{"milo-rivers"})
	if err != nil || enabled.Changed() != 1 || enabled.Correlation == result.Correlation {
		t.Fatalf("Bulk(enable) = %+v, %v", enabled, err)
	}
}

func TestBulkSignsOutAndAsksForNewPasswords(t *testing.T) {
	service, db, actor := newTestService(t, nil)
	ctx := t.Context()
	addPerson(t, db, "nora-vale", auth.UserStatusActive, true)

	signedOut, err := service.Bulk(ctx, actor, BulkSignOut, []string{"milo-rivers", "nora-vale"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomesOf(signedOut), "milo-rivers changed=true sessions=1\nnora-vale changed=false unchanged"; got != want {
		t.Fatalf("Bulk(sign-out) =\n%s\nwant\n%s", got, want)
	}

	asked, err := service.Bulk(ctx, actor, BulkRequirePasswordChange, []string{"milo-rivers", "nora-vale"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomesOf(asked), "milo-rivers changed=false no_password\nnora-vale changed=true"; got != want {
		t.Fatalf("Bulk(require-password-change) =\n%s\nwant\n%s", got, want)
	}
	again, err := service.Bulk(ctx, actor, BulkRequirePasswordChange, []string{"nora-vale"})
	if err != nil || outcomesOf(again) != "nora-vale changed=false unchanged" {
		t.Fatalf("Bulk(require-password-change again) = %s, %v", outcomesOf(again), err)
	}

	required, err := service.Bulk(ctx, actor, BulkRequireMFA, []string{"milo-rivers", "head-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomesOf(required), "milo-rivers changed=true\nhead-admin changed=false administrator"; got != want {
		t.Fatalf("Bulk(require-mfa) =\n%s\nwant\n%s", got, want)
	}
}

func TestBulkRefusesWholeRequestsItCannotMake(t *testing.T) {
	service, db, actor := newTestService(t, nil)
	ctx := t.Context()
	tooMany := make([]string, MaxBulkPeople+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("person-%03d", i)
	}
	for _, test := range []struct {
		name   string
		action BulkAction
		ids    []string
		want   Code
	}{
		{"unknown change", BulkAction("promote"), []string{"milo-rivers"}, CodeInvalid},
		{"nobody", BulkDisable, []string{" ", ""}, CodeInvalid},
		{"too many", BulkDisable, tooMany, CodeTooMany},
	} {
		if result, err := service.Bulk(ctx, actor, test.action, test.ids); result != nil || CodeOf(err) != test.want {
			t.Errorf("%s: Bulk = %+v, %v (%s), want %s", test.name, result, err, CodeOf(err), test.want)
		}
	}
	// The same person named many times is one person.
	repeated := make([]string, MaxBulkPeople+1)
	for i := range repeated {
		repeated[i] = "milo-rivers"
	}
	if result, err := service.Bulk(ctx, actor, BulkSignOut, repeated); err != nil || len(result.Outcomes) != 1 {
		t.Fatalf("Bulk(one person repeated) = %+v, %v", result, err)
	}
	// A session that must verify again changes no one.
	if _, err := db.Write().ExecContext(ctx, `UPDATE sessions SET step_up_at = ? WHERE id = 'admin-session'`, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if result, err := service.Bulk(ctx, actor, BulkDisable, []string{"milo-rivers"}); result != nil || CodeOf(err) != CodeVerificationRequired {
		t.Fatalf("stale session = %+v, %v", result, err)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM users WHERE id = 'milo-rivers'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("milo-rivers = %q, %v", status, err)
	}
}
