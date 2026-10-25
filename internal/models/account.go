package models

import "time"

type AccountConfig struct {
	AccountID          string
	Provider           string
	ProviderAccountID  string
	IMAPHost           string
	IMAPPort           int
	IMAPTLSMode        string
	IMAPAllowPlaintext bool
	// IMAPAllowPrivate is set when an administrator approved this private
	// server; otherwise Gofer refuses to connect to private addresses.
	IMAPAllowPrivate   bool
	SMTPHost           string
	SMTPPort           int
	SMTPTLSMode        string
	SMTPAllowPlaintext bool
	SMTPAllowPrivate   bool
	Username           string
	AuthMethod         string
	SmtpUsername       string
	// JMAPSessionURL is where a JMAP account's session is fetched from; it is
	// empty for every other provider.
	JMAPSessionURL string
	// JMAPAllowPlaintext and JMAPAllowPrivate are set when an administrator
	// approved plain HTTP or a private address for the session URL's server.
	JMAPAllowPlaintext bool
	JMAPAllowPrivate   bool
}

type ConnectionTestResult struct {
	Success bool   `json:"success"`
	Service string `json:"service"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	// Warning is a problem a successful connection still has, such as a
	// login that can read mail but not send it.
	Warning string `json:"warning,omitempty"`
}

type EditAccountData struct {
	AccountID         string
	Provider          string
	ProviderAccountID string
	EmailAddress      string
	DisplayName       string
	IMAPHost          string
	IMAPPort          int
	IMAPTLSMode       string
	SMTPHost          string
	SMTPPort          int
	SMTPTLSMode       string
	Username          string
	AuthMethod        string
	SmtpUsername      string
	JMAPSessionURL    string
	SameSmtpAuth      bool
	EmailSyncEnabled  bool
	// ReconnectPending is set for an account imported from a profile that
	// still needs its password or sign-in.
	ReconnectPending bool
	// ReconnectNeeds are the logins the imported account still waits for.
	// ReconnectSMTP is set when its sending login is separate and is entered
	// with the mail one; ReconnectContacts and ReconnectCalendar when those
	// services come back on with it.
	ReconnectNeeds    []string
	ReconnectSMTP     bool
	ReconnectContacts bool
	ReconnectCalendar bool
	Signatures        []Signature
	SignatureSettings AccountSignatureSettings
	ContactSync       ContactSyncConfig
	CalendarSync      CalendarSyncConfig
}

type CreateAccountRequest struct {
	Provider          string `json:"provider"`
	ProviderAccountID string `json:"provider_account_id"`
	EmailAddress      string `json:"email_address"`
	DisplayName       string `json:"display_name"`
	IMAPHost          string `json:"imap_host"`
	IMAPPort          int    `json:"imap_port"`
	IMAPTLSMode       string `json:"imap_tls_mode"`
	SMTPHost          string `json:"smtp_host"`
	SMTPPort          int    `json:"smtp_port"`
	SMTPTLSMode       string `json:"smtp_tls_mode"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	AuthMethod        string `json:"auth_method"`
	SmtpUsername      string `json:"smtp_username"`
	SmtpPassword      string `json:"smtp_password"`
	JMAPSessionURL    string `json:"jmap_session_url"`
	// EmailSyncDisabled creates the account with mail sync off, for accounts
	// imported without their credentials.
	EmailSyncDisabled bool `json:"-"`
}

type SyncSettings struct {
	SyncIntervalMinutes int
	Accounts            []AccountSyncStatus
}

type AccountSyncStatus struct {
	AccountID    string
	AccountName  string
	AccountEmail string
	Provider     string
	Color        string
	Initials     string
	Folders      []FolderSyncStatus
	// OutlookPush is set for Outlook accounts.
	OutlookPush *OutlookPushSettings
}

// OutlookPushSettings is an Outlook account's choice between Microsoft's
// change notifications and scheduled checks, with the subscription's state.
type OutlookPushSettings struct {
	AccountID string
	// Enabled is the account's choice; it only takes effect while Available.
	Enabled bool
	// Available is false while GOFER_OUTLOOK_WEBHOOKS is off on the server.
	Available bool
	// Managed hides server settings from people who can't change them.
	Managed    bool
	Subscribed bool
	Expires    time.Time
	// Error is the last failure to subscribe and RetryAt when Gofer tries
	// again on its own.
	Error   string
	RetryAt time.Time
	// Checked is set after Check now, with CheckError empty when Microsoft
	// accepted the new subscription.
	Checked    bool
	CheckError string
}

type FolderSyncStatus struct {
	ID                 string
	Name               string
	RemoteID           string
	Icon               string
	Role               string
	LastSyncedAt       string
	MessageCount       int
	IsIDLE             bool
	EffectiveIDLE      bool
	IDLEFallbackReason string
	IDLERetryAt        string
}

type Signature struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HTMLBody  string `json:"html_body"`
	TextBody  string `json:"text_body"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Identity is a further From address an account sends as. The account's own
// address is not an Identity. Provider identities come from the provider and
// only their signature is set in Gofer; manual ones are wholly the user's.
type Identity struct {
	ID                 string `json:"id"`
	AccountID          string `json:"account_id"`
	Name               string `json:"name"`
	Email              string `json:"email"`
	ReplyTo            string `json:"reply_to,omitempty"`
	SignatureID        string `json:"signature_id,omitempty"`
	Source             string `json:"source"`
	ProviderIdentityID string `json:"provider_identity_id,omitempty"`
	SortOrder          int    `json:"sort_order"`
	// SMTPHost is set for an identity that sends through its own SMTP server
	// rather than the account's way of sending.
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty"`
	SMTPTLSMode  string `json:"smtp_tls_mode,omitempty"`
	SMTPUsername string `json:"smtp_username,omitempty"`
	// HasSMTPPassword says a password for that server is stored; the
	// password itself never leaves the account store.
	HasSMTPPassword bool `json:"has_smtp_password,omitempty"`
}

// HasOwnSMTP reports whether the identity sends through its own server.
func (i Identity) HasOwnSMTP() bool { return i.SMTPHost != "" }

const (
	IdentitySourceManual   = "manual"
	IdentitySourceProvider = "provider"
)

type AccountSignatureSettings struct {
	AccountID          string `json:"account_id"`
	NewSignatureID     string `json:"new_signature_id"`
	ReplySignatureID   string `json:"reply_signature_id"`
	ForwardSignatureID string `json:"forward_signature_id"`
	NewEnabled         bool   `json:"new_enabled"`
	ReplyEnabled       bool   `json:"reply_enabled"`
	ForwardEnabled     bool   `json:"forward_enabled"`
	ReplyPlacement     string `json:"reply_placement"`
	ForwardPlacement   string `json:"forward_placement"`
}

// AccountIdentitiesData is what the identities dialog of one account shows:
// its identities, the signatures they may use, and the identity in the form,
// which is empty when adding one. Error says why the last change failed.
type AccountIdentitiesData struct {
	Account    Account
	Identities []Identity
	Signatures []Signature
	Editing    Identity
	Error      string
	// ProviderListsIdentities is set when the account's provider can be
	// asked for the addresses it may send from.
	ProviderListsIdentities bool
	// RefreshError says why asking the provider failed.
	RefreshError string
	// CatchAllDomains are the domains the provider lets the account send
	// from at any address.
	CatchAllDomains []string
	// CanAddIdentities is set when the provider sends as addresses added by
	// hand; Unusable names the manual identities it would not send as.
	CanAddIdentities bool
	Unusable         map[string]bool
}

type AccountSignatureData struct {
	Account    Account
	Signatures []Signature
	Settings   AccountSignatureSettings
}
