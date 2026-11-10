package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func renderDiagnostics(t *testing.T, render func(*bytes.Buffer) error) string {
	t.Helper()
	var out bytes.Buffer
	if err := render(&out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The owner's diagnostics are the console's, at the owner's addresses, with
// no choice of whose data to show.
func TestOwnerDiagnosticsUseTheOwnersAddresses(t *testing.T) {
	ctx := context.Background()
	owner := models.AdminWebmailScope{Owner: true}
	avatars := renderDiagnostics(t, func(out *bytes.Buffer) error {
		return OwnerDiagnosticsPartial("avatars", AdminPage(models.AvatarStatus{Scope: owner}, "senders")).Render(ctx, out)
	})
	requireAdminUsersHTML(t, "owner avatar checks", avatars,
		`id="settings-content"`, "max-w-6xl", "Diagnostics",
		`action="/settings/diagnostics/avatars/recheck"`, `action="/settings/diagnostics/avatars/cancel"`,
		`href="/api/diagnostics/avatars/status"`, `data-diagnostics-api="/api/diagnostics"`, `data-diagnostics-events="app"`,
		`data-tui-tabs-path="/settings/diagnostics/avatars/"`, `href="/settings/operations"`,
		`hx-get="/settings/diagnostics/contacts"`, `hx-target="#settings-content"`,
	)
	if tab := openingTag(avatars, `href="/settings/diagnostics/avatars/"`); !strings.Contains(tab, `aria-current="page"`) {
		t.Fatalf("Avatar checks isn't the current page: %q", tab)
	}
	for _, console := range []string{"data-admin-webmail-scope-form", "/admin/", "/api/admin"} {
		if strings.Contains(avatars, console) {
			t.Fatalf("the owner's avatar checks lead into the console: %q", console)
		}
	}
	contacts := renderDiagnostics(t, func(out *bytes.Buffer) error {
		return OwnerDiagnosticsPartial("contacts", AdminContactsPage(models.ContactAdminStatus{Scope: owner})).Render(ctx, out)
	})
	requireAdminUsersHTML(t, "owner contacts", contacts, `action="/settings/diagnostics/contacts/backfill"`, "data-tui-tabs-local", `data-diagnostics-events="app"`)
	labels := renderDiagnostics(t, func(out *bytes.Buffer) error {
		return OwnerDiagnosticsPartial("labels", AdminLabelsPage(models.LabelAdminStatus{Scope: owner})).Render(ctx, out)
	})
	requireAdminUsersHTML(t, "owner labels", labels, `href="/api/diagnostics/labels/status"`)
	for name, html := range map[string]string{"contacts": contacts, "labels": labels} {
		if strings.Contains(html, "/admin/") || strings.Contains(html, "/api/admin") {
			t.Fatalf("the owner's %s lead into the console", name)
		}
	}

	console := renderDiagnostics(t, func(out *bytes.Buffer) error {
		return AdminPage(models.AvatarStatus{}, "overview").Render(ctx, out)
	})
	requireAdminUsersHTML(t, "console avatar checks", console,
		"data-admin-webmail-scope-form", `action="/admin/avatar-backfill/recheck"`, `data-diagnostics-api="/api/admin"`,
		`data-diagnostics-events="/api/admin/events"`, `data-tui-tabs-path="/admin/system/diagnostics/avatars/"`,
	)
}
