# Shared mailboxes

Many mail servers let a person open mailboxes besides their own: a group's
mailbox, such as support@ or sales@, or a colleague's they were given access
to. Gofer finds them, over IMAP or JMAP, shows each under its own entry in the
sidebar, keeps its sent mail and drafts in its own folders, and can make a
From address for it. Each can be turned off.

Nothing needs setting up. It works with any IMAP server that reports where
other people's mailboxes are in `NAMESPACE` (RFC 2342), and any JMAP server
that lists them as accounts of the login (RFC 8620, RFC 9670), which servers
with shared mailboxes do. Both have been checked against Stalwart.

## How shared mailboxes are found

Each time an account's folders sync, Gofer reads the server's `NAMESPACE`
answer. The "other users" and "shared" namespaces it reports are where other
people's mailboxes are. Stalwart, for one, answers:

```
* NAMESPACE (("" "/")) (("Shared Folders" "/")) NIL
```

and lists a group's mailbox below it:

```
* LIST (\NoSelect) "/" "Shared Folders/support@example.com"
* LIST () "/" "Shared Folders/support@example.com/INBOX"
* LIST (\Sent) "/" "Shared Folders/support@example.com/Sent Items"
* LIST (\Drafts) "/" "Shared Folders/support@example.com/Drafts"
```

A folder is in a shared mailbox when its name starts with one of those
namespaces and the next level names the mailbox's owner:

- In the "other users" namespace the owner may be an address, as on Stalwart,
  or a user name, as on Cyrus (`Other Users.support`) and some Dovecot setups.
- In a "shared" namespace, where public folders also live, only an address
  names a mailbox.

Only the server's namespaces decide: a personal folder named after an address
is never taken for a shared mailbox, and no folder name is assumed. A
namespace prefix is matched exactly, as the server names it, so a personal
folder "Shared" is not Dovecot's "shared" namespace.

The owner's own folder (`Other Users.support`, `user.bob`) is the mailbox's
Inbox when the server lets it be opened, as Cyrus does, unless an `INBOX`
inside it is; otherwise, as on Stalwart, it only holds the mailbox's folders.

Within a shared mailbox, each folder's role (Inbox, Sent, Drafts, Junk, Trash,
Archive) comes from its `SPECIAL-USE` attribute (RFC 6154), or else from its
name; there "Deleted Items" and "Junk Mail" are also Trash and Spam, as group
mailboxes often name them. Those two names are not read as roles for the
account's own folders. The role is kept for that mailbox only: the account's own Sent, Drafts
and Trash are always its own folders, never a shared mailbox's.

Gofer asks the server nothing more for this; it uses the folder listing the
sync already reads. A shared mailbox no longer listed is marked removed only
after a listing the server fully answered, so a dropped connection never puts
one aside.

## Over JMAP

A JMAP server lists, in the session of a login, each mailbox shared with it as
one more account, not personal to the login. Stalwart lists a group's mailbox
by its address:

```json
"accounts": {
  "y": {"name": "person@example.com", "isPersonal": true, ...},
  "z": {"name": "support@example.com", "isPersonal": false,
        "accountCapabilities": {"urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:submission": {}, ...}}
}
```

Each such account with mail, named by an address, is a shared mailbox of the
Gofer account. Its mailboxes become its folders, each with its role (`inbox`,
`sent`, `drafts`, `junk`, `trash`, `archive`), synced from cursors of their
own, so one shared mailbox failing leaves the others and the login's own mail
alone. JMAP ids are unique only within an account, so what Gofer stores of a
shared one carries its account in front (`z:a1`).

Everything follows the account the mail is in: marking, moving and deleting
mail, its download, drafts, and folders made inside the shared mailbox. A
message sent from a shared mailbox's address is submitted in that mailbox's
own account, so the server stores it in that mailbox's Sent. Mail moved
between the login's own mailbox and a shared one is copied across
(`Email/copy`) and the original removed, or only taken out of the folder it
left when it is in others too, as a message the shared mailbox both sent and
received is. Push notifications for any of the accounts wake the sync.

The shared mailbox's calendars and address books are listed with the login's
own in the account's Calendar and Contacts settings; events and contacts saved
in them go to its account.

A JMAP server may also list the shared mailboxes' addresses among the login's
own identities. Gofer's From address for a shared mailbox takes their place,
so mail from that address is sent, and filed, as the shared mailbox.

## Choosing which to use

The account wizard (Add account, Edit) has a Shared mailboxes step, second
after Mail, listing the mailboxes the account can open with a box for each;
after a new account is created, the wizard shows it before Contacts. Each
account's card in Settings → Accounts lists them too, faded where turned off,
with a button to that step.

A shared mailbox turned off is not synced, its folders are hidden and its From
address is not offered. Mail is not sent from it, even from a draft written
while it was on. Its folders and identity are kept, so turning it back
on brings them back as they were. Nothing changes on the server.

## In the sidebar

Below the account's own folders, a "Shared mailboxes" entry holds one entry per
shared mailbox, with its name, its address and its unread mail (Sent, Drafts,
Spam and Trash are not counted). Each folder inside shows by its role, such as
Sent for a server folder called "Sent Items"; the server's folder names are
never changed. Both levels fold, and remember whether they were folded.

## Sending as a shared mailbox

When "Add a From address for each shared mailbox" is on, Gofer adds a From
address (identity) to the account for each shared mailbox named by an address. It is named after the mailbox: `support@example.com`
becomes "Example Support" by default, or "Support", or your own text and
"Support" (Settings → Advanced → Shared mailboxes).

- It sends with the account's own SMTP sign-in. No password is made or stored
  for the shared mailbox; the server decides whether the account may send as
  it.
- Gofer makes one identity per mailbox, however often the folders sync.
- An identity you made yourself for the same address is left alone, and none
  is added beside it. A name you changed is kept. One you delete is not made
  again. One you turn off stays off.
- When a mailbox is no longer shared with the account, its identity is put
  aside (not offered when writing) and comes back if the mailbox is shared
  again. It is never deleted.

When "Ask the server about a new From address" is on and an identity is made,
Gofer asks the SMTP server whether it accepts the
address as a sender, giving `MAIL FROM` and `RCPT TO` and then `RSET`, so
nothing is sent. The account's identities show the answer, including the
server's own message when it refuses. It signs in with the account's SMTP
credentials and runs within the sync. Only a permanent refusal (a 5xx reply)
counts as refused; a temporary one (4xx), or a server that can't be reached,
leaves the answer unknown. A real message the server refuses at
`MAIL FROM` updates it too. A refused identity is kept, so you can fix the
server's permissions and send again. Some servers only refuse once the message
itself is sent; for those, the first send tells.

## Replies, sent mail and drafts

- A reply, or reply-all, to a message in a shared mailbox's folder is sent
  from that mailbox by default. You can still choose another From.
- A message sent from a shared mailbox's address is copied to that mailbox's
  Sent folder (IMAP `APPEND`), so its other members see it, not to the
  account's own Sent.
- A draft from a shared mailbox's address is kept in that mailbox's Drafts. If
  you change its From, it moves to the other Drafts folder.

The Sent and Drafts folders are found by their role, not by their name. A
shared mailbox without one uses the account's own.

## Deleting, archiving and spam

Mail deleted, archived or marked as spam in a shared mailbox goes to that
mailbox's own Trash, Archive or Spam folder, found by its role. A shared
mailbox without that folder keeps the mail where it is and says so; the
account's own folder is never used instead, since that would take the mail
out of the shared mailbox. Deleting for good is still offered where there is
no Trash.

## Settings

Settings → Advanced → Shared mailboxes:

| Setting | Default |
| --- | --- |
| Add a From address for each shared mailbox | off |
| Name of a new From address | the server's name, or else one starting with the domain's name |
| Ask the server about a new From address | off |
| Put aside the From address of a mailbox no longer shared | on |
| Show each shared mailbox's address in the sidebar | on |
| Show shared mailboxes' mail in unified folders | off |

## Names

A shared mailbox is shown under the name the user gave it in the account
wizard, or else the server's name for it, or else its address's local part
("Support"). A JMAP server's name is the shared account's own sender name,
which Stalwart takes from a group's description. Mail cards in unified
folders and the Folders dialog mark a shared mailbox's mail and folders with
that name.

A new From address takes the server's name too by default; it follows the
naming setting until the user renames it, and then keeps the user's name.

## Checking a server

`internal/mail/shared_mailboxes_live_test.go` checks the IMAP side against a
real server. It only reads over IMAP and never sends over SMTP. It runs only when
asked for:

```sh
GOFER_LIVE_IMAP_HOST=mail.example.com \
GOFER_LIVE_USER=person@example.com \
GOFER_LIVE_PASSWORD=... \
GOFER_LIVE_EXPECT_SHARED=support@example.com,sales@example.com \
go test -tags livemail -run Live -v ./internal/mail/
```

The file lists the other variables (ports, `starttls`, and an address the
account must not send as).

`internal/mail/jmap_shared_live_test.go` checks the JMAP side against a real
server whose login has a shared mailbox. Unlike the IMAP check it writes: it
saves and removes a draft in the shared mailbox, sends one message from the
shared address to the login's own, moves the received copy into the shared
mailbox and back, saves and removes an event and a contact there, and removes
everything it made:

```sh
GOFER_LIVE_JMAP_HOST=mail.example.com \
GOFER_LIVE_JMAP_USER=person@example.com \
GOFER_LIVE_JMAP_PASSWORD=... \
go test -run TestLiveJMAPSharedMailboxes -v ./internal/mail/
```
