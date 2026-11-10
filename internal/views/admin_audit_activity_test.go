package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

func renderAdminAuditActivity(t *testing.T, data AdminAuditActivityData, verification ...AdminSecurityVerificationData) string {
	t.Helper()
	var out bytes.Buffer
	if err := AdminAuditActivityPage(data, verification...).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestAdminAuditActivityReadsEachChangeAsASentence(t *testing.T) {
	at := time.Date(2026, time.November, 10, 9, 30, 0, 0, time.UTC)
	admin := audit.Actor{Kind: audit.ActorAdmin, ID: "admin-1", Label: "ada.admin"}
	data := AdminAuditActivityData{
		ManagedMode: true, Kind: "user", Before: 90, Next: 40,
		Records: []audit.Record{
			{Seq: 89, OccurredAt: at, Event: audit.Event{Action: "user.disabled", Actor: admin, Target: audit.Target{Kind: "user", ID: "u-1", Label: "milo.rivers"}, Client: audit.Client{UserAgent: "Sample Browser 1.0"}}},
			{Seq: 88, OccurredAt: at, Event: audit.Event{
				Action: "instance.security_retention_changed", Actor: admin, Target: audit.Target{Kind: "instance"},
				Changes: map[string]audit.Change{"security_event_retention_days": {From: float64(180), To: float64(90)}},
			}},
			{Seq: 87, OccurredAt: at, Event: audit.Event{Action: "audit.pruned", Actor: audit.Actor{Kind: audit.ActorSystem}, Metadata: map[string]any{"deleted": float64(12), "older_than": at.Format(time.RFC3339Nano)}}},
			{Seq: 86, OccurredAt: at, Event: audit.Event{Action: "security.proxy_policy_changed", Actor: admin, Metadata: map[string]any{"people_may_add_proxies": true}}},
			{Seq: 85, OccurredAt: at, Event: audit.Event{Action: "user.mfa_required", Actor: admin, Target: audit.Target{Label: "nora.vale"}, Outcome: audit.OutcomeDenied}},
			{Seq: 84, OccurredAt: at, Event: audit.Event{Action: "group.member_added", Actor: admin}},
		},
	}
	html := renderAdminAuditActivity(t, data)
	requireAdminUsersHTML(t, "administration tab", html,
		`data-admin-audit-activity`, "Sign-in and security", `href="/admin/activity/administration" data-admin-navigation-link data-admin-navigation-label="Administration" aria-current="page"`,
		"Disabled milo.rivers", "ada.admin", "Sample Browser 1.0",
		"Kept sign-in and security events for 90 days instead of 180",
		"Removed 12 administrative changes from before", ">Gofer<",
		"Let people add proxies of their own",
		"Required 2FA for nora.vale", "Refused", "Done",
		// An action Gofer has no words for still shows, as itself.
		"group.member_added",
		`href="/admin/activity/administration?kind=user" data-admin-navigation-link`, "People",
		`href="/admin/activity/administration?kind=user"`, "Newest changes",
		`href="/admin/activity/administration?before=40&amp;kind=user"`, "Older changes",
	)
}

func TestAdminAuditActivityStates(t *testing.T) {
	unmanaged := renderAdminAuditActivity(t, AdminAuditActivityData{})
	requireAdminUsersHTML(t, "unmanaged tab", unmanaged, `role="status"`, "Managed authentication is not enabled")
	if strings.Contains(unmanaged, "Sign-in and security") || strings.Contains(unmanaged, "<table") {
		t.Fatal("unmanaged tab offered the records")
	}
	empty := renderAdminAuditActivity(t, AdminAuditActivityData{ManagedMode: true})
	requireAdminUsersHTML(t, "empty tab", empty, "No administrative changes yet.")
	filtered := renderAdminAuditActivity(t, AdminAuditActivityData{ManagedMode: true, Kind: "security"})
	requireAdminUsersHTML(t, "empty filter", filtered, "No changes of this kind here.")

	verification := AdminSecurityVerificationData{Required: true, HasTOTP: true, ReturnTo: AdminAuditActivityPath, TOTPPath: "/settings/security/step-up", TOTPCSRFToken: strings.Repeat("c", 64)}
	locked := renderAdminAuditActivity(t, AdminAuditActivityData{ManagedMode: true, StepUpRequired: true}, verification)
	requireAdminUsersHTML(t, "locked tab", locked,
		`data-admin-audit-activity-locked`, "see administrative changes for the next ten minutes",
		`name="return_to" value="/admin/activity/administration"`,
	)
	if strings.Contains(locked, "<table") {
		t.Fatal("a stale session saw administrative changes")
	}
}

// Every action Gofer records reads as a sentence, in every state it can
// be in.
func TestEveryRecordedActionHasASentence(t *testing.T) {
	ctx := context.Background()
	for _, record := range []audit.Record{
		{Event: audit.Event{Action: "user.invited"}},
		{Event: audit.Event{Action: "invitation.revoked"}},
		{Event: audit.Event{Action: "invitation.rotated"}},
		{Event: audit.Event{Action: "invitation.issued"}},
		{Event: audit.Event{Action: "user.disabled"}},
		{Event: audit.Event{Action: "user.enabled"}},
		{Event: audit.Event{Action: "user.mfa_required"}},
		{Event: audit.Event{Action: "user.mfa_cleared"}},
		{Event: audit.Event{Action: "user.credential_reset_issued"}},
		{Event: audit.Event{Action: "user.credential_reset_revoked"}},
		{Event: audit.Event{Action: "user.password_change_required"}},
		{Event: audit.Event{Action: "user.deletion_started"}},
		{Event: audit.Event{Action: "user.deleted"}},
		{Event: audit.Event{Action: "instance.security_retention_changed"}},
		{Event: audit.Event{Action: "instance.mfa_policy_changed", Changes: map[string]audit.Change{"mfa_policy": {From: "administrators", To: "all_users"}}}},
		{Event: audit.Event{Action: "audit.pruned"}},
		{Event: audit.Event{Action: "security.exception_added", Metadata: map[string]any{"kind": "http_discovery"}}},
		{Event: audit.Event{Action: "security.exception_removed", Metadata: map[string]any{"kind": "private_target"}}},
		{Event: audit.Event{Action: "security.allowlist_entry_added"}},
		{Event: audit.Event{Action: "security.allowlist_entry_removed"}},
		{Event: audit.Event{Action: "security.allowlist_provider_changed", Metadata: map[string]any{"allowed": true}}},
		{Event: audit.Event{Action: "security.allowlist_provider_changed", Metadata: map[string]any{"allowed": false}}},
		{Event: audit.Event{Action: "security.allowlist_exemption_changed", Metadata: map[string]any{"exempt": true}}},
		{Event: audit.Event{Action: "security.allowlist_exemption_changed", Metadata: map[string]any{"exempt": false}}},
		{Event: audit.Event{Action: "security.proxy_added"}},
		{Event: audit.Event{Action: "security.proxy_changed"}},
		{Event: audit.Event{Action: "security.proxy_deleted"}},
		{Event: audit.Event{Action: "security.proxy_everyone_changed", Metadata: map[string]any{"everyone": true}}},
		{Event: audit.Event{Action: "security.proxy_everyone_changed", Metadata: map[string]any{"everyone": false}}},
		{Event: audit.Event{Action: "security.proxy_person_changed", Metadata: map[string]any{"chosen": true}}},
		{Event: audit.Event{Action: "security.proxy_person_changed", Metadata: map[string]any{"chosen": false}}},
		{Event: audit.Event{Action: "security.proxy_policy_changed", Metadata: map[string]any{"people_may_add_proxies": true}}},
		{Event: audit.Event{Action: "security.proxy_policy_changed", Metadata: map[string]any{"people_may_add_proxies": false}}},
	} {
		sentence := adminAuditSentence(ctx, record)
		if sentence == record.Action || strings.Contains(sentence, "admin.audit.") || strings.Contains(sentence, "{") {
			t.Errorf("%s (%v) reads %q", record.Action, record.Metadata, sentence)
		}
	}
	if got := adminAuditSentence(ctx, audit.Record{Event: audit.Event{Action: "instance.mfa_policy_changed", Changes: map[string]audit.Change{"mfa_policy": {From: "administrators", To: "all_users"}}}}); got != "Required 2FA for everyone instead of administrators" {
		t.Errorf("policy sentence = %q", got)
	}
}
