# Setting up Gofer

Everything about configuring, upgrading, and deploying Gofer. For a quick start, see the [README](../README.md).

## auth modes and storage

Gofer runs in `personal` mode by default: one protected profile with multiple mailboxes. Choose `GOFER_AUTH_MODE=managed` for multiple users with separate administrator accounts. Both modes guide you through first-run setup using a token printed in the terminal. `GOFER_AUTH_MODE=open` has no login and is intended for local use. See [`.env.example`](../.env.example) for configuration options.

Managed mode keeps each user's mail, contacts, Calendar and preferences in a separate SQLite database. Authentication and installation settings stay in the central database. New installations initialize this layout automatically.

Upgrading a managed installation from before 0.5.0, or switching an existing open or personal installation to managed mode? Gofer converts the database on the first start, one way only. Read [pre-0.5.0-migration.md](./pre-0.5.0-migration.md) and back up first.

## accounts

Generic IMAP/SMTP accounts need no OAuth application credentials. Gmail and Outlook currently require your own provider client ID and secret.

iCloud is an IMAP/SMTP account with Apple's servers filled in. Apple doesn't let other mail apps sign in with your Apple Account password, so create an app-specific password at [account.apple.com](https://account.apple.com) under **Sign-In and Security** (this needs two-factor authentication) and enter it when adding the account. Contacts and Calendar use the same login through CardDAV and CalDAV. Changing your Apple Account password revokes app-specific passwords, so you'll need to create a new one and enter it under **Edit account**. iCloud+ custom-domain addresses should work by signing in with your @icloud.com address, but haven't been tested yet. Mailbox authorization and optional Google/Microsoft application sign-in use separate clients and callbacks.

> [!NOTE]
> I plan to add Gmail and Outlook connections that don't require you to provide your own OAuth app credentials. First, I need to research the right way to set up public OAuth applications and complete the providers' identity verification and any required app reviews. Until then, you'll need to use your own credentials as described above.

Configure Calendar under **Settings → Accounts**: enable it for an account, discover calendars, and select which ones to sync. Google and Outlook use the account's existing OAuth connection; reconnect older accounts to grant Calendar access if needed. Generic accounts can connect a CalDAV server using an HTTPS URL and server credentials, which may require an app password. iCloud accounts connect to iCloud's CalDAV server with the account's app-specific password.

## deployment precautions

For server deployments, use personal or managed authentication, serve the application through an HTTPS reverse proxy, and configure `GOFER_ADDR` and `GOFER_BASE_URL` for your deployment. The listener binds to loopback by default. Set `GOFER_TRUSTED_PROXY_CIDRS` only for your actual proxy peers; use `GOFER_ALLOWED_CIDRS` when you need to restrict client networks.

To sign in over plain HTTP on a home or office network, set `GOFER_BASE_URL` to the server's private address and `GOFER_ALLOW_INSECURE_LAN=true`. Sign-in cookies then travel unencrypted on that network, and Gofer logs a warning at startup; public addresses and host names still require HTTPS.

Gofer doesn't connect to servers on private network addresses unless they are approved. If your mail, calendar, or contacts server is on your local network, approve it under Mail security: in Settings, Security for personal and open installations, or in the admin panel in managed mode.

Gofer looks up sender pictures from Gravatar, Libravatar, BIMI, and the sender domain's website icon. The last two contact servers chosen by whoever sent the mail. To keep Gofer from reaching them, set `GOFER_AVATAR_PROVIDERS=gravatar,libravatar`, or `none` to turn sender picture lookups off.

Runtime data is stored in `data/` by default. Protect the data directory, configuration, and secrets, and back them up together before upgrades. Shared databases apply schema migrations at startup, and managed mode converts them to per-user storage as described in [pre-0.5.0-migration.md](./pre-0.5.0-migration.md). Going back to an older version may require restoring its matching backup, and the retained shared database does not include changes made after conversion. I recommend testing your providers and access setup before relying on Gofer for anything critical.

## outlook change notifications

Outlook accounts are checked for new mail on a schedule, and more often while Gofer is open. If your installation is reachable from the internet over HTTPS, Microsoft can instead tell Gofer about changes in any folder as they happen. Set `GOFER_OUTLOOK_WEBHOOKS=true` and Gofer subscribes each Outlook account that receives mail, renews the subscriptions in the background, and syncs an account when Microsoft reports a change. This needs:

- Personal or managed authentication.
- A `GOFER_BASE_URL` on a public host name or address, served over HTTPS with a certificate Microsoft trusts. Self-signed certificates and private addresses don't work.
- `/webhooks/outlook` reachable from the internet without signing in. If your reverse proxy adds its own login or an IP allowlist, exempt this path. Gofer only accepts notifications signed for one of its own subscriptions.
- `GOFER_OUTLOOK_WEBHOOKS_SKIP_ALLOWED_CIDRS=true` if you set `GOFER_ALLOWED_CIDRS`, so Microsoft's servers can reach that path. Gofer logs a warning at startup when it's missing.

To check the path from outside your network, run `curl -X POST 'https://your-host/webhooks/outlook?validationToken=hello'`; it should print `hello`. Gofer logs `outlook webhook account=…: subscribed until …` when a subscription is made. If Microsoft can't reach Gofer, subscribing fails, Gofer tries again later, and the scheduled checks keep working meanwhile.

## docker

Images for amd64 and arm64 are published to `ghcr.io/cristianadrielbraun/gofer` with each release. The quickest start is the example [`compose.yaml`](../compose.yaml): set `GOFER_BASE_URL` to the address you'll open in your browser, then run

```sh
docker compose up -d
docker compose logs gofer   # shows the first-run setup token
```

The image differs from a plain binary in a few defaults: it runs in `personal` mode, listens on all addresses inside the container on port 8090, and keeps its data in the `/data` volume. Every setting from [`.env.example`](../.env.example) can be passed as an environment variable, or from a file with `env_file`.

The example publishes the port on `127.0.0.1` only, for an HTTPS reverse proxy on the same machine. That proxy reaches Gofer through the Docker network gateway, so set `GOFER_TRUSTED_PROXY_CIDRS: 172.16.0.0/12` to pass on the real client address. To reach Gofer from other machines without a proxy, publish `"8090:8090"` instead, and see the plain HTTP note above.

If the setup token is lost or expired, rotate it with Gofer stopped:

```sh
docker compose stop gofer
docker compose run --rm gofer auth setup-token rotate
docker compose start gofer
```

Your mail, settings, and encryption keys all live in the volume. Back it up with Gofer stopped. `latest` follows each new release; pin a version such as `ghcr.io/cristianadrielbraun/gofer:0.9` if you'd rather choose when to upgrade. If you update images automatically, read the upgrade notes above first, and consider `GOFER_PREVENT_STORAGE_CONVERSION=true` in managed mode.

## admin panel

In managed mode, `/admin` provides user administration, invitations, security policies, and security activity. It also includes diagnostics for account sync, contacts, avatars, and provider behavior.

