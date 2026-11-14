package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The settings policies decide. Each is declared once, here; the console,
// the resolver and the audit log all read these declarations.

// MFARequired requires two-factor sign-in. Administrators need it
// whatever this says. The Default policy's value is the instance's
// "require two-factor sign-in of everyone" (auth_system_state.mfa_policy),
// which no other policy can turn off for anyone.
var MFARequired = Bool(Spec{
	Key: "auth.mfa_required", Category: CategorySignIn, Kind: KindSecurity,
	Label: "admin.policy.setting.mfa_required", Help: "admin.policy.setting.mfa_required.help",
}, false).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var policy string
	err := q.QueryRowContext(ctx, `SELECT mfa_policy FROM auth_system_state WHERE id = 1`).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read the instance two-factor policy: %w", err)
	}
	everyone := policy == "all_users"
	return json.RawMessage(strconv.FormatBool(everyone)), everyone, true, nil
})

// SecurityRetentionDays is how long sign-in and security events are kept
// (auth_system_state.auth_event_retention_days).
var SecurityRetentionDays = Int(Spec{
	Key: "audit.security_retention_days", Category: CategoryActivity, Kind: KindSecurity, InstanceOnly: true,
	Label: "admin.policy.setting.security_retention_days", Help: "admin.policy.setting.security_retention_days.help",
}, 180, 1, 365).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var days int
	err := q.QueryRowContext(ctx, `SELECT auth_event_retention_days FROM auth_system_state WHERE id = 1`).Scan(&days)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read the security event retention: %w", err)
	}
	return json.RawMessage(strconv.Itoa(days)), false, true, nil
})

// OwnProxies lets people add proxies of their own for their mailboxes
// (outbound_proxy_policy.people_add).
var OwnProxies = Bool(Spec{
	Key: "mail.own_proxies", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.own_proxies", Help: "admin.policy.setting.own_proxies.help",
}, false).adapt(func(ctx context.Context, q Querier) (json.RawMessage, bool, bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT people_add = 1 FROM outbound_proxy_policy WHERE singleton = 1`).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read whether people may add proxies: %w", err)
	}
	return json.RawMessage(strconv.FormatBool(allowed)), false, true, nil
})

// The allowed mail lists, which a mailbox is checked against when it's
// saved and each time it connects (storage.MailAllowlist). An empty list
// limits nothing.
const maxMailListEntries = 5000

// MailAllowedServers are the servers IMAP, SMTP, JMAP, CalDAV and CardDAV
// connections may go to, each a host, an IP address or "*." and a domain.
var MailAllowedServers = StringList(Spec{
	Key: "mail.allowed_servers", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.mail_allowed_servers", Help: "admin.policy.setting.mail_allowed_servers.help",
}, func(entry string) (string, error) {
	return storage.NormalizeMailAllowlistEntry(storage.MailAllowlistServer, entry)
}, maxMailListEntries)

// MailAllowedDomains are the domains a mailbox's address may have.
var MailAllowedDomains = StringList(Spec{
	Key: "mail.allowed_address_domains", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.mail_allowed_address_domains", Help: "admin.policy.setting.mail_allowed_address_domains.help",
}, func(entry string) (string, error) {
	return storage.NormalizeMailAllowlistEntry(storage.MailAllowlistDomain, entry)
}, maxMailListEntries)

// MailAllowedProviders are Gmail and Outlook, when they're let in while
// connections are limited. Either alone limits connections to itself.
var MailAllowedProviders = StringList(Spec{
	Key: "mail.allowed_providers", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.mail_allowed_providers", Help: "admin.policy.setting.mail_allowed_providers.help",
}, func(entry string) (string, error) {
	switch provider := strings.ToLower(strings.TrimSpace(entry)); provider {
	case providers.ProviderGmail, providers.ProviderOutlook:
		return provider, nil
	}
	return "", errors.New("not Gmail or Outlook")
}, 2)
