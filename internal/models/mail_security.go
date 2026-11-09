package models

import "time"

const (
	MailSecurityExceptionHTTPDiscovery      = "http_discovery"
	MailSecurityExceptionPlaintextTransport = "plaintext_transport"
	MailSecurityExceptionPrivateTarget      = "private_target"
)

type MailSecurityException struct {
	ID        string
	Kind      string
	Protocol  string
	Host      string
	Port      int
	CreatedBy string
	CreatedAt time.Time
	Accounts  []MailSecurityExceptionAccount
	// OutsideAllowlist marks an exception for a server the allowed mail
	// servers leave out, so it lets nothing connect.
	OutsideAllowlist bool
}

type MailSecurityExceptionAccount struct {
	ID    string
	Email string
}

type MailSecurityAdminData struct {
	// ActionBase is where the panel's forms post: the admin page in managed
	// and open mode, the owner's security settings in personal mode.
	ActionBase string
	Exceptions []MailSecurityException
	// Allowlist is the allowed mail servers and domains, which only an
	// administrator of a managed installation sets; nil elsewhere.
	Allowlist *MailAllowlistAdminData
	// Proxies are the saved outbound proxies accounts can connect through.
	Proxies []OutboundProxyView
	// ProxyPeople is set on the administrator's page of a managed
	// installation, where a proxy can be for chosen people and people can be
	// let to add their own.
	ProxyPeople bool
	// PeopleMayAddProxies is whether people may add proxies of their own.
	PeopleMayAddProxies bool
	// OwnProxies is set when the panel lists the proxies a person saved for
	// their own accounts, rather than an administrator's.
	OwnProxies bool
	// Tab is the administrator page's open tab: "allowlist", "exceptions"
	// or "proxies".
	Tab            string
	Notice         string
	Error          string
	StepUpRequired bool
	CSRFTokens     map[string]string
}

// MailAllowlistAdminData is the allowed mail lists on the administrator's
// Mail security page.
type MailAllowlistAdminData struct {
	Servers []MailAllowlistAdminEntry
	Domains []MailAllowlistAdminEntry
	Gmail   bool
	Outlook bool
	// Exempt are the people the lists don't apply to, and Candidates the
	// others who could be exempted.
	Exempt     []AdminWebmailUserOption
	Candidates []AdminWebmailUserOption
	// Refused are the mailboxes the lists stop from connecting.
	Refused []MailAllowlistRefusedAccount
}

type MailAllowlistAdminEntry struct {
	ID    string
	Value string
}

type MailAllowlistRefusedAccount struct {
	ID       string
	Email    string
	Username string
	Reason   string
}

// OutboundProxyView is a saved outbound proxy as the Mail security page shows
// it. Its password is never shown, only whether it has one.
type OutboundProxyView struct {
	ID          string
	Name        string
	Type        string
	Host        string
	Port        int
	Username    string
	HasPassword bool
	IsDefault   bool
	// Everyone is whether an administrator's proxy is for everyone; People
	// are those chosen for it when it isn't, and Candidates the others.
	Everyone   bool
	People     []AdminWebmailUserOption
	Candidates []AdminWebmailUserOption
}

// AccountProxyData is the dialog where a person picks the proxy one of their
// accounts connects through.
type AccountProxyData struct {
	AccountID string
	Name      string
	Email     string
	Color     string
	// Choice is "" for the default, "direct", or a proxy's id.
	Choice  string
	Proxies []OutboundProxyView
	// Default is the default proxy's name, or "" for direct connections.
	Default string
	// ManageURL is where this person adds and changes proxies, or "" when
	// only an administrator can.
	ManageURL string
	Saved     bool
	Error     string
	// Form is the form a Connect through field being added with belongs to,
	// when it sits outside it; Sidebar is set when it sits in a wizard's
	// sidebar, as a setting of the whole account.
	Form    string
	Sidebar bool
}
