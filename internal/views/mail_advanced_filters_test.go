package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailAdvancedFiltersRenderWithCompactSidebarSections(t *testing.T) {
	var out bytes.Buffer
	accounts := []models.Account{{ID: "account-1", Email: "person@example.com"}}
	if err := MailFilterAdvancedDialog(accounts).Render(context.Background(), &out); err != nil {
		t.Fatalf("MailFilterAdvancedDialog.Render() error = %v", err)
	}

	html := out.String()
	for _, want := range []string{
		`data-mail-filter-panel-button="calendar"`,
		`data-mail-filter-panel-button="audience"`,
		`data-mail-filter-panel-button="content"`,
		`data-mail-filter-panel-button="files"`,
		`data-mail-filter-panel-button="status"`,
		`data-mail-advanced-filter-count`,
		`sm:h-[26rem]`,
		`data-mail-filter-panel-box`,
		"People and accounts",
		"Date range",
		"Message status",
		`name="subject"`,
		`name="account_id"`,
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
	if strings.Contains(html, "data-mail-filter-summary") || strings.Contains(html, "data-mail-filter-chip-remove") {
		t.Error("advanced filter selections should use the Apply counter instead of a pills section")
	}
	if strings.Contains(html, "min-h-[19rem]") || strings.Contains(html, "sm:min-h-[15rem]") {
		t.Error("advanced filter panel should size naturally instead of reserving empty space")
	}
}
