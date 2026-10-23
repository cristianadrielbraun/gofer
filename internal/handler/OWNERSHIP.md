# HTTP ownership boundaries

`RegisterRoutes` is wrapped by the application authentication middleware. A
route being registered here does not make it public. The categories below are
the ownership inventory and must be updated when a route is added. Authenticated
routes require an active user; pending or disabled users cannot enter the
handler layer through an application session.

## Public and pre-authentication routes

- Static assets: `GET /assets/*` and `GET /sw.js`.
- Application login: `GET`/`POST /login`, password-MFA continuation under
  `/login/mfa`, recovery-code entry at `/login/mfa/recovery`, restricted factor
  repair under `/login/recovery/*`, plus `GET /auth/google` and
  `GET /auth/google/login/callback`.
- First-run setup: `GET`/`POST routes under `/setup`; each continuation is
  available only while setup is uninitialized and requires the matching
  origin-bound pre-authentication challenge.

Public routes must not call private mail, contact, account, settings, avatar,
or operation repositories. OAuth callback state is a pre-authentication
capability and is not an application session. Password-MFA, recovery repair,
and setup challenges likewise carry no authenticated request user and cannot
access private repositories before full completion.

## Signed capability routes

- `GET /api/message-file/{kind}/{messageID}/{name}` serves one inline part or
  saved remote image of a message without a session. A message is shown in a
  sandboxed frame the browser treats as another site, so its image requests
  carry no session cookie. When Gofer builds a message page it signs that
  message's own file addresses with a key that is random for each server
  start; the signature names the owner, message, kind, and file and expires
  after twelve hours. The route serves only a file matching a valid
  signature, as its owner, after the same user and file checks as an owned
  route; anything else is `404`. Responses stay sandboxed and are never
  type-sniffed.
- The `web` kind of the same route relays one remote file of a message the
  reader chose to load. Its signature also names the remote address, so a
  link can only fetch what that message's page asked for. The download goes
  through the same private-network guard and size limit as saved remote
  images, and the result is served as a message file, typed by its content.

## Administrator-only operational routes

- Pages under `/admin`, including avatars, contacts, labels, operations, and
  security.
- Security-exception mutations under `/admin/security/*`. In personal and open
  mode, where Gofer has a single owner, the same mutations are served under
  `/settings/security/mail/*` instead; they return `404` in managed mode.
- `GET /api/admin/mail-operations/status`.
- `GET /api/system/processing`.
- Avatar/contact/label diagnostic endpoints and avatar/contact backfill
  mutations registered through `adminRoute`.

These routes may expose aggregate operational state. Administrator status does
not grant access to another user's message, contact, draft, attachment,
signature, outgoing-send, or provider-avatar content.

## Authenticated shared routes

- `GET /api/push/vapid-public-key` returns the instance's public VAPID key.

No authenticated shared route accepts a private resource identifier.

## Authenticated owned routes

The following route groups derive the user from the authenticated request and
constrain every private lookup or mutation to that user:

- Mail pages and lists: `/`, `/email/*`, `/folder/*`, `/mail/folder/*`,
  `/mail/thread/*`, `/search`, `/api/sidebar/*`, and `/api/folders/unread`.
- Message content and actions: `/api/messages/*`, `/api/attachments/*`,
  `/api/inline-content/*`, `/api/remote-content/*`, and
  `/api/remote-assets/*`.
- Contacts: `/contacts*`, `/api/contacts*`, contact import/export, contact sync
  setup/confirmation, provider sync, suppression, and observed-contact cleanup.
- Calendar: `/calendar*` and `/api/accounts/*/calendar/discover` are rendered
  from or mutate the authenticated user's calendar sources; event/source
  lookups must remain user-scoped.
- Accounts: account discovery/creation/edit/service/color/test/deletion,
  mailbox size, account contact settings, signatures, and `/api/mail/sync*`.
  `GET /api/accounts/server-check` and `POST /api/accounts/test` take mail
  server settings typed into the add-account wizard before any account exists;
  they read only instance mail-security policy, save nothing, and reach servers
  only through the private-network guard. `POST /api/accounts/{id}/test-changes`
  does the same for the edit-account wizard, filling a blank password from the
  current user's own saved account.
- Account OAuth: `/api/accounts/oauth2/authorize`,
  `/auth/google/mailbox/callback`, and `/auth/microsoft/mailbox/callback` use a
  single-use flow bound to the current user, session, and provider.
- Settings, signatures, UI preferences, Web Push subscriptions, and test
  notifications sent to the requesting browser's own subscription.
- Compose, staged compose attachments, drafts, outgoing sends, and mail
  operations.
- `GET /api/events`, whose EventBus messages require explicit account, user,
  multi-user, or administrator scope.
- `GET /api/avatars/{hash}` and `POST /api/avatars/warmup`, which require the
  sender email to be visible through the current user's messages or contacts.
- `GET /api/provider-avatar`, which requires an exact provider URL stored on
  the current user's profile or contact before any outbound request.

Foreign and nonexistent private identifiers must be indistinguishable. Unless
a route explicitly documents another privacy-preserving result (account
deletion status does), both return `404`. Ownership is checked before database
mutation, queue insertion, blob access, translation, remote fetch, or provider
traffic. The same rule applies to administrators.
