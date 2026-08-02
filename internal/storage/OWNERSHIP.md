# Repository ownership boundaries

Authenticated handlers must use repository methods that accept `userID` or a
`ForUser` suffix and enforce ownership in SQL. A preceding handler check is not
a substitute for a constrained mutation when a user-scoped repository method
can perform the operation atomically.

## Browser-safe boundaries

The browser-facing handler layer uses these ownership shapes:

- Messages, threads, bodies, fetch information, remote content, and
  attachments join through `accounts.user_id` and exclude deleting accounts.
- Accounts and folders are loaded from `GetAccounts*`, `GetAccountIDs`,
  `GetEmailSyncAccountIDs`, `ResolveFolderIDForUser`, or the account store's
  `GetAccountByIDForUser`.
- Contacts, contact profiles/cards/fields, signatures, settings, suppressed
  contacts, and Web Push subscriptions carry `userID` directly.
- Draft, outgoing-send, and mail-operation HTTP reads and retries use their
  `ForUser` variants or begin with an owned message/account lookup.
- Avatar delivery uses `GetSenderAvatarByHashForUser`,
  `IsSenderAvatarEmailVisibleToUser`, or
  `IsProviderAvatarURLVisibleToUser`. Global avatar-cache methods are not
  browser authorization boundaries.

## Trusted unscoped worker boundaries

These methods intentionally have no browser user argument. Their callers and
preconditions are part of their contract:

- `ClaimDueOutgoingSends` and `ClaimDueSentCopies`: outgoing and sent-copy
  workers only. Queue creation/retry is user-scoped; claims exclude deleting
  accounts.
- `ClaimDueMessageMutations`: message-mutation worker only. Authenticated
  enqueue methods validate all targets first; claims exclude deleting accounts.
- `ClaimDueIMAPDraftOperations`: IMAP draft worker only. Draft handlers first
  establish message/account ownership; claims exclude deleting accounts.
- `ClaimContactSyncOperations`: contact-sync worker only. The worker re-reads
  the current user-owned contact and current target accounts before provider
  traffic.
- `ListDueLabelMutations`: provider sync worker only. `accountID` comes from an
  active account sync path enumerated by `GetAllEmailSyncAccountIDs`, which
  excludes deleting accounts.
- Queue completion, retry-state, provider UID, folder-state, body persistence,
  and attachment persistence methods are called only after one of the claims
  above or after an owned/account-scoped provider sync has established the
  account and message identity.
- `GetEmailByID`, `GetMessageFetchInfo`, `GetMessageMutationInfo`,
  `GetMessageMutation`, `GetDraftProviderInfo`, `GetMessageLocalIDByInternetID`,
  `OutgoingSendForMessage`, and unscoped draft/body mutation helpers are
  internal continuations only. Browser handlers must begin with the matching
  `ForUser` lookup or an owned account and must not accept a global ID directly
  into these methods.
- `GetAllAccountIDs` and `GetAllEmailSyncAccountIDs` are startup/background
  enumerators. They are not valid sources for a browser response.
- Global avatar candidate/cache methods are used by avatar warmup/backfill and
  administrator diagnostics. User delivery is separately authorization
  checked.
- Global mail-operation, avatar, contact, label, retention, threading, and
  security-exception diagnostics are administrator-only and expose operational
  metadata rather than private message content.

When a new unscoped method is introduced, it must be added here with its exact
trusted caller and the ownership check that occurs before any side effect.

