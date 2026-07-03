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

Offering a more complete, multi-user, hosted version in the future is not off the table, it can be an interesting path

For reference, I'm using it actively with 6 configured accounts and about 100k emails in total. So far not a single performance issue or increased memory consumption

## features

Things that already work (well, they work on my machine):

- adding generic IMAP/SMTP accounts
- connecting Gmail accounts through Google OAuth, with mail handled by the Gmail API
- connecting Outlook accounts through Microsoft OAuth, with mail handled by Microsoft Graph
- syncing mail into SQLite, with provider-native sync for Gmail and Outlook and polling/optional IMAP IDLE for generic accounts
- reading messages, threads, cached bodies, inline content, and attachments
- blocking remote content by default, then allowing it per message or sender
- sending mail, including attachments and account signatures
- scheduled send, with scheduled messages shown as a virtual folder
- drafts and compose autosave
- folders, unread state, starred messages, archive, move, spam/not-spam, trash, and delete actions
- quick search plus advanced filters for status, attachments, threads, tags, accounts, dates, people, subject, body, domains, and attachment names
- contacts, including manually saved contacts and observed contacts from mail
- contact import/export with vCard files
- Google Contacts sync through the People API
- Outlook contact sync through Microsoft Graph
- CardDAV contact sync with discovery, multiple address books, pull, push, update, and delete paths
- the Contacts area, in general, aims to offer a "centralized" contacts solution, where you can use Gofer as a source of truth and syncronize with the accounts you want. Interesting idea, I'm not so sure about the execution. We'll see how it progress
- account colors, account testing, account service toggles, and encrypted stored passwords/tokens
- local mode by default, with optional Google login/session auth when configured (I recommend you not to use this for now, very very alpha stage)
- optional browser-tab notifications and Web Push notifications for new mail
- theme, layout, list navigation, compose, signature, contact, sync, timezone, and notification settings
- local cached blobs for message bodies, remote assets, and attachments
- Live translation using the Google Translate public API, for now
- I'm sure I'm forgetting a lot of stuff

## still moving

Things I'm still improving, in no especially noble order:

- smoother first-run setup and OAuth credential guidance
- proper, public implementation of the oauth integration, so you as end user don't need to create your own provider
- clearer diagnostics and reconnect flows
- broader test coverage around provider sync behavior
- deeper labels/tags workflows beyond filtering
- calendar support
- richer regional and language settings
- more keyboard shortcuts, bulk actions, and cleanup flows

Gofer is meant for local use. Please do not expose it directly on a public server. That's a different kind of adventure, meant for future chapters in this story

## running it

Downloaded release binaries include the generated web assets. For development from source, you need Go, `templ`, `tailwindcss`, and `task`.

For development with hot reload:

```sh
task dev
```

Or roughly:

```sh
templ generate
tailwindcss -i ./assets/css/input.css -o ./assets/css/output.css
go run .
```

Then open `http://local.localhost:8090`.

## building it

For a normal local development build:

```sh
task build
```

That runs `templ generate`, builds the Tailwind CSS file, and writes the binary to:

```sh
./tmp/main
```

Then run it with whatever env vars you need:

```sh
GO_ENV=production ./tmp/main
```

For a self-contained release binary with embedded assets:

```sh
task release
```

That writes:

```sh
./dist/gofer
```

For cross-platform release archives:

```sh
VERSION=v0.1.0-alpha.1 task release:all
```

That writes Linux, macOS, and Windows archives plus `dist/checksums.txt`.

## OAuth credentials

Generic IMAP/SMTP accounts do not need OAuth application credentials. Gmail and Outlook do.

For now, alpha builds expect you to provide your own OAuth client ID and client secret for Google and Microsoft. Create credentials in the provider console, configure callback URLs for your `GOFER_BASE_URL`, and keep the generated secrets private. For Google, enable the Gmail API and People API in the same project.

Official provider docs:

- [Google OAuth 2.0 for web server applications](https://developers.google.com/identity/protocols/oauth2/web-server)
- [Microsoft Entra app registration](https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-register-app)

Default local callback URLs:

```text
http://local.localhost:8090/auth/google/account/callback
http://local.localhost:8090/auth/google/callback
http://local.localhost:8090/auth/microsoft/account/callback
```

The Google login callback is only needed if you enable Gofer's optional Google-backed app login (again please don't do this). Gmail account setup uses the Google account callback.

## configuration

Runtime data lives in `data/` by default. That includes the SQLite DB, cached emails, attachments, and the local secret key used for encrypted account passwords.

That directory is ignored by git. Do not commit it.

Useful env vars:

```sh
GO_ENV=development
GOFER_DB_PATH=data/gofer.db
GOFER_SECRET_KEY=64_hex_chars_if_you_want_to_provide_your_own_key
GOFER_AUTH_ENABLED=false
GOFER_BASE_URL=http://local.localhost:8090
GOOGLE_OAUTH_CLIENT_ID=optional_for_google_login_gmail_contacts
GOOGLE_OAUTH_CLIENT_SECRET=optional_for_google_login_gmail_contacts
MICROSOFT_OAUTH_CLIENT_ID=optional_for_outlook_oauth_mail
MICROSOFT_OAUTH_CLIENT_SECRET=optional_for_outlook_oauth_mail
MICROSOFT_OAUTH_TENANT=common
GOFER_VAPID_PUBLIC_KEY=optional_web_push_public_key
GOFER_VAPID_PRIVATE_KEY=optional_web_push_private_key
GOFER_VAPID_SUBJECT=mailto:gofer@gofer.email
```

Google OAuth is used for optional Google login, Gmail mail through the Gmail API, and Google Contacts sync through the People API. Microsoft OAuth is used for Outlook mail and contact sync through Microsoft Graph.

## local security model

Gofer stores mail, cached blobs, account credentials, OAuth tokens, and runtime state locally. The practical security model is simple and very glamorous: run it on a trusted local machine, keep `data/` private, keep OAuth client secrets out of git, and avoid exposing the app directly to the public Internet.

## admin panel

There is also a small `/admin` area for operational bits I did not want to hide in logs forever. It currently has pages for avatar checks, contact sync/backfill status, and label/provider diagnostics, including Gmail API and Outlook Graph parity checks. It is mostly a local debugging cockpit, not a grand enterprise command center, but it is useful when sync feels suspicious.

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
