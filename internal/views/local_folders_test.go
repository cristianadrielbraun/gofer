package views

import (
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// Hidden Local folders aren't offered as a place to move mail, unless the
// message is already in them.
func TestMoveTargetsLeaveOutHiddenLocalFolders(t *testing.T) {
	accounts := []models.Account{{ID: "mail", Provider: "imap"}, {ID: "local", Provider: providers.ProviderLocal, Hidden: true}}
	if got := moveTargetAccounts(accounts, "mail"); len(got) != 1 || got[0].ID != "mail" {
		t.Fatalf("targets from mail = %+v", got)
	}
	if got := moveTargetAccounts(accounts, "local"); len(got) != 2 {
		t.Fatalf("targets from local = %+v", got)
	}
}

// The Accounts settings show Local folders on their own card, with the
// switch and the backup warning.
func TestSettingsShowLocalFoldersCard(t *testing.T) {
	var b strings.Builder
	accounts := []models.Account{{ID: "local", Name: "Local folders", Provider: providers.ProviderLocal, Hidden: true, EmailSyncEnabled: true}}
	if err := SettingsAccountsTab(accounts).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{"data-local-folders-card", `hx-post="/api/accounts/local/shown"`, "stored only on this Gofer server", `href="/settings/export-import"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(html, `name="shown" value="true" checked`) {
		t.Fatal("switch on for hidden Local folders")
	}
}
