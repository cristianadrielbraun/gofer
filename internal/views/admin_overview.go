package views

import (
	"github.com/cristianadrielbraun/gofer/internal/audit"
)

// AdminOverviewPath is the console's front page.
const AdminOverviewPath = "/admin/overview"

// AdminOverviewData is the console's front page: how many people there
// are, what needs an administrator, and the latest administrative changes.
type AdminOverviewData struct {
	Total          int
	Active         int
	Pending        int
	Disabled       int
	Administrators int
	// Attention lists what needs an administrator, each with a count and
	// where to see it. Only what has a count shows.
	Attention []AdminOverviewItem
	// Recent are the latest administrative changes, when the session may
	// read them; RecentLocked says it must verify again first.
	Recent       []audit.Record
	RecentLocked bool
}

// AdminOverviewItem is one thing that needs an administrator.
type AdminOverviewItem struct {
	// Key names the item's message, "admin.overview.attention.<Key>".
	Key   string
	Count int
	Href  string
}
