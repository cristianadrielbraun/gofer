package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderAdminKit(t *testing.T, component templ.Component, children string) string {
	t.Helper()
	var out bytes.Buffer
	ctx := templ.WithChildren(context.Background(), templ.Raw(children))
	if err := component.Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestAdminKitPieces(t *testing.T) {
	header := renderAdminKit(t, adminPageHeader(adminPageHeaderProps{Title: "Sample page", Description: "What the page is for"}), "")
	requireAdminUsersHTML(t, "page header", header, "data-admin-page-header", "<h2", "Sample page", "What the page is for")
	if strings.Contains(header, "flex-wrap items-center gap-2") {
		t.Fatal("a page header without actions drew their row")
	}
	withActions := renderAdminKit(t, adminPageHeader(adminPageHeaderProps{Title: "Sample page", Actions: templ.Raw(`<button>Act</button>`)}), "")
	requireAdminUsersHTML(t, "page header actions", withActions, "<button>Act</button>")

	section := renderAdminKit(t, adminSection(adminSectionProps{ID: "sample-heading", Title: "Sample card", Aside: adminSectionNote("A note")}), "<p>Card body</p>")
	requireAdminUsersHTML(t, "section", section, `aria-labelledby="sample-heading"`, `<h3 id="sample-heading"`, "Sample card", "A note", "<p>Card body</p>")

	badge := renderAdminKit(t, adminBadge(adminToneWarning, templ.Attributes{"data-sample": ""}), "Expired")
	requireAdminUsersHTML(t, "badge", badge, "Expired", "bg-amber-500/10", "rounded-full", "data-sample")
	if strings.Contains(badge, "rounded-md") || strings.Contains(badge, "text-foreground") {
		t.Fatalf("the badge kept templui's shape or color under its tone: %q", badge)
	}
	plain := renderAdminKit(t, adminBadge(adminTonePlain), "Webmail user")
	if !strings.Contains(plain, "text-foreground") {
		t.Fatalf("a plain badge isn't templui's outline badge: %q", plain)
	}
	requireAdminUsersHTML(t, "empty state", renderAdminKit(t, adminEmptyState("Nothing here."), ""), "data-admin-empty", "Nothing here.")
}
