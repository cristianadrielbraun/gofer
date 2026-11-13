package views

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// AdminUsersQuery is what the Users page's list shows: a search, a status,
// a reason people need an administrator, an order, and a page (from 1).
// Empty fields are the defaults: everyone, by username, the first page.
type AdminUsersQuery struct {
	Search    string
	Status    string
	Attention string
	Sort      string
	// Group keeps the members of the group with this ID.
	Group string
	Page  int
}

// Narrowed reports whether the query leaves anyone out.
func (query AdminUsersQuery) Narrowed() bool {
	return query.Search != "" || query.Status != "" || query.Attention != "" || query.Group != ""
}

// Path is the Users page showing page of query's list.
func (query AdminUsersQuery) Path(page int) string {
	values := url.Values{}
	if query.Search != "" {
		values.Set("q", query.Search)
	}
	if query.Status != "" {
		values.Set("status", query.Status)
	}
	if query.Attention != "" {
		values.Set("attention", query.Attention)
	}
	if query.Sort != "" {
		values.Set("sort", query.Sort)
	}
	if query.Group != "" {
		values.Set("group", query.Group)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if len(values) == 0 {
		return "/admin/users"
	}
	return "/admin/users?" + values.Encode()
}

// AdminUsersExportPath downloads the Users list.
const AdminUsersExportPath = "/admin/users/export"

// ExportPath downloads everyone query's list holds, in its order, as CSV.
func (query AdminUsersQuery) ExportPath() string {
	path := query.Path(1)
	if rest, ok := strings.CutPrefix(path, "/admin/users?"); ok {
		return AdminUsersExportPath + "?" + rest
	}
	return AdminUsersExportPath
}

// adminUsersPageLink moves the list to path in place, as its filters do.
func adminUsersPageLink(path string) templ.Attributes {
	if path == "" {
		return nil
	}
	return templ.Attributes{
		"hx-get": path, "hx-target": "#admin-users-results", "hx-select": "#admin-users-results",
		"hx-swap": "outerHTML", "hx-push-url": "true", "data-admin-users-page-link": "",
	}
}

type adminUsersChoiceOption struct {
	Value string
	Label string
}

// adminUsersGroupChoices are the group filter's choices: everyone, then
// each group.
func adminUsersGroupChoices(ctx context.Context, groups []AdminGroupRef) []adminUsersChoiceOption {
	choices := []adminUsersChoiceOption{{"", i18n.T(ctx, "admin.users.filter.any_group")}}
	for _, group := range groups {
		choices = append(choices, adminUsersChoiceOption{group.ID, group.Name})
	}
	return choices
}

func adminUsersStatusChoices(ctx context.Context) []adminUsersChoiceOption {
	return []adminUsersChoiceOption{
		{"", i18n.T(ctx, "admin.users.filter.any_status")},
		{"active", adminValueLabel(ctx, "active")},
		{"pending", adminValueLabel(ctx, "pending")},
		{"disabled", adminValueLabel(ctx, "disabled")},
	}
}

func adminUsersAttentionChoices(ctx context.Context) []adminUsersChoiceOption {
	return []adminUsersChoiceOption{
		{"", i18n.T(ctx, "admin.users.filter.everyone")},
		{"reset_requested", i18n.T(ctx, "admin.users.filter.reset_requested")},
		{"mfa_setup", i18n.T(ctx, "admin.users.filter.mfa_setup")},
		{"invitation_expired", i18n.T(ctx, "admin.users.filter.invitation_expired")},
		{"deleting", i18n.T(ctx, "admin.users.filter.deleting")},
	}
}

func adminUsersSortChoices(ctx context.Context) []adminUsersChoiceOption {
	return []adminUsersChoiceOption{
		{"", i18n.T(ctx, "admin.users.sort.username")},
		{"newest", i18n.T(ctx, "admin.users.sort.newest")},
		{"last_sign_in", i18n.T(ctx, "admin.users.sort.last_sign_in")},
		{"status", i18n.T(ctx, "admin.users.sort.status")},
	}
}
