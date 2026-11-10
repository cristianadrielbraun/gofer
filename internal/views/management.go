package views

import (
	"context"
	"io"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="`, escaped(pageLang(ctx)), `" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>`, escaped(i18n.T(ctx, "admin.login.page_title")), `</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk">`); err != nil {
			return err
		}
		if err := PublicPageCorners().Render(ctx, w); err != nil {
			return err
		}
		if err := writeHTML(w, `<main class="min-h-screen flex items-center justify-center p-4"><div class="w-full max-w-sm"><div class="text-center mb-8"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-semibold uppercase tracking-[0.2em] text-primary">`, escaped(i18n.T(ctx, "admin.management")), `</p><h1 class="mt-2 text-2xl font-semibold">`, escaped(i18n.T(ctx, "admin.login.title")), `</h1><p class="mt-2 text-sm text-muted-foreground">`, escaped(i18n.T(ctx, "admin.login.separate")), `</p></div>`); err != nil {
			return err
		}
		if errorMessage != "" {
			if err := writeHTML(w, `<div id="admin-login-error" role="alert" class="mb-6 rounded-lg border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive">`, escaped(errorMessage), `</div>`); err != nil {
				return err
			}
		}
		return writeHTML(w, `<form method="post" action="/admin/login" class="space-y-4"><div class="space-y-2"><label for="admin-login-identifier" class="text-sm font-medium">`, escaped(i18n.T(ctx, "identities.username")), `</label><input id="admin-login-identifier" name="identifier" type="text" value="`, escaped(identifier), `" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus class="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"></div><div class="space-y-2"><label for="admin-login-password" class="text-sm font-medium">`, escaped(i18n.T(ctx, "identities.password")), `</label><input id="admin-login-password" name="password" type="password" autocomplete="current-password" required class="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"></div><button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground shadow-sm hover:bg-primary/90">`, escaped(i18n.T(ctx, "admin.login.submit")), `</button></form><p class="mt-6 text-center text-xs text-muted-foreground">`, escaped(i18n.T(ctx, "admin.login.mailbox_q")), ` <a href="/login" class="font-semibold underline underline-offset-4 hover:text-foreground">`, escaped(i18n.T(ctx, "admin.login.webmail")), `</a>.</p></div></main></body></html>`)
	})
}

func ManagementMFAContinuationPage(errorMessage string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="`, escaped(pageLang(ctx)), `" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>`, escaped(i18n.T(ctx, "admin.mfa.page_title")), `</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk">`); err != nil {
			return err
		}
		if err := PublicPageCorners().Render(ctx, w); err != nil {
			return err
		}
		if err := writeHTML(w, `<main class="min-h-screen flex items-center justify-center p-4"><div class="w-full max-w-sm"><div class="text-center"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-semibold uppercase tracking-[0.2em] text-primary">`, escaped(i18n.T(ctx, "admin.management")), `</p><h1 class="mt-2 text-2xl font-semibold">`, escaped(i18n.T(ctx, "admin.mfa.title")), `</h1><p class="mt-3 text-sm text-muted-foreground">`, escaped(i18n.T(ctx, "admin.mfa.description")), `</p></div>`); err != nil {
			return err
		}
		if errorMessage != "" {
			if err := writeHTML(w, `<div id="admin-login-mfa-error" role="alert" class="mt-6 rounded-lg border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive">`, escaped(errorMessage), `</div>`); err != nil {
				return err
			}
		}
		return writeHTML(w, `<form method="post" action="/login/mfa" class="mt-6 space-y-4"><label for="admin-login-mfa-code" class="text-sm font-medium">`, escaped(i18n.T(ctx, "admin.mfa.code")), `</label><input id="admin-login-mfa-code" name="code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" minlength="6" maxlength="6" required autofocus class="flex h-12 w-full rounded-md border border-input bg-background px-3 py-2 text-center font-mono text-xl tracking-[0.35em] outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50"><button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground shadow-sm hover:bg-primary/90">`, escaped(i18n.T(ctx, "admin.mfa.submit")), `</button></form><div class="mt-4"><a href="/admin/login" class="inline-flex h-10 w-full items-center justify-center rounded-md border border-border bg-background px-4 text-sm font-semibold hover:bg-accent">`, escaped(i18n.T(ctx, "admin.mfa.restart")), `</a></div><div class="mt-4 text-center"><a href="/login/mfa/recovery" class="text-sm text-muted-foreground underline underline-offset-4 hover:text-foreground">`, escaped(i18n.T(ctx, "admin.mfa.recovery")), `</a></div></div></main></body></html>`)
	})
}

func SeparatedSetupOwnerPage(data SetupOwnerData) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := writeHTML(w, `<!DOCTYPE html><html lang="`, escaped(pageLang(ctx)), `" class="dark" data-theme="classic"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>`, escaped(i18n.T(ctx, "admin.owner.page_title")), `</title><link rel="icon" type="image/svg+xml" href="/assets/logo.svg"><link rel="stylesheet" href="/assets/css/output.css"></head><body class="bg-background text-foreground antialiased surface-desk">`); err != nil {
			return err
		}
		if err := PublicPageCorners().Render(ctx, w); err != nil {
			return err
		}
		if err := writeHTML(w, `<main class="min-h-screen flex items-center justify-center p-4 py-10"><div class="w-full max-w-2xl"><div class="text-center mb-8"><img src="/assets/logo.svg" alt="Gofer" class="h-12 w-auto mx-auto"><p class="mt-5 text-xs font-medium uppercase tracking-widest text-success">`, escaped(i18n.T(ctx, "admin.owner.verified")), `</p><h1 class="mt-2 text-2xl font-semibold">`, escaped(i18n.T(ctx, "admin.owner.title")), `</h1><p class="mt-2 text-sm text-muted-foreground">`, escaped(i18n.T(ctx, "admin.owner.description")), `</p></div>`); err != nil {
			return err
		}
		if data.DraftSaved {
			if err := writeHTML(w, `<div role="status" class="mb-6 rounded-lg border border-success/25 bg-success/10 px-4 py-3"><p class="text-sm font-medium text-success">`, escaped(i18n.T(ctx, "admin.owner.ready")), `</p><p class="mt-1 text-xs text-muted-foreground">`, escaped(i18n.T(ctx, "admin.owner.ready_detail")), `</p></div>`); err != nil {
				return err
			}
		}
		if data.DraftStale {
			if err := writeHTML(w, `<div role="alert" class="mb-6 rounded-lg border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm">`, escaped(i18n.T(ctx, "admin.owner.stale")), `</div>`); err != nil {
				return err
			}
		}
		if data.BlockedMessage != "" {
			return writeHTML(w, `<div role="alert" class="rounded-xl border border-destructive/25 bg-destructive/10 p-6"><h2 class="text-lg font-semibold">`, escaped(i18n.T(ctx, "admin.owner.blocked")), `</h2><p class="mt-2 text-sm text-muted-foreground">`, escaped(data.BlockedMessage), `</p></div></div></main></body></html>`)
		}
		if err := writeHTML(w, `<form method="post" action="/setup/owner" class="space-y-6 rounded-xl border border-border bg-card p-6"><input type="hidden" name="owner_target" value="create"><div class="rounded-lg border border-border bg-muted/30 p-4"><p class="text-sm font-medium">`, escaped(i18n.T(ctx, "admin.owner.new_identity")), `</p><p class="mt-1 text-xs text-muted-foreground">`, escaped(i18n.T(ctx, "admin.owner.new_identity_detail")), `</p></div>`); err != nil {
			return err
		}
		fields := []struct{ key, id, label, typ, autocomplete, value, help string }{
			{"name", "setup-owner-name", i18n.T(ctx, "compose.from.display_name"), "text", "name", data.Form.Name, ""},
			{"username", "setup-owner-username", i18n.T(ctx, "identities.username"), "text", "username", data.Form.Username, i18n.T(ctx, "admin.owner.username_help")},
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
		return writeHTML(w, `<button type="submit" class="inline-flex h-10 w-full items-center justify-center rounded-md bg-primary px-4 text-sm font-semibold text-primary-foreground hover:bg-primary/90">`, escaped(i18n.T(ctx, "admin.owner.submit")), `</button></form></div></main></body></html>`)
	})
}
