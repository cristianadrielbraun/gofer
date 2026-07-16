package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailFiltersPopoverRendersWithCompactSidebarSections(t *testing.T) {
	var out bytes.Buffer
	accounts := []models.Account{{ID: "account-1", Email: "person@example.com"}}
	if err := MailFiltersPopover(accounts).Render(context.Background(), &out); err != nil {
		t.Fatalf("MailFiltersPopover.Render() error = %v", err)
	}

	html := out.String()
	for _, want := range []string{
		`id="mail-filters-popover"`,
		`data-tui-popover-content`,
		`data-tui-popover-placement="bottom-end"`,
		`data-mail-filter-button`,
		`>Filters</span>`,
		`data-mail-filter-panel-button="calendar"`,
		`data-mail-filter-panel-button="audience"`,
		`data-mail-filter-panel-button="content"`,
		`data-mail-filter-panel-button="files"`,
		`data-mail-filter-panel-button="status"`,
		`data-mail-advanced-filter-count`,
		`w-[min(36rem,calc(100vw-1.5rem))]`,
		`h-[min(28rem,calc(100vh-2rem))]`,
		`sm:grid-cols-[9rem_minmax(0,1fr)]`,
		`grid-rows-[auto_minmax(0,1fr)]`,
		`sm:grid-rows-[minmax(0,1fr)]`,
		`overflow-y-auto overscroll-contain`,
		`data-[tui-popover-open=true]:[&amp;::backdrop]:bg-black/25`,
		`data-mail-filter-panel-box`,
		`data-mail-filters-close`,
		"People and accounts",
		"Choose a contact, account, sender, or recipient.",
		"Date range",
		"Message status",
		`name="subject"`,
		`name="account_id"`,
		`name="participant"`,
		`data-mail-contact-filter-select`,
		`data-mail-contact-filter-options`,
		`Type a name or email`,
		`name="recipient_type"`,
		`name="recipient_domain"`,
		`name="after_date"`,
		`name="attachment_type"`,
		`name="attachment_extension"`,
		`name="min_size_mb"`,
		`name="max_size_mb"`,
		`data-mail-tristate="threads"`,
		`data-mail-tristate="tags"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("advanced filters markup missing %q", want)
		}
	}
	if strings.Contains(html, `data-mail-filter-panel-button="threads"`) || strings.Contains(html, `data-mail-filter-panel="threads"`) {
		t.Error("threads should be consolidated into the status section")
	}
	if strings.Contains(html, `mail-advanced-filter-dialog`) || strings.Contains(html, `data-mail-advanced-filter-open`) || strings.Contains(html, "Quick filters") {
		t.Error("mail filters should use one popover entry point without the old dialog or quick filters")
	}
	if strings.Contains(html, "data-mail-filter-summary") || strings.Contains(html, "data-mail-filter-chip-remove") {
		t.Error("advanced filter selections should use the Apply counter instead of a pills section")
	}
	if strings.Contains(html, "min-h-[19rem]") || strings.Contains(html, "sm:min-h-[15rem]") {
		t.Error("advanced filter panel should size naturally instead of reserving empty space")
	}
	if strings.Contains(html, "sm:grid-cols-2") || strings.Contains(html, "sm:grid-cols-3") || strings.Contains(html, "grid-cols-2 gap-3") {
		t.Error("filter controls should use one column inside the detail panel")
	}
}
