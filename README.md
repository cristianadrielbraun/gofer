<h1>
  <img src="./assets/logo.svg" width="64" align="absmiddle" alt="Gofer logo" />
  Gofer
</h1>

| Minimal light | Classic dark |
| --- | --- |
| <img alt="Gofer email card view in the minimal light theme" src="./screenshots/emails-card-minimal-light.png" /> | <img alt="Gofer email card view in the classic dark theme" src="./screenshots/emails-card-classic-dark.png" /> |

[View all screenshots](./screenshots/README.md)

<br>

Gofer is a self-hosted email, contacts, and calendar app I'm building. It started as a small mail thing and has grown to support personal use and shared installations with multiple users, separate administrators, invitations, and security policies.

You can run it on your own machine or a server and access it through a browser. Mail and related data are cached in SQLite wherever you host it. Gofer talks directly to your providers: IMAP/SMTP for generic mail accounts, Google APIs for Gmail, contacts, and calendars, Microsoft Graph for Outlook, and CardDAV/CalDAV for compatible contact and calendar services.

I'm still working on it, and it's pre-1.0, so expect things to keep changing as the app settles. Provider compatibility, setup, and upgrades still need care. Please read the limitations below and keep backups before upgrading.

For reference, I'm actively using it with six configured accounts and around 100k emails in total. So far, I haven't run into performance issues or increased memory consumption.

## features

What you can do with it today:

- **Users and access:** multiple users with separate administrators, invitations, and security policies; personal mode for one protected profile; and a no-login mode for local use.
- **Accounts and sync:** multiple IMAP/SMTP, Gmail, and Outlook accounts, with mail cached in the instance's SQLite database.
- **Reading and sending:** threads, attachments, drafts with autosave, signatures, scheduled send, and translation.
- **Organization and search:** folders, stars, archive, spam controls, and advanced search filters.
- **Contacts:** local address books, vCard import/export, Google, Outlook, and CardDAV sync, plus Gofer Sync for automatic contact syncing between accounts.
- **Calendar:** Google Calendar, Outlook, and CalDAV sync, month and week views, upcoming events, and event creation, editing, and deletion, including recurring series and individual occurrences.
- **Meetings:** guest invitations, RSVP from Calendar or Mail, rich event descriptions, and Google Meet or Microsoft Teams links where the provider supports them.
- **Security:** encrypted stored credentials, remote content blocked by default, password and external sign-in, and MFA with TOTP or passkeys.
- **Customization:** themes, layouts, account colors, regional settings, and browser/Web Push notifications.

## still moving

Things I'm still improving, in no particular order:

- smoother first-run setup and OAuth credential guidance
- shared OAuth applications so users do not need to register their own provider clients
- clearer diagnostics and reconnect flows
- broader test coverage around provider sync behavior
- deeper labels/tags workflows beyond filtering
- broader calendar provider compatibility and recurring meeting support
- richer regional and language settings
- more keyboard shortcuts, bulk actions, and cleanup flows

## running and building

Downloaded release binaries include the web assets. Development from source requires Go, `templ`, `tailwindcss`, and `task`.

```sh
task dev      # development server with hot reload
task build    # local build at ./tmp/main
task release  # self-contained binary at ./dist/gofer
```

With the development server running, open `http://local.localhost:8090`. See the [Taskfile](./Taskfile.yml) for packaging and other build tasks.

## setup and configuration

Choose `GOFER_AUTH_MODE=managed` for multiple users with separate administrator accounts, or `personal` for one protected profile with multiple mailboxes. Both modes guide you through first-run setup using a token printed in the terminal. The default `open` mode has no login and is intended for local use. See [`.env.example`](./.env.example) for configuration options.

Managed mode keeps each user's mail, contacts, Calendar and preferences in a separate SQLite database. Authentication and installation settings stay in the central database. New installations initialize this layout automatically.

An existing shared database is converted automatically the first time Gofer starts in managed mode, keeping the same `GOFER_DB_PATH`. Conversion can take a while for large mailboxes; the server starts listening once it finishes. The original database is kept as `<database>.shared` next to the new central database, its `.users` directory and `.layout.json` file. Back up the new files together with the encryption key and the `accounts` directory. If conversion fails, the original database is put back unchanged and the error is logged; if it is interrupted, it resumes on the next start. When the failure comes from the database's contents, later starts report the same error immediately instead of repeating the conversion, until the database or Gofer changes; delete `<database>.migration-failed.json` to retry anyway. Personal and open modes keep shared storage.

> [!NOTE]
> **Staying in open or personal mode?** Nothing changes for you in 0.5.0. Your database receives the usual automatic schema upgrade on the first start, and the per-user conversion below never runs unless you switch to managed mode.

> [!WARNING]
> **Upgrading a managed installation from a version before 0.5.0, or switching an existing installation to managed mode.** The first start in managed mode converts your database to per-user storage, and this is one-way:
>
> - **Back up first.** Stop Gofer and copy the whole data directory, including the database, `secret.key` (unless you use `GOFER_SECRET_KEY`) and the `accounts` directory.
> - **Have free disk space for a second copy.** The conversion needs about twice the database size: the original is kept as `<database>.shared` and the new databases are roughly as large, plus temporary files while it runs.
> - **Start from the same working directory as before.** When `GOFER_DB_PATH` is relative, earlier versions stored message and attachment file paths relative to the directory Gofer was started from. The conversion resolves them from the current directory; started elsewhere, it stops and leaves the database unchanged. After the first successful start, the paths are absolute and any working directory works.
> - **There is no way back except the retained original.** After conversion, earlier versions and personal mode cannot open the data. To return to the pre-upgrade state, stop Gofer, move away `<database>`, `<database>.users` and `<database>.layout.json`, and rename `<database>.shared` back to `<database>`. Mail, settings and accounts changed since the conversion are lost, and you need a pre-0.5.0 release to run it. Once you are sure you won't roll back, you can delete `<database>.shared` to free its space.
> - **Switching from open or personal mode works the same way.** Your profile keeps its mailboxes and becomes a regular user, and setup creates a separate administrator account. A personal profile keeps its password and other sign-in methods. An open profile has none yet, so the setup notice in the terminal also shows a token for setting its password at `/account/enroll`.

To convert into a separate file instead, stop Gofer and run `./gofer storage migrate --db data/gofer.db --to data/gofer-users.db` (repeat with `--retry` if interrupted), then set `GOFER_DB_PATH=data/gofer-users.db`.

Generic IMAP/SMTP accounts need no OAuth application credentials. Gmail and Outlook currently require your own provider client ID and secret. Mailbox authorization and optional Google/Microsoft application sign-in use separate clients and callbacks.

> [!NOTE]
> I plan to add Gmail and Outlook connections that don't require you to provide your own OAuth app credentials. First, I need to research the right way to set up public OAuth applications and complete the providers' identity verification and any required app reviews. Until then, you'll need to use your own credentials as described above.

Configure Calendar under **Settings → Accounts**: enable it for an account, discover calendars, and select which ones to sync. Google and Outlook use the account's existing OAuth connection; reconnect older accounts to grant Calendar access if needed. Generic accounts can connect a CalDAV server using an HTTPS URL and server credentials, which may require an app password.

## deployment precautions

For server deployments, use personal or managed authentication, serve the application through an HTTPS reverse proxy, and configure `GOFER_ADDR` and `GOFER_BASE_URL` for your deployment. The listener binds to loopback by default. Set `GOFER_TRUSTED_PROXY_CIDRS` only for your actual proxy peers; use `GOFER_ALLOWED_CIDRS` when you need to restrict client networks.

Runtime data is stored in `data/` by default. Protect the data directory, configuration, and secrets, and back them up together before upgrades. Shared databases apply schema migrations at startup, and managed mode converts them to per-user storage as described above. Going back to an older version may require restoring its matching backup, and the retained shared database does not include changes made after conversion. I recommend testing your providers and access setup before relying on Gofer for anything critical.

## current limitations

Calendar actions depend on provider permissions and capabilities:

- Creating or editing recurring meetings with guests or online meeting links is not supported yet.
- Existing Google Meet conferences cannot be removed in Gofer, Teams meetings cannot be disabled, and conference providers cannot be changed.
- CalDAV invitation delivery depends on the server's scheduling support or the account's mail-sending configuration.

## admin panel

In managed mode, `/admin` provides user administration, invitations, security policies, and security activity. It also includes diagnostics for account sync, contacts, avatars, and provider behavior.

## built with

Some of the main libraries and tools Gofer leans on, because pretending I wrote the whole mail stack from scratch would be absurd:

- [templ](https://templ.guide/) for Go-based views
- [templUI](https://templui.io/) for several UI components
- [HTMX](https://htmx.org/) for server-driven interactions
- [Tailwind CSS](https://tailwindcss.com/) for styling
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) for SQLite storage
- [emersion](https://github.com/emersion)'s Go mail libraries, including [go-imap](https://github.com/emersion/go-imap), [go-smtp](https://github.com/emersion/go-smtp), [go-message](https://github.com/emersion/go-message), [go-sasl](https://github.com/emersion/go-sasl), and [go-vcard](https://github.com/emersion/go-vcard), which provide much of Gofer's mail, MIME, auth, and contact-format foundation
- [golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2) for OAuth flows
- [webpush-go](https://github.com/SherClockHolmes/webpush-go) for Web Push notifications
- [Lucide](https://lucide.dev/) icons through templUI's icon component
