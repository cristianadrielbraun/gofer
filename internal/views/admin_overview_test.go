package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

func renderAdminOverview(t *testing.T, data AdminOverviewData) string {
	t.Helper()
	var out bytes.Buffer
	if err := AdminOverviewPage(data).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestAdminOverviewShowsCountsAttentionAndRecentChanges(t *testing.T) {
	html := renderAdminOverview(t, AdminOverviewData{
		Total: 7, Active: 4, Pending: 2, Disabled: 1, Administrators: 1,
		Attention: []AdminOverviewItem{
			{Key: "reset_requests", Count: 1, Href: "/admin/users"},
			{Key: "mfa_setup", Count: 3, Href: "/admin/users"},
			{Key: "expired_invitations", Count: 2, Href: "/admin/users"},
			{Key: "refused", Count: 2, Href: "/admin/activity/administration?kind=access"},
		},
		Recent: []audit.Record{{Seq: 3, OccurredAt: time.Date(2026, time.November, 10, 9, 0, 0, 0, time.UTC), Event: audit.Event{
			Action: "user.disabled", Actor: audit.Actor{Kind: audit.ActorAdmin, Label: "ada.admin"}, Target: audit.Target{Label: "milo.rivers"},
		}}},
	})
	requireAdminUsersHTML(t, "overview", html,
		`data-admin-overview`, "Overview", ">4<", ">2<", ">1<",
		"1 person asked for a password reset", "2 invitations expired before they were used",
		"3 people still have to set up two-factor authentication", "2 refused attempts in the last 7 days",
		`href="/admin/activity/administration?kind=access"`,
		`data-admin-overview-item="reset_requests"`, `href="/admin/users"`,
		"Latest administrative changes", "Disabled milo.rivers", "ada.admin",
		`href="/admin/activity/administration"`,
	)
	if strings.Contains(html, "Nothing needs an administrator") {
		t.Fatal("overview said nothing needs attention while something does")
	}
}

func TestAdminOverviewQuietAndLockedStates(t *testing.T) {
	html := renderAdminOverview(t, AdminOverviewData{RecentLocked: true})
	requireAdminUsersHTML(t, "quiet overview", html, "Nothing needs an administrator right now.", "Verify again on the Activity page")
	if strings.Contains(html, "<table") {
		t.Fatal("a stale session saw administrative changes on the overview")
	}
	empty := renderAdminOverview(t, AdminOverviewData{})
	requireAdminUsersHTML(t, "empty overview", empty, "No administrative changes yet.")
}
