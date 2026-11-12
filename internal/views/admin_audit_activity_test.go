package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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
	if tab := openingTag(html, `href="/admin/activity/administration"`); !strings.Contains(tab, `aria-current="page"`) || !strings.Contains(tab, "data-admin-navigation-link") {
		t.Fatalf("the Administration tab isn't the current one: %q", tab)
	}
	requireAdminUsersHTML(t, "administration tab", html,
		`data-admin-audit-activity`, "Sign-in and security",
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
		{Event: audit.Event{Action: "user.signed_out", Metadata: map[string]any{"revoked_sessions": 2}}},
		{Event: audit.Event{Action: "user.deletion_started"}},
		{Event: audit.Event{Action: "user.deleted"}},
		{Event: audit.Event{Action: "instance.security_retention_changed"}},
		{Event: audit.Event{Action: "instance.mfa_policy_changed", Changes: map[string]audit.Change{"mfa_policy": {From: "administrators", To: "all_users"}}}},
		{Event: audit.Event{Action: "audit.pruned"}},
		{Event: audit.Event{Action: "audit.exported", Metadata: map[string]any{"format": "csv"}}},
		{Event: audit.Event{Action: "user.list_exported", Metadata: map[string]any{"format": "csv", "count": float64(12)}}},
		{Event: audit.Event{Action: "instance.audit_retention_changed"}},
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
		{Event: audit.Event{Action: "access.denied", Target: audit.Target{Kind: "permission", ID: "users.delete"}, Metadata: map[string]any{"reason": "verification_required"}}},
		{Event: audit.Event{Action: "access.denied", Target: audit.Target{Kind: "permission", ID: "users.delete"}, Metadata: map[string]any{"reason": "forbidden"}}},
	} {
		sentence := adminAuditSentence(ctx, record)
		if sentence == record.Action || strings.Contains(sentence, "admin.audit.") || strings.Contains(sentence, "{") {
			t.Errorf("%s (%v) reads %q", record.Action, record.Metadata, sentence)
		}
	}
	if got := adminAuditSentence(ctx, audit.Record{Event: audit.Event{Action: "instance.mfa_policy_changed", Changes: map[string]audit.Change{"mfa_policy": {From: "administrators", To: "all_users"}}}}); got != "Required 2FA for everyone instead of administrators" {
		t.Errorf("policy sentence = %q", got)
	}
	if got := adminAuditSentence(ctx, audit.Record{Event: audit.Event{Action: "user.list_exported", Metadata: map[string]any{"count": float64(1)}}}); got != "Exported the Users list as CSV, 1 person" {
		t.Errorf("export sentence = %q", got)
	}
	if got := adminAuditSentence(ctx, audit.Record{Event: audit.Event{Action: "access.denied", Target: audit.Target{ID: "users.delete"}, Metadata: map[string]any{"reason": "verification_required"}}}); got != "Refused without a recent verification: deleting people" {
		t.Errorf("refusal sentence = %q", got)
	}
}

// A refusal names the permission it refused, so each has words.
func TestEveryPermissionHasWords(t *testing.T) {
	for _, permission := range authz.All() {
		if !i18n.Has("admin.audit.permission." + string(permission)) {
			t.Errorf("permission %s has no words for refusals", permission)
		}
	}
}

func TestAdminAuditActivityOffersExportAndRetention(t *testing.T) {
	html := renderAdminAuditActivity(t, AdminAuditActivityData{
		ManagedMode: true, Kind: "security", RetentionDays: 730, RetentionMinimum: 90, RetentionMaximum: 3650,
		RetentionCSRFToken: strings.Repeat("r", 64), RetentionOpen: true,
	})
	requireAdminUsersHTML(t, "administration tab", html,
		`href="/admin/activity/administration/export?format=csv&amp;kind=security"`, "Export CSV",
		`href="/admin/activity/administration/export?format=jsonl&amp;kind=security"`, "Export JSON Lines",
		`data-tui-dialog-target="admin-audit-retention-dialog"`, `data-admin-audit-retention`,
		`action="/admin/activity/administration/retention"`, strings.Repeat("r", 64),
		`id="admin-audit-retention-days"`, `min="90"`, `max="3650"`, `value="730"`,
		"Administrative changes are kept for 730 days.",
	)
	if !strings.Contains(html, `id="admin-audit-retention-dialog" data-tui-dialog data-tui-dialog-open="true"`) {
		t.Fatal("a refused retention didn't reopen its dialog")
	}
	locked := renderAdminAuditActivity(t, AdminAuditActivityData{ManagedMode: true, StepUpRequired: true})
	if strings.Contains(locked, "/export?") || strings.Contains(locked, "admin-audit-retention-dialog") {
		t.Fatal("a stale session was offered the export or the settings")
	}
}

// Records name the browser as the sign-in tab does, keeping the whole
// user agent in a tooltip; Gofer's own changes have none.
func TestAdminAuditRowNamesTheBrowser(t *testing.T) {
	agent := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	html := renderAdminAuditActivity(t, AdminAuditActivityData{ManagedMode: true, Records: []audit.Record{
		{Seq: 2, Event: audit.Event{Action: "user.disabled", Actor: audit.Actor{Kind: audit.ActorAdmin, Label: "ada.admin"}, Client: audit.Client{UserAgent: agent}}},
		{Seq: 1, Event: audit.Event{Action: "audit.pruned", Actor: audit.Actor{Kind: audit.ActorSystem}}},
	}})
	requireAdminUsersHTML(t, "browser", html, ">Chrome on Linux<", `title="`+agent+`"`, "—")
}

// openingTag is the opening tag in html that holds attribute.
func openingTag(html, attribute string) string {
	at := strings.Index(html, attribute)
	if at < 0 {
		return ""
	}
	start := strings.LastIndex(html[:at], "<")
	end := strings.Index(html[at:], ">")
	if start < 0 || end < 0 {
		return ""
	}
	return html[start : at+end+1]
}
