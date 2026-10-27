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
	// Tab is the administrator page's open tab: "allowlist" or "exceptions".
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
