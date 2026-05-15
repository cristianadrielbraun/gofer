package models

type AccountConfig struct {
	AccountID         string
	Provider          string
	ProviderAccountID string
	IMAPHost          string
	IMAPPort          int
	IMAPTLSMode       string
	SMTPHost          string
	SMTPPort          int
	SMTPTLSMode       string
	Username          string
	AuthMethod        string
	SmtpUsername      string
}

type ConnectionTestResult struct {
	Success bool   `json:"success"`
	Service string `json:"service"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
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
	SameSmtpAuth      bool
	Signatures        []Signature
	SignatureSettings AccountSignatureSettings
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
}

type SyncSettings struct {
	SyncIntervalMinutes int
	Accounts            []AccountSyncStatus
}

type AccountSyncStatus struct {
	AccountID    string
	AccountName  string
	AccountEmail string
	Color        string
	Initials     string
	Folders      []FolderSyncStatus
}

type FolderSyncStatus struct {
	Name         string
	Icon         string
	Role         string
	LastSyncedAt string
	MessageCount int
	IsIDLE       bool
}

type Signature struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HTMLBody  string `json:"html_body"`
	TextBody  string `json:"text_body"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

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

type AccountSignatureData struct {
	Account    Account
	Signatures []Signature
	Settings   AccountSignatureSettings
}
