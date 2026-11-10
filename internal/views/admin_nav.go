package views

import (
	"context"
	"net/url"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/components/icon"
	"github.com/cristianadrielbraun/gofer/components/toast"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// The admin console's sections. A handler names the one its page belongs
// to, and the navigation marks it.
const (
	AdminSectionUsers           = "users"
	AdminSectionMailSecurity    = "security"
	AdminSectionActivity        = "activity"
	AdminSectionAvatars         = "avatars"
	AdminSectionContacts        = "contacts"
	AdminSectionLabels          = "labels"
	AdminSectionOperations      = "operations"
	AdminSectionAccountSecurity = "account-security"
)

// Where the System section's diagnostics live. AdminAvatarsPath ends in a
// slash: its tabs follow it (overview has none).
const (
	AdminSystemPath      = "/admin/system"
	AdminDiagnosticsPath = AdminSystemPath + "/diagnostics"
	AdminAvatarsPath     = AdminDiagnosticsPath + "/avatars/"
	AdminContactsPath    = AdminDiagnosticsPath + "/contacts"
	AdminLabelsPath      = AdminDiagnosticsPath + "/labels"
	AdminOperationsPath  = AdminDiagnosticsPath + "/operations"
)

// AdminShellPage is one page of the admin console: the section it belongs
// to, the webmail user its diagnostics are narrowed to, its content, and
// the results to show as toasts when it opens.
type AdminShellPage struct {
	UISettings map[string]string
	Section    string
	Scope      models.AdminWebmailScope
	Content    templ.Component
	Toasts     []AdminToast
}

// AdminToast is the result of a change, shown as a toast: a success that
// goes away by itself, or an error that stays until it's closed.
type AdminToast struct {
	Variant string
	Message string
}

const (
	AdminToastSuccess = "success"
	AdminToastError   = "error"
)

// adminToastProps shows a result with templui's toast. Results are
// sentences, so they go in the description; a title would be cut to one
// line. An error has a negative duration, which keeps toast.js from
// dismissing it on a timer.
func adminToastProps(item AdminToast) toast.Props {
	props := toast.Props{
		Description: item.Message, Variant: toast.VariantSuccess,
		Duration: 5000, Dismissible: true, Icon: true,
	}
	if item.Variant == AdminToastError {
		props.Variant = toast.VariantError
		props.Duration = -1
	}
	return props
}

type adminNavItem struct {
	section string
	path    string
	label   string // translation key
	short   string // translation key for the phone navigation, when it needs a shorter name
	icon    func(...icon.Props) templ.Component
	scoped  bool // diagnostics: the link keeps the webmail user they are narrowed to
	managed bool // only with managed sign-in, not in open mode's local workspace
}

type adminNavGroup struct {
	label string // translation key
	items []adminNavItem
}

// adminNavigation lists the console's sections once, for the sidebar and
// the phone navigation alike.
var adminNavigation = []adminNavGroup{
	{label: "admin.nav.group.people", items: []adminNavItem{
		{section: AdminSectionUsers, path: "/admin/users", label: "admin.nav.users", icon: icon.Users, managed: true},
	}},
	{label: "admin.nav.group.security", items: []adminNavItem{
		{section: AdminSectionMailSecurity, path: "/admin/security", label: "admin.nav.security", icon: icon.ShieldAlert},
		{section: AdminSectionActivity, path: "/admin/activity", label: "admin.nav.activity_short", icon: icon.History, managed: true},
	}},
	{label: "admin.nav.group.system", items: []adminNavItem{
		{section: AdminSectionAvatars, path: AdminAvatarsPath, label: "admin.nav.avatars", short: "admin.nav.avatars_short", icon: icon.CircleUserRound, scoped: true},
		{section: AdminSectionContacts, path: AdminContactsPath, label: "admin.nav.contacts", icon: icon.BookUser, scoped: true},
		{section: AdminSectionLabels, path: AdminLabelsPath, label: "admin.nav.labels", icon: icon.Tags, scoped: true},
		{section: AdminSectionOperations, path: AdminOperationsPath, label: "admin.nav.operations", short: "admin.nav.operations_short", icon: icon.ClipboardList, scoped: true},
	}},
}

// adminAccountNavItem is the administrator's own sign-in and sessions. The
// sidebar keeps it in its footer; the phone navigation lists it last.
var adminAccountNavItem = adminNavItem{
	section: AdminSectionAccountSecurity, path: auth.ManagementAccountPath,
	label: "admin.nav.account_security", icon: icon.ShieldCheck, managed: true,
}

type adminNavLinkView struct {
	Href   string
	Label  string
	Short  string
	Icon   func(...icon.Props) templ.Component
	Active bool
}

type adminNavGroupView struct {
	Label string
	Items []adminNavLinkView
}

// managedAuthenticationNavigation reports whether the console shows its
// managed navigation: to management identities, and when nobody is signed
// in. Open mode's local profile gets the local workspace instead.
func managedAuthenticationNavigation(ctx context.Context) bool {
	user := auth.GetCurrentUser(ctx)
	return user == nil || user.IsManagement()
}

// adminNavigationGroups resolves the navigation for the person viewing the
// console, leaving out groups that end up empty.
func adminNavigationGroups(ctx context.Context, active string, scope models.AdminWebmailScope) []adminNavGroupView {
	managed := managedAuthenticationNavigation(ctx)
	groups := make([]adminNavGroupView, 0, len(adminNavigation))
	for _, group := range adminNavigation {
		view := adminNavGroupView{Label: i18n.T(ctx, group.label)}
		for _, item := range group.items {
			if item.managed && !managed {
				continue
			}
			view.Items = append(view.Items, adminNavLinkFor(ctx, item, active, scope))
		}
		if len(view.Items) > 0 {
			groups = append(groups, view)
		}
	}
	return groups
}

// adminMobileNavigation is the navigation as one row of links, with the
// administrator's own account at the end.
func adminMobileNavigation(ctx context.Context, active string, scope models.AdminWebmailScope) []adminNavLinkView {
	var links []adminNavLinkView
	for _, group := range adminNavigationGroups(ctx, active, scope) {
		links = append(links, group.Items...)
	}
	if managedAuthenticationNavigation(ctx) {
		links = append(links, adminNavLinkFor(ctx, adminAccountNavItem, active, scope))
	}
	return links
}

func adminNavLinkFor(ctx context.Context, item adminNavItem, active string, scope models.AdminWebmailScope) adminNavLinkView {
	href := item.path
	if item.scoped && scope.SelectedUserID != "" {
		href += "?user_id=" + url.QueryEscape(scope.SelectedUserID)
	}
	label := i18n.T(ctx, item.label)
	short := label
	if item.short != "" {
		short = i18n.T(ctx, item.short)
	}
	return adminNavLinkView{Href: href, Label: label, Short: short, Icon: item.icon, Active: item.section == active}
}

func adminNavLinkClass(active bool) string {
	base := "flex w-full items-center gap-2.5 rounded-md px-3 py-2 text-left text-sm transition-colors "
	if active {
		return base + "bg-accent font-semibold text-foreground"
	}
	return base + "text-muted-foreground hover:bg-accent hover:text-foreground"
}

func adminMobileNavLinkClass(active bool) string {
	base := "flex shrink-0 items-center rounded-md px-3 py-1.5 text-xs font-semibold transition-colors "
	if active {
		return base + "bg-accent text-foreground"
	}
	return base + "text-muted-foreground hover:bg-accent hover:text-foreground"
}

// The console's pages still carry the mobile header they had inside the
// webmail shell. The console has its own, so it hides theirs.
const adminPanelStyle = `<style>[data-management-shell] #main-content > [class*="flex-1"] > [class*="overflow-y-auto"] > .lg\:hidden.border-b{display:none!important}</style>`

// Management identities sign in with local credentials only, so their
// security page hides the webmail's provider sign-in card.
const adminAccountStyle = `<style>[data-management-shell] [data-federated\-identity-settings]{display:none!important}</style>`
