# AI access (MCP)

Gofer serves a [Model Context Protocol](https://modelcontextprotocol.io) server,
so an AI client such as Claude can read your mail, organize it, send it, and
manage your accounts, folders and settings, as you, with only what you allow.

The server is at `https://your-gofer/mcp` (Streamable HTTP). It is available in
personal and open mode. Settings → Advanced → AI access shows the address and
everything let in.

## Connecting a client

**With OAuth (claude.ai, Claude Desktop, ChatGPT and other connector UIs).** Add
`https://your-gofer/mcp` as a custom connector. The client is sent to Gofer: you
sign in as usual (two-factor included), see what it asks for, untick anything
you do not want it to do, and allow it. It then signs itself in and renews its
access on its own. Settings → Advanced → AI access → Connected apps lists it,
and Revoke signs it out at once.

Gofer is its own OAuth 2.1 authorization server, as the MCP authorization spec
describes:

| | |
| --- | --- |
| Protected resource metadata (RFC 9728) | `/.well-known/oauth-protected-resource/mcp` |
| Authorization server metadata (RFC 8414) | `/.well-known/oauth-authorization-server` |
| Dynamic client registration (RFC 7591) | `/oauth/register` |
| Authorization (you sign in and choose here) | `/oauth/authorize` |
| Token (authorization code with PKCE S256, refresh token) | `/oauth/token` |
| Revocation (RFC 7009) | `/oauth/revoke` |

Clients are public (no client secret) and must use PKCE with S256. A redirect
address must be the one the client registered: https, http on the device itself,
or an app's own scheme. Access tokens last an hour; refresh tokens 90 days and
are replaced each time they are used. An authorization code works once, for ten
minutes; one presented again revokes what it was traded for.

**With a token (command-line clients, scripts).** In Settings → Advanced → AI access, make
a token: name it, choose what it may do and when it expires. It is shown once.
Send it as a bearer token:

```sh
claude mcp add --transport http gofer https://your-gofer/mcp \
  --header "Authorization: Bearer gofer_mcp_…"
```

Only a hash of a token is kept. Revoke it there when it is no longer needed.

Gofer must be reachable from the client. A client in the cloud, such as
claude.ai, reaches it over the internet: check `GOFER_ALLOWED_CIDRS` if you set
it.

## What a client may do

Each tool needs a scope, chosen when you allow a client or make a token:

| Scope | Tools |
| --- | --- |
| Read mail (`mail.read`) | `list_accounts`, `list_folders`, `get_unread_summary`, `list_messages`, `search_messages`, `read_message`, `list_tags`, `list_identities`, `list_signatures`, `list_calendars` |
| Organize mail (`mail.organize`) | `mark_messages` (read, unread, starred), `move_messages`, `archive_messages`, `mark_spam`, `tag_messages`, `delete_messages`, `mark_folder_read`, `empty_folder` |
| Send mail (`mail.send`) | `send_message` (now or later, with attachments), `save_draft`, `discard_draft`, `reply_to_message` (and reply-all), `forward_message`, `get_send_status` |
| Manage accounts (`accounts.manage`) | `discover_account_settings`, `test_account_connection` (IMAP/SMTP or JMAP), `add_account` (IMAP/SMTP or JMAP), `update_account`, `set_account_services` (mail, contacts, calendar), `set_shared_mailbox_enabled`, `rename_shared_mailbox`, `set_account_color`, `reorder_accounts`, `sync_accounts`, `remove_account`, `sync_contacts_and_calendars` |
| Manage folders and settings (`settings.manage`) | `create_folder`, `rename_folder`, `move_folder`, `delete_folder`, `set_appearance` (color and icon of a folder or tag), `reorder_folders`, `set_folder_roles`, `rename_tag`, `delete_tag`, `save_identity`, `delete_identity`, `set_default_identity`, `set_shared_identity_enabled`, `save_signature`, `delete_signature`, `set_account_signatures`, `get_settings`, `update_settings`, `subscribe_calendar` |

Every tool does what the same action in Gofer does, with the same checks, and
only on your own mail. Deleting for good, emptying Trash or Spam, deleting a
folder or tag and removing an account need `confirm: true`, so a client asks
you first. Each call is logged with the token's name.

`add_account` finds a new address's IMAP and SMTP servers when none are given,
checks that it can sign in before adding it, and can turn on its contacts and
calendars (CardDAV and CalDAV on the same server). Shared mailboxes the login
can open are found on their own, over IMAP or JMAP ([shared mailboxes](./shared-mailboxes.md)).
`list_accounts` lists them, and those turned off; `set_shared_mailbox_enabled`
turns one on or off.

Replies go out as the shared mailbox the message is in, and mail sent or saved
from a shared mailbox's address goes to that mailbox's Sent or Drafts, as in
Gofer.

A message's text is the sender's, not yours. The server tells clients never to
follow instructions found in an email.

## Limiting a client to some mailboxes

By default a client reaches every mailbox, including ones added later. When you
make a token or allow a client, you can instead tick the mailboxes it may use:
each account's own mailbox, and each shared mailbox the account opens. A client
limited this way:

- sees only those mailboxes' folders, messages, unread counts, tags,
  identities and (with the account's own mailbox) calendars;
- can move mail only between folders of those mailboxes;
- sends, replies and saves drafts only as those mailboxes' addresses: without
  a `from`, it sends as the first allowed account's own address, and a token
  limited to shared mailboxes must name one;
- cannot use the `accounts.manage` and `settings.manage` tools, which reach
  every mailbox, even if it holds those scopes;
- cannot act on whole conversations (`thread: true`), which can span
  mailboxes.

A refreshed OAuth token keeps the mailboxes the user chose when allowing the
client. Settings > AI access lists each token's and client's mailboxes.
