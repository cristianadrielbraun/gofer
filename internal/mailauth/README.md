# Mailbox credential boundary

This package owns OAuth grants used to access connected Gmail and Microsoft
mailboxes. It stores and refreshes provider access tokens, selects the scopes
needed by mail and contact operations, and owns the authenticated account-OAuth
flow state.

`internal/auth` owns application users, login sessions, request identity, and
application-login behavior. A Gofer session must never be accepted as a mailbox
credential, and a mailbox OAuth grant must never establish an application
session by itself.

Handlers and background mail workers receive this service separately from the
application authentication manager. Provider operations should depend on the
narrow token-provider interfaces in `internal/mail`, not on `auth.Manager`.

Application login clients are configured separately in `internal/auth`. A
successful application login neither stores its provider tokens in
`oauth_accounts` nor creates a mailbox. Gmail and Outlook grants enter this
package only through the authenticated account-authorization callbacks.
