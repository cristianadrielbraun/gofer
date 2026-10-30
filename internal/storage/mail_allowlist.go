package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/google/uuid"
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

// MailAllowlistEntry is one row of the allowed mail lists.
type MailAllowlistEntry struct {
	ID        string
	Kind      string
	Value     string
	CreatedBy string
}

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
	Provider       string
	Email          string
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
	// An account with no mail has no address, only its DAV servers.
	if !strings.EqualFold(strings.TrimSpace(mailbox.Provider), providers.ProviderDAV) {
		if err := l.CheckAddress(mailbox.Email); err != nil {
			return err
		}
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
	return l.CheckMailbox(MailAllowlistMailbox{Provider: account.Provider, Email: account.Email,
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

// ListMailAllowlistEntries returns every entry, servers and domains in name
// order.
func (db *DB) ListMailAllowlistEntries(ctx context.Context) ([]MailAllowlistEntry, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT id, kind, value, created_by FROM mail_allowlist_entries ORDER BY kind, value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []MailAllowlistEntry
	for rows.Next() {
		var entry MailAllowlistEntry
		if err := rows.Scan(&entry.ID, &entry.Kind, &entry.Value, &entry.CreatedBy); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// GetMailAllowlist returns the lists as they apply to everyone not exempted.
// Mailboxes check them on every connection, so they are read once and kept
// until changed here.
func (db *DB) GetMailAllowlist(ctx context.Context) (MailAllowlist, error) {
	if cached := db.mailAllowlist.Load(); cached != nil {
		return *cached, nil
	}
	entries, err := db.ListMailAllowlistEntries(ctx)
	if err != nil {
		return MailAllowlist{}, err
	}
	var list MailAllowlist
	for _, entry := range entries {
		switch entry.Kind {
		case MailAllowlistServer:
			list.Servers = append(list.Servers, entry.Value)
		case MailAllowlistDomain:
			list.Domains = append(list.Domains, entry.Value)
		case MailAllowlistProvider:
			list.Gmail = list.Gmail || entry.Value == providers.ProviderGmail
			list.Outlook = list.Outlook || entry.Value == providers.ProviderOutlook
		}
	}
	db.mailAllowlist.Store(&list)
	return list, nil
}

// MailAllowlistForUser returns the lists as they apply to one person: none
// at all when the administrator exempted them.
func (db *DB) MailAllowlistForUser(ctx context.Context, userID string) (MailAllowlist, error) {
	list, err := db.GetMailAllowlist(ctx)
	if err != nil || !list.Limited() {
		return list, err
	}
	exempt, err := db.IsMailAllowlistExempt(ctx, userID)
	if err != nil {
		return MailAllowlist{}, fmt.Errorf("check allowed mail list exemption: %w", err)
	}
	if exempt {
		return MailAllowlist{}, nil
	}
	return list, nil
}

// AddMailAllowlistEntry adds a server or domain, normalized. Adding one that
// is already there does nothing.
func (db *DB) AddMailAllowlistEntry(ctx context.Context, kind, value, createdBy string) (string, error) {
	if kind != MailAllowlistServer && kind != MailAllowlistDomain {
		return "", fmt.Errorf("unknown allowed mail list %q", kind)
	}
	value, err := NormalizeMailAllowlistEntry(kind, value)
	if err != nil {
		return "", err
	}
	_, err = db.write.ExecContext(ctx, `
		INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, value) DO NOTHING`,
		uuid.NewString(), kind, value, strings.TrimSpace(createdBy))
	db.mailAllowlist.Store(nil)
	return value, err
}

// DeleteMailAllowlistEntry removes a server or domain entry.
func (db *DB) DeleteMailAllowlistEntry(ctx context.Context, id string) error {
	_, err := db.write.ExecContext(ctx, `DELETE FROM mail_allowlist_entries WHERE id = ? AND kind != ?`, strings.TrimSpace(id), MailAllowlistProvider)
	db.mailAllowlist.Store(nil)
	return err
}

// SetMailAllowlistProvider lets Gmail or Outlook mailboxes in while servers
// are limited, or stops letting them in.
func (db *DB) SetMailAllowlistProvider(ctx context.Context, provider string, allowed bool, createdBy string) error {
	if provider != providers.ProviderGmail && provider != providers.ProviderOutlook {
		return fmt.Errorf("unknown provider %q", provider)
	}
	defer db.mailAllowlist.Store(nil)
	if !allowed {
		_, err := db.write.ExecContext(ctx, `DELETE FROM mail_allowlist_entries WHERE kind = ? AND value = ?`, MailAllowlistProvider, provider)
		return err
	}
	_, err := db.write.ExecContext(ctx, `
		INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, value) DO NOTHING`,
		uuid.NewString(), MailAllowlistProvider, provider, strings.TrimSpace(createdBy))
	return err
}

// IsMailAllowlistExempt reports whether the allowed mail lists don't apply to
// a person.
func (db *DB) IsMailAllowlistExempt(ctx context.Context, userID string) (bool, error) {
	var exempt int
	err := db.read.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mail_allowlist_exempt_users WHERE user_id = ?)`, strings.TrimSpace(userID)).Scan(&exempt)
	return exempt == 1, err
}

// SetMailAllowlistExempt makes the allowed mail lists not apply to a person,
// or apply again.
func (db *DB) SetMailAllowlistExempt(ctx context.Context, userID string, exempt bool, createdBy string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("user is required")
	}
	if !exempt {
		_, err := db.write.ExecContext(ctx, `DELETE FROM mail_allowlist_exempt_users WHERE user_id = ?`, userID)
		return err
	}
	_, err := db.write.ExecContext(ctx, `
		INSERT INTO mail_allowlist_exempt_users (user_id, created_by) VALUES (?, ?)
		ON CONFLICT(user_id) DO NOTHING`, userID, strings.TrimSpace(createdBy))
	return err
}

// ListMailAllowlistExemptUsers returns the IDs of the people the lists don't
// apply to, sorted.
func (db *DB) ListMailAllowlistExemptUsers(ctx context.Context) ([]string, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT user_id FROM mail_allowlist_exempt_users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, rows.Err()
}
