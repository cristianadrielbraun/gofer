package views

import (
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// OwnerDiagnosticsPath is Settings → Diagnostics, where the owner of an
// open or personal instance finds what the console's diagnostics show.
const OwnerDiagnosticsPath = "/settings/diagnostics"

// OwnerAvatarDiagnosticsPath is the avatar checks in Settings; its tabs
// follow it, as the console's follow AdminAvatarsPath.
const OwnerAvatarDiagnosticsPath = OwnerDiagnosticsPath + "/avatars/"

// adminDiagnosticsURL is one of the diagnostics' addresses, given as the
// console's, as the page's viewer reaches it: in the console, narrowed to
// the person chosen; in an owner's Settings, at the owner's addresses.
func adminDiagnosticsURL(scope models.AdminWebmailScope, consolePath string) string {
	if !scope.Owner {
		return adminScopeURL(consolePath, scope)
	}
	switch {
	case strings.HasPrefix(consolePath, "/api/admin/"):
		return "/api/diagnostics/" + strings.TrimPrefix(consolePath, "/api/admin/")
	case consolePath == "/admin/avatar-backfill/recheck":
		return OwnerAvatarDiagnosticsPath + "recheck"
	case consolePath == "/admin/avatar-backfill/cancel":
		return OwnerAvatarDiagnosticsPath + "cancel"
	case consolePath == "/admin/contacts/backfill":
		return OwnerDiagnosticsPath + "/contacts/backfill"
	case strings.HasPrefix(consolePath, AdminDiagnosticsPath):
		return OwnerDiagnosticsPath + strings.TrimPrefix(consolePath, AdminDiagnosticsPath)
	}
	return consolePath
}

// adminAvatarTabsPath is where the avatar checks' tabs live; each tab adds
// its name to it, and the overview is the address itself.
func adminAvatarTabsPath(scope models.AdminWebmailScope) string {
	if scope.Owner {
		return OwnerAvatarDiagnosticsPath
	}
	return AdminAvatarsPath
}

// adminDiagnosticsRootAttributes mark a diagnostics page's live part for
// diagnostics.js: what it is (marker), where its data and events come
// from, and, for tabs that follow the address, their base path.
func adminDiagnosticsRootAttributes(scope models.AdminWebmailScope, marker, tabsPath string) templ.Attributes {
	attributes := templ.Attributes{marker: "", "data-diagnostics-api": "/api/admin", "data-diagnostics-events": "/api/admin/events"}
	if scope.Owner {
		// Settings shares the webmail's event stream.
		attributes["data-diagnostics-api"] = "/api/diagnostics"
		attributes["data-diagnostics-events"] = "app"
	}
	if tabsPath != "" {
		attributes["data-tui-tabs-path"] = tabsPath
	} else {
		// Tabs that don't follow the address stay out of Settings' own
		// tab addresses.
		attributes["data-tui-tabs-local"] = ""
	}
	return attributes
}
