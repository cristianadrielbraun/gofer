# HTTP ownership boundaries

`RegisterRoutes` is wrapped by the application authentication middleware. A
route being registered here does not make it public. The categories below are
the ownership inventory and must be updated when a route is added.

## Public and pre-authentication routes

- Static assets: `GET /assets/*` and `GET /sw.js`.
- Application login: `GET /login`, `GET /auth/google`, and
  `GET /auth/google/callback`.

Public routes must not call private mail, contact, account, settings, avatar,
or operation repositories. OAuth callback state is a pre-authentication
capability and is not an application session.

## Administrator-only operational routes

- Pages under `/admin`, including avatars, contacts, labels, operations, and
  security.
- Security-exception mutations under `/admin/security/*`.
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
- Accounts: account discovery/creation/edit/service/color/test/deletion,
  account contact settings, signatures, and `/api/mail/sync*`.
- Account OAuth: `/api/accounts/oauth2/authorize`,
  `/auth/google/account/callback`, and `/auth/microsoft/account/callback` use a
  single-use flow bound to the current user, session, and provider.
- Settings, signatures, UI preferences, and Web Push subscriptions.
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
