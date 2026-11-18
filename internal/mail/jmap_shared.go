package mail

import (
	"context"
	"fmt"
	netmail "net/mail"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A JMAP login may open mailboxes besides its own: a server such as Stalwart
// lists each mailbox shared with the login as one more account in the
// session, not personal to it (RFC 8620 section 2, RFC 9670). Gofer shows
// each as a shared mailbox of the Gofer account, as IMAP's shared namespace
// is shown.
//
// JMAP ids are unique only within their account, so what Gofer stores of a
// shared account (its mailboxes, emails and threads) carries the account's
// id in front: "<account>:<id>". JMAP ids never hold a colon (RFC 8620
// section 1.2). What Gofer stores of the login's own account stays as the
// server gave it, as it always has.

// jmapSharedSeparator joins a shared account's id to one of its ids.
const jmapSharedSeparator = ":"

// jmapTarget is one JMAP account an account syncs: the login's own (mailbox
// "") or a shared mailbox, by address.
type jmapTarget struct {
	account string
	mailbox string
}

// shared reports whether the target is a shared mailbox.
func (t jmapTarget) shared() bool { return t.mailbox != "" }

// ref gives the id Gofer stores for one of the target's ids.
func (t jmapTarget) ref(id string) string {
	if !t.shared() || id == "" {
		return id
	}
	return t.account + jmapSharedSeparator + id
}

// owns reports whether a stored id is one of the target's, and gives the
// server's id.
func (t jmapTarget) owns(stored string) (string, bool) {
	account, id := jmapSplitRef(stored)
	if t.shared() {
		return id, account == t.account
	}
	return id, account == ""
}

// scope names one of the target's sync cursors.
func (t jmapTarget) scope(name string) string {
	if !t.shared() {
		return name
	}
	return name + "@" + t.account
}

// jmapSplitRef splits a stored id into the shared account it is in, "" for
// the login's own, and the server's id.
func jmapSplitRef(stored string) (account, id string) {
	if account, id, ok := strings.Cut(stored, jmapSharedSeparator); ok {
		return account, id
	}
	return "", stored
}

// jmapSharedAccounts returns the mailboxes shared with the login: the
// session's accounts that are not its own and hold mail, by address. An
// account named without an address gives no mailbox to show or send from.
func jmapSharedAccounts(session *jmap.Session) []jmapTarget {
	if session == nil {
		return nil
	}
	own := session.PrimaryAccount(jmap.CapabilityMail)
	var out []jmapTarget
	for id, account := range session.Accounts {
		if id == own || account.IsPersonal {
			continue
		}
		if _, ok := account.AccountCapabilities[jmap.CapabilityMail]; !ok {
			continue
		}
		address := strings.ToLower(strings.TrimSpace(account.Name))
		if !strings.Contains(address, "@") || strings.ContainsAny(address, " \t") {
			continue
		}
		out = append(out, jmapTarget{account: id, mailbox: address})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mailbox < out[j].mailbox })
	return out
}

// jmapAccountOf gives the JMAP account a stored id is in: the shared account
// it names, which the session must still list, or the login's own.
func jmapAccountOf(session *jmap.Session, stored string) (account, id string, err error) {
	if session.PrimaryAccount(jmap.CapabilityMail) == "" {
		return "", "", fmt.Errorf("the JMAP server offers no mail account for this login")
	}
	return jmapCapabilityAccountOf(session, jmap.CapabilityMail, stored)
}

// jmapSenderAccount gives the JMAP account to send from as address: the
// shared mailbox of that address when the login may submit there, so the
// message is stored in that mailbox's own Sent, or else the login's own.
func jmapSenderAccount(session *jmap.Session, address string) jmapTarget {
	if parsed, err := netmail.ParseAddress(address); err == nil {
		address = parsed.Address
	}
	address = strings.ToLower(strings.TrimSpace(address))
	for _, target := range jmapSharedAccounts(session) {
		if target.mailbox != address {
			continue
		}
		if _, ok := session.Accounts[target.account].AccountCapabilities[jmap.CapabilitySubmission]; ok {
			return target
		}
	}
	return jmapTarget{account: session.PrimaryAccount(jmap.CapabilityMail)}
}

// jmapRefIn gives the id Gofer stores for an id of a JMAP account: as it is
// for the login's own account, with the account in front for a shared one.
func jmapRefIn(session *jmap.Session, account, id string) string {
	return jmapCapabilityRef(session, jmap.CapabilityMail, account, id)
}

// jmapIDsIn gives the server's ids of stored ids that must all be in one
// JMAP account, and that account.
func jmapIDsIn(session *jmap.Session, stored []string) (string, []string, error) {
	if len(stored) == 0 {
		return "", nil, fmt.Errorf("JMAP email identity is unavailable")
	}
	account, _, err := jmapAccountOf(session, stored[0])
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(stored))
	for _, ref := range stored {
		in, id, err := jmapAccountOf(session, ref)
		if err != nil {
			return "", nil, err
		}
		if in != account {
			return "", nil, fmt.Errorf("the emails of one change are in different JMAP accounts")
		}
		ids = append(ids, id)
	}
	return account, ids, nil
}

// openJMAPSession opens the account's JMAP session.
func openJMAPSession(ctx context.Context, cfg *models.AccountConfig, password string) (*jmap.Client, *jmap.Session, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, nil, err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, nil, fmt.Errorf("JMAP session: %w", err)
	}
	return client, session, nil
}

// jmapSharedAccountsWith returns the accounts shared with the login that
// offer capability, such as calendars, by id, in a stable order.
func jmapSharedAccountsWith(session *jmap.Session, capability string) []string {
	own := session.PrimaryAccount(capability)
	var out []string
	for id, account := range session.Accounts {
		if id == own || account.IsPersonal {
			continue
		}
		if _, ok := account.AccountCapabilities[capability]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// jmapCapabilityAccountOf gives the JMAP account a stored id of capability
// is in, its own primary account when the id names none.
func jmapCapabilityAccountOf(session *jmap.Session, capability, stored string) (account, id string, err error) {
	shared, id := jmapSplitRef(stored)
	if shared == "" {
		return session.PrimaryAccount(capability), id, nil
	}
	if _, ok := session.Accounts[shared].AccountCapabilities[capability]; !ok {
		return "", "", fmt.Errorf("the shared account %s no longer offers this to the login", shared)
	}
	return shared, id, nil
}

// jmapCapabilityRef gives the id Gofer stores for an id of a JMAP account
// offering capability.
func jmapCapabilityRef(session *jmap.Session, capability, account, id string) string {
	if id == "" || account == session.PrimaryAccount(capability) {
		return id
	}
	return account + jmapSharedSeparator + id
}
