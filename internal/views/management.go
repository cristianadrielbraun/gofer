package views

import (
	"context"
	"io"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func writeHTML(w io.Writer, values ...string) error {
	for _, value := range values {
		if _, err := io.WriteString(w, value); err != nil {
			return err
		}
	}
	return nil
}

func escaped(value string) string { return templ.EscapeString(value) }

func ManagementLoginPage(errorMessage, identifier string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="en" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Gofer Admin — Sign In</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk"><main class="min-h-screen flex items-center justify-center p-4"><div class="w-full max-w-sm"><div class="text-center mb-8"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-semibold uppercase tracking-[0.2em] text-primary">Management</p><h1 class="mt-2 text-2xl font-semibold">Sign in to Gofer Admin</h1><p class="mt-2 text-sm text-muted-foreground">Management accounts are separate from webmail accounts.</p></div>`); err != nil {
			return err
		}
		if errorMessage != "" {
			if err := writeHTML(w, `<div id="admin-login-error" role="alert" class="mb-6 rounded-lg border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive">`, escaped(errorMessage), `</div>`); err != nil {
				return err
			}
		}
		return writeHTML(w, `<form method="post" action="/admin/login" class="space-y-4"><div class="space-y-2"><label for="admin-login-identifier" class="text-sm font-medium">Username</label><input id="admin-login-identifier" name="identifier" type="text" value="`, escaped(identifier), `" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus class="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"></div><div class="space-y-2"><label for="admin-login-password" class="text-sm font-medium">Password</label><input id="admin-login-password" name="password" type="password" autocomplete="current-password" required class="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"></div><button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground shadow-sm hover:bg-primary/90">Sign in to Admin</button></form><p class="mt-6 text-center text-xs text-muted-foreground">Looking for your mailbox? <a href="/login" class="font-semibold underline underline-offset-4 hover:text-foreground">Use webmail sign-in</a>.</p></div></main></body></html>`)
	})
}

func ManagementMFAContinuationPage(errorMessage string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="en" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Gofer Admin — Verify</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk"><main class="min-h-screen flex items-center justify-center p-4"><div class="w-full max-w-sm"><div class="text-center"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-semibold uppercase tracking-[0.2em] text-primary">Management</p><h1 class="mt-2 text-2xl font-semibold">Verify your authenticator</h1><p class="mt-3 text-sm text-muted-foreground">Enter the six-digit code from your authenticator app to finish signing in.</p></div>`); err != nil {
			return err
		}
		if errorMessage != "" {
			if err := writeHTML(w, `<div id="admin-login-mfa-error" role="alert" class="mt-6 rounded-lg border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive">`, escaped(errorMessage), `</div>`); err != nil {
				return err
			}
		}
		return writeHTML(w, `<form method="post" action="/login/mfa" class="mt-6 space-y-4"><label for="admin-login-mfa-code" class="text-sm font-medium">Authenticator code</label><input id="admin-login-mfa-code" name="code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" minlength="6" maxlength="6" required autofocus class="flex h-12 w-full rounded-md border border-input bg-background px-3 py-2 text-center font-mono text-xl tracking-[0.35em] outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"><button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground shadow-sm hover:bg-primary/90">Verify and sign in</button></form><div class="mt-4"><a href="/admin/login" class="inline-flex h-10 w-full items-center justify-center rounded-md border border-border bg-background px-4 text-sm font-semibold hover:bg-accent">Restart sign-in</a></div><div class="mt-4 text-center"><a href="/login/mfa/recovery" class="text-sm text-muted-foreground underline underline-offset-4 hover:text-foreground">Use a recovery code instead</a></div></div></main></body></html>`)
	})
}

func managementNavClass(active, item string) string {
	base := "flex items-center rounded-md px-3 py-2 text-sm transition-colors "
	if active == item {
		return base + "bg-accent font-semibold text-foreground"
	}
	return base + "text-muted-foreground hover:bg-accent hover:text-foreground"
}

func renderManagementSidebar(ctx context.Context, w io.Writer, active string) error {
	items := []struct{ path, key, label string }{
		{"/admin/users", "users", "Users"},
		{"/admin/avatars/", "avatars", "Avatar checks"},
		{"/admin/contacts", "contacts", "Contacts"},
		{"/admin/labels", "labels", "Labels"},
		{"/admin/operations", "operations", "Mail operations"},
		{"/admin/security", "security", "Mail security"},
		{"/admin/account/security", "account-security", "Account security"},
	}
	if err := writeHTML(w, `<aside class="hidden lg:flex w-64 shrink-0 flex-col border-r border-border bg-card/75 backdrop-blur-sm"><div class="border-b border-border px-5 py-5"><a href="/admin" class="inline-flex items-center gap-2.5 text-foreground hover:text-primary"><img src="/assets/logo.svg" alt="Gofer" class="h-8 w-8 shrink-0 p-1"><span class="text-lg font-bold tracking-tight" style="font-family:var(--font-serif)">Gofer Admin</span></a><p class="mt-2 text-xs leading-relaxed text-muted-foreground">Dedicated management workspace.</p></div><nav class="flex-1 space-y-1 px-3 py-4">`); err != nil {
		return err
	}
	for _, item := range items {
		current := ""
		if active == item.key {
			current = ` aria-current="page"`
		}
		if err := writeHTML(w, `<a href="`, item.path, `" data-admin-navigation-link data-admin-navigation-label="`, escaped(item.label), `" class="`, managementNavClass(active, item.key), `"`, current, `>`, escaped(item.label), `</a>`); err != nil {
			return err
		}
	}
	csrf := auth.CSRFToken(ctx, "POST", "/auth/logout")
	return writeHTML(w, `</nav><div class="border-t border-border px-5 py-4"><form method="post" action="/auth/logout"><input type="hidden" name="_csrf" value="`, escaped(csrf), `"><button type="submit" class="inline-flex h-9 w-full items-center justify-center rounded-md border border-border bg-background px-3 text-xs font-semibold text-muted-foreground hover:bg-accent hover:text-foreground">Sign out of Admin</button></form></div></aside>`)
}

func renderManagementMobileNav(w io.Writer, active string) error {
	items := []struct{ path, key, label string }{
		{"/admin/users", "users", "Users"},
		{"/admin/avatars/", "avatars", "Avatars"},
		{"/admin/contacts", "contacts", "Contacts"},
		{"/admin/labels", "labels", "Labels"},
		{"/admin/operations", "operations", "Operations"},
		{"/admin/security", "security", "Mail security"},
		{"/admin/account/security", "account-security", "Account security"},
	}
	if err := writeHTML(w, `<header class="lg:hidden shrink-0 border-b border-border bg-card"><div class="flex items-center justify-between px-4 py-3"><a href="/admin" class="font-bold text-foreground" style="font-family:var(--font-serif)">Gofer Admin</a><span class="text-xs font-semibold uppercase tracking-wider text-primary">Management</span></div><nav class="flex gap-1 overflow-x-auto px-3 pb-3" aria-label="Admin sections">`); err != nil {
		return err
	}
	for _, item := range items {
		current := ""
		if active == item.key {
			current = ` aria-current="page"`
		}
		if err := writeHTML(w, `<a href="`, item.path, `" data-admin-navigation-link data-admin-navigation-label="`, escaped(item.label), `" class="shrink-0 rounded-md px-3 py-1.5 text-xs font-semibold `, managementNavClass(active, item.key), `"`, current, `>`, escaped(item.label), `</a>`); err != nil {
			return err
		}
	}
	return writeHTML(w, `</nav></header>`)
}

func ManagementAdminLayout(uiSettings map[string]string, userData AdminUsersData, avatarStatus models.AvatarStatus, contactStatus models.ContactAdminStatus, labelStatus models.LabelAdminStatus, securityData models.MailSecurityAdminData, operationStatus models.MailOperationsAdminStatus, activeSection, activeTab string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="en" class="`, escaped(themeClass(uiSettings)), `" data-theme="`, escaped(themeStyle(uiSettings)), `"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Admin — Gofer</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css">`); err != nil {
			return err
		}
		if err := SettingsComponentScripts().Render(ctx, w); err != nil {
			return err
		}
		if err := writeHTML(w, `<style>[data-management-shell] #main-content > [class*="flex-1"] > [class*="overflow-y-auto"] > .lg\:hidden.border-b{display:none!important}</style></head><body class="bg-background text-foreground antialiased surface-desk" data-management-shell><div class="flex h-screen overflow-hidden bg-background">`); err != nil {
			return err
		}
		if err := renderManagementSidebar(ctx, w, activeSection); err != nil {
			return err
		}
		if err := writeHTML(w, `<div class="flex min-w-0 flex-1 flex-col overflow-hidden">`); err != nil {
			return err
		}
		if err := renderManagementMobileNav(w, activeSection); err != nil {
			return err
		}
		if err := writeHTML(w, `<div id="main-content" class="relative flex flex-1 min-w-0">`); err != nil {
			return err
		}
		if err := AdminNavigationLoading().Render(ctx, w); err != nil {
			return err
		}
		var content templ.Component
		switch activeSection {
		case "users":
			content = AdminUsersPage(userData)
		case "contacts":
			content = AdminContactsPage(contactStatus)
		case "labels":
			content = AdminLabelsPage(labelStatus)
		case "security":
			content = AdminSecurityPage(securityData)
		case "operations":
			content = AdminMailOperationsPage(operationStatus)
		default:
			content = AdminPage(avatarStatus, activeTab)
		}
		if err := content.Render(ctx, w); err != nil {
			return err
		}
		return writeHTML(w, `</div></div></div><script src="/assets/js/ui-settings.js"></script><script src="/assets/js/admin.js"></script></body></html>`)
	})
}

func ManagementSecurityLayout(uiSettings map[string]string, content templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="en" class="`, escaped(themeClass(uiSettings)), `" data-theme="`, escaped(themeStyle(uiSettings)), `"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Account security — Gofer Admin</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css">`); err != nil {
			return err
		}
		if err := SettingsComponentScripts().Render(ctx, w); err != nil {
			return err
		}
		if err := writeHTML(w, `<style>[data-management-shell] [data-federated\-identity-settings]{display:none!important}</style></head><body class="bg-background text-foreground antialiased surface-desk" data-management-shell><div class="flex h-screen overflow-hidden bg-background">`); err != nil {
			return err
		}
		if err := renderManagementSidebar(ctx, w, "account-security"); err != nil {
			return err
		}
		if err := writeHTML(w, `<div class="flex min-w-0 flex-1 flex-col overflow-hidden">`); err != nil {
			return err
		}
		if err := renderManagementMobileNav(w, "account-security"); err != nil {
			return err
		}
		if err := writeHTML(w, `<main id="main-content" class="flex flex-1 min-w-0 overflow-y-auto"><div class="w-full max-w-3xl px-8 py-10"><div class="mb-6"><p class="text-xs font-semibold uppercase tracking-[0.18em] text-primary">Management account</p><h1 class="mt-2 text-2xl font-bold" style="font-family:var(--font-serif)">Account security</h1><p class="mt-1 text-sm text-muted-foreground">Credentials and active sessions for this management identity.</p></div>`); err != nil {
			return err
		}
		if err := content.Render(ctx, w); err != nil {
			return err
		}
		return writeHTML(w, `</div></main></div></div><script src="/assets/js/ui-settings.js"></script><script src="/assets/js/passkey-registration.js"></script><script src="/assets/js/passkey-authentication.js"></script><script src="/assets/js/settings.js"></script></body></html>`)
	})
}

func SeparatedSetupOwnerPage(data SetupOwnerData) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="en" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Gofer — Create Management Owner</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk"><main class="min-h-screen flex items-center justify-center p-4 py-10"><div class="w-full max-w-2xl"><div class="text-center mb-8"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-medium uppercase tracking-widest text-success">Setup access verified</p><h1 class="mt-2 text-2xl font-semibold">Create the management owner</h1><p class="mt-2 text-sm text-muted-foreground">This identity is used only for Gofer administration. Existing webmail users, mailboxes, and their data remain attached to their current owners.</p></div>`); err != nil {
			return err
		}
		if data.DraftSaved {
			if err := writeHTML(w, `<div role="status" class="mb-6 rounded-lg border border-success/25 bg-success/10 px-4 py-3"><p class="text-sm font-medium text-success">Management owner profile ready</p><p class="mt-1 text-xs text-muted-foreground">No user, role, credential, or mailbox ownership has changed yet.</p></div>`); err != nil {
				return err
			}
		}
		if data.DraftStale {
			if err := writeHTML(w, `<div role="alert" class="mb-6 rounded-lg border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm">Existing users changed after this draft was saved. Review and save the management owner again.</div>`); err != nil {
				return err
			}
		}
		if data.BlockedMessage != "" {
			return writeHTML(w, `<div role="alert" class="rounded-xl border border-destructive/25 bg-destructive/10 p-6"><h2 class="text-lg font-semibold">Owner setup needs local repair</h2><p class="mt-2 text-sm text-muted-foreground">`, escaped(data.BlockedMessage), `</p></div></div></main></body></html>`)
		}
		if err := writeHTML(w, `<form method="post" action="/setup/owner" class="space-y-6 rounded-xl border border-border bg-card p-6"><input type="hidden" name="owner_target" value="create"><div class="rounded-lg border border-border bg-muted/30 p-4"><p class="text-sm font-medium">A new, separate identity will be created</p><p class="mt-1 text-xs text-muted-foreground">Management users cannot own mailboxes. Existing webmail users are not promoted, claimed, merged, or renamed.</p></div>`); err != nil {
			return err
		}
		fields := []struct{ key, id, label, typ, autocomplete, value, help string }{
			{"name", "setup-owner-name", "Display name", "text", "name", data.Form.Name, ""},
			{"username", "setup-owner-username", "Username", "text", "username", data.Form.Username, "3–32 ASCII letters, numbers, periods, underscores, or hyphens."},
		}
		for _, field := range fields {
			if err := writeHTML(w, `<div class="space-y-2"><label for="`, field.id, `" class="text-sm font-medium">`, escaped(field.label), `</label><input id="`, field.id, `" name="`, field.key, `" type="`, field.typ, `" value="`, escaped(field.value), `" autocomplete="`, field.autocomplete, `" required class="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50">`); err != nil {
				return err
			}
			if field.help != "" {
				if err := writeHTML(w, `<p class="text-xs text-muted-foreground">`, escaped(field.help), `</p>`); err != nil {
					return err
				}
			}
			if message := data.Form.Errors[field.key]; message != "" {
				if err := writeHTML(w, `<p role="alert" class="text-sm text-destructive">`, escaped(message), `</p>`); err != nil {
					return err
				}
			}
			if err := writeHTML(w, `</div>`); err != nil {
				return err
			}
		}
		return writeHTML(w, `<button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground hover:bg-primary/90">Save management owner and continue</button></form></div></main></body></html>`)
	})
}
