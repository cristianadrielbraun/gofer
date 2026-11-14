package storage

import (
	"database/sql"
	"errors"
	"net/netip"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// The kinds of allowed mail list entries. A server entry names a host an
// IMAP, SMTP, JMAP, CalDAV or CardDAV connection may go to, a domain entry
// the domain a mailbox's address may have, and a provider entry lets Gmail
// or Outlook mailboxes in while servers are limited.
const (
	MailAllowlistServer   = "server"
	MailAllowlistDomain   = "domain"
	MailAllowlistProvider = "provider"
)

// ErrMailAllowlistEntry means an entry is not a host or domain, or a
// "*." pattern of one.
var ErrMailAllowlistEntry = errors.New("enter a host or domain, or *. followed by one")

// MailAllowlist limits the mail servers mailboxes connect to and the domains
// their addresses have. An empty list limits nothing, so a zero value allows
// everything, as it does for a person the administrator exempted.
type MailAllowlist struct {
	Servers []string
	Domains []string
	Gmail   bool
	Outlook bool
}

// MailAllowlistError says which rule a mailbox broke. Its text is shown to the
// person who owns the mailbox.
type MailAllowlistError struct {
	// Rule is MailAllowlistServer, MailAllowlistDomain or MailAllowlistProvider.
	Rule string
	// Value is the server, the address's domain or the provider name.
	Value string
}

func (e *MailAllowlistError) Error() string {
	switch e.Rule {
	case MailAllowlistDomain:
		return "Your Gofer administrator doesn't allow mailboxes with @" + e.Value + " addresses."
	case MailAllowlistProvider:
		return "Your Gofer administrator doesn't allow " + e.Value + " mailboxes."
	default:
		return "Your Gofer administrator doesn't allow connecting to the mail server " + e.Value + "."
	}
}

// IsMailAllowlistError reports whether err is a refusal by the allowed mail
// lists.
func IsMailAllowlistError(err error) bool {
	var refusal *MailAllowlistError
	return errors.As(err, &refusal)
}

// ServersLimited reports whether connections are limited to the server list.
// Turning on Gmail or Outlook alone limits them too: to that provider.
func (l MailAllowlist) ServersLimited() bool {
	return len(l.Servers) > 0 || l.Gmail || l.Outlook
}

// DomainsLimited reports whether mailbox addresses are limited to the domain
// list.
func (l MailAllowlist) DomainsLimited() bool {
	return len(l.Domains) > 0
}

// Limited reports whether either list limits anything.
func (l MailAllowlist) Limited() bool {
	return l.ServersLimited() || l.DomainsLimited()
}

// AllowsServer reports whether a connection may go to host.
func (l MailAllowlist) AllowsServer(host string) bool {
	if !l.ServersLimited() {
		return true
	}
	host = normalizePrivateTargetHost(host)
	return host != "" && mailAllowlistMatches(l.Servers, host)
}

// CheckServer refuses a host the server list doesn't allow. An empty host is
// a service the mailbox doesn't use.
func (l MailAllowlist) CheckServer(host string) error {
	if strings.TrimSpace(host) == "" || l.AllowsServer(host) {
		return nil
	}
	return &MailAllowlistError{Rule: MailAllowlistServer, Value: normalizePrivateTargetHost(host)}
}

// CheckServerURL refuses the host of a JMAP, CalDAV or CardDAV address the
// server list doesn't allow.
func (l MailAllowlist) CheckServerURL(address string) error {
	address = strings.TrimSpace(address)
	if address == "" || !l.ServersLimited() {
		return nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" {
		return &MailAllowlistError{Rule: MailAllowlistServer, Value: address}
	}
	return l.CheckServer(parsed.Hostname())
}

// CheckAddress refuses a mailbox address whose domain the domain list
// doesn't allow.
func (l MailAllowlist) CheckAddress(address string) error {
	if !l.DomainsLimited() {
		return nil
	}
	domain := ""
	if at := strings.LastIndex(address, "@"); at >= 0 {
		domain = normalizeMailSecurityHost(address[at+1:])
	}
	if domain != "" && mailAllowlistMatches(l.Domains, domain) {
		return nil
	}
	if domain == "" {
		domain = strings.TrimSpace(address)
	}
	return &MailAllowlistError{Rule: MailAllowlistDomain, Value: domain}
}

// CheckLogin refuses the login of an account with no mail when it is an
// address the domain list doesn't allow. A plain username has no domain, so
// only the servers it signs in to are checked.
func (l MailAllowlist) CheckLogin(username string) error {
	if !strings.Contains(username, "@") {
		return nil
	}
	return l.CheckAddress(strings.TrimSpace(username))
}

// CheckProvider refuses Gmail and Outlook mailboxes while servers are limited
// and the administrator hasn't let that provider in. Other providers connect
// to the servers they name, which CheckServer checks.
func (l MailAllowlist) CheckProvider(provider string) error {
	if !l.ServersLimited() {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providers.ProviderGmail:
		if !l.Gmail {
			return &MailAllowlistError{Rule: MailAllowlistProvider, Value: "Gmail"}
		}
	case providers.ProviderOutlook:
		if !l.Outlook {
			return &MailAllowlistError{Rule: MailAllowlistProvider, Value: "Outlook"}
		}
	}
	return nil
}

// MailAllowlistMailbox is what the lists check about a mailbox.
type MailAllowlistMailbox struct {
	Provider string
	Email    string
	// Username is the login of an account with no mail, which its DAV
	// servers know it by.
	Username       string
	IMAPHost       string
	SMTPHost       string
	JMAPSessionURL string
	CardDAVURL     string
	CalDAVURL      string
}

// CheckMailbox refuses a mailbox that breaks either list. Gmail and Outlook
// mailboxes are checked by provider, since they only reach Google and
// Microsoft.
func (l MailAllowlist) CheckMailbox(mailbox MailAllowlistMailbox) error {
	// Local folders reach no server.
	if strings.EqualFold(strings.TrimSpace(mailbox.Provider), providers.ProviderLocal) {
		return nil
	}
	// An account with no mail has no address, only its DAV servers and the
	// login it signs in to them with.
	if strings.EqualFold(strings.TrimSpace(mailbox.Provider), providers.ProviderDAV) {
		if err := l.CheckLogin(mailbox.Username); err != nil {
			return err
		}
	} else if err := l.CheckAddress(mailbox.Email); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(mailbox.Provider)) {
	case providers.ProviderGmail, providers.ProviderOutlook:
		if err := l.CheckProvider(mailbox.Provider); err != nil {
			return err
		}
	default:
		if err := l.CheckServer(mailbox.IMAPHost); err != nil {
			return err
		}
		if err := l.CheckServer(mailbox.SMTPHost); err != nil {
			return err
		}
		if err := l.CheckServerURL(mailbox.JMAPSessionURL); err != nil {
			return err
		}
	}
	if err := l.CheckServerURL(mailbox.CardDAVURL); err != nil {
		return err
	}
	return l.CheckServerURL(mailbox.CalDAVURL)
}

// CheckMailboxTransports is CheckMailbox for an account as administrator
// diagnostics list it.
func (l MailAllowlist) CheckMailboxTransports(account DiagnosticTransportAccount) error {
	return l.CheckMailbox(MailAllowlistMailbox{Provider: account.Provider, Email: account.Email, Username: account.Username,
		IMAPHost: account.IMAPHost, SMTPHost: account.SMTPHost, JMAPSessionURL: account.JMAPSessionURL,
		CardDAVURL: account.CardDAVURL, CalDAVURL: account.CalDAVURL})
}

// mailAllowlistMatches reports whether value is in the list, exactly or under
// a "*." pattern. A pattern matches the names below its domain, not the
// domain itself, so example.org needs its own entry.
func mailAllowlistMatches(list []string, value string) bool {
	for _, entry := range list {
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if strings.HasSuffix(value, "."+suffix) {
				return true
			}
			continue
		}
		if entry == value {
			return true
		}
	}
	return false
}

// NormalizeMailAllowlistEntry lowercases a server or domain entry and checks
// that it is a host name, an IP address for a server, or a "*." pattern of a
// name.
func NormalizeMailAllowlistEntry(kind, value string) (string, error) {
	value = normalizePrivateTargetHost(value)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "http://")
	if strings.HasPrefix(value, "@") && kind == MailAllowlistDomain {
		value = value[1:]
	}
	name, wildcard := strings.CutPrefix(value, "*.")
	if kind == MailAllowlistServer && !wildcard {
		if _, err := netip.ParseAddr(name); err == nil {
			return value, nil
		}
	}
	if !validMailAllowlistName(name) {
		return "", ErrMailAllowlistEntry
	}
	return value, nil
}

func validMailAllowlistName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}

// migrateV130ToV131 adds the allowed mail server and domain lists and the
// people they don't apply to. Both lists start empty, so nothing is limited.
func migrateV130ToV131(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 131 {
		return nil
	}
	for _, statement := range []string{mailAllowlistEntriesTableSQL, mailAllowlistExemptUsersTableSQL} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 131); err != nil {
		return err
	}
	return tx.Commit()
}

const mailAllowlistEntriesTableSQL = `CREATE TABLE IF NOT EXISTS mail_allowlist_entries (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('server', 'domain', 'provider')),
    value TEXT NOT NULL CHECK (value <> ''),
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (kind != 'provider' OR value IN ('gmail', 'outlook')),
    UNIQUE(kind, value)
)`

const mailAllowlistExemptUsersTableSQL = `CREATE TABLE IF NOT EXISTS mail_allowlist_exempt_users (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`
