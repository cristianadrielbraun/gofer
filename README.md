<h1>
  <img src="./assets/logo.svg" width="48" align="absmiddle" alt="Gofer logo" />
  Gofer
</h1>

| Minimal light | Classic dark |
| --- | --- |
| <img alt="Gofer email card view in the minimal light theme" src="./screenshots/emails-card-minimal-light.png" /> | <img alt="Gofer email card view in the classic dark theme" src="./screenshots/emails-card-classic-dark.png" /> |

[View all screenshots](./screenshots/README.md)

<br>

Gofer is a local-first email client I work on as a side project. It's built with Go, templ views, HTMX-style interactions, and SQLite storage.

It is meant to run on your own machine, keep mail and related data local, and talk directly to mail and contact providers. Generic accounts use IMAP/SMTP. Gmail uses the Gmail API and Google People API. Outlook uses Microsoft Graph.

The project is in alpha, but it is already useful for real local mail. It started as a small mail thing and then, predictably, became a slightly larger mail thing. I'm keeping it light for now, so expect things to keep changing as the app settles.

For reference, I'm using it actively with 6 configured accounts and about 100k emails in total. So far not a single performance issue or increased memory consumption

## features

Things that already work (well, they work on my machine):

- **Accounts and sync:** multiple IMAP/SMTP, Gmail, and Outlook accounts, with mail cached locally in SQLite.
- **Reading and sending:** threads, attachments, drafts with autosave, signatures, scheduled send, and translation.
- **Organization and search:** folders, stars, archive, spam controls, and advanced search filters.
- **Contacts:** local address books, vCard import/export, Google, Outlook, and CardDAV sync, plus Gofer Sync for automatic contact syncing between accounts.
- **Security:** encrypted stored credentials, remote content blocked by default, and optional authentication with passwords, TOTP, passkeys, or external sign-in.
- **Access modes:** no-login local use, one protected personal profile, or managed users with separate administrators.
- **Customization:** themes, layouts, account colors, regional settings, and browser/Web Push notifications.

## still moving

Things I'm still improving, in no particular order:

- smoother first-run setup and OAuth credential guidance
- proper, public implementation of the oauth integration, so you as end user don't need to create your own provider
- clearer diagnostics and reconnect flows
- broader test coverage around provider sync behavior
- deeper labels/tags workflows beyond filtering
- calendar support
- richer regional and language settings
- more keyboard shortcuts, bulk actions, and cleanup flows

Local use is the default. Personal and managed modes also support authenticated remote access with HTTPS and explicit configuration; the project is still alpha.

## running and building

Downloaded release binaries include the web assets. Development from source requires Go, `templ`, `tailwindcss`, and `task`.

```sh
task dev      # development server with hot reload
task build    # local build at ./tmp/main
task release  # self-contained binary at ./dist/gofer
```

With the development server running, open `http://local.localhost:8090`. See the [Taskfile](./Taskfile.yml) for packaging and other build tasks.

## setup and configuration

Gofer runs locally without a login by default. It also supports personal mode (one protected profile with multiple mailboxes) and managed mode (separate administrators and webmail users). See [`.env.example`](./.env.example) for configuration options. Authenticated modes guide you through first-run setup using a token printed in the terminal.

Generic IMAP/SMTP accounts need no OAuth application credentials. Gmail and Outlook currently require your own provider client ID and secret. Mailbox authorization and optional Google/Microsoft application sign-in use separate clients and callbacks.

Runtime data is stored in `data/` by default. Keep it and your secrets private. The default listener is loopback-only; remote access requires explicit configuration.

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
