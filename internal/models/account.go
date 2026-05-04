package models

type AccountConfig struct {
	AccountID    string
	IMAPHost     string
	IMAPPort     int
	IMAPTLSMode  string
	SMTPHost     string
	SMTPPort     int
	SMTPTLSMode  string
	Username     string
	AuthMethod   string
	SmtpUsername string
}

type ConnectionTestResult struct {
	Success    bool   `json:"success"`
	Service    string `json:"service"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
}

type EditAccountData struct {
	AccountID    string
	EmailAddress string
	DisplayName  string
	IMAPHost     string
	IMAPPort     int
	IMAPTLSMode  string
	SMTPHost     string
	SMTPPort     int
	SMTPTLSMode  string
	Username     string
	AuthMethod   string
	SmtpUsername string
	SameSmtpAuth bool
}

type CreateAccountRequest struct {
	EmailAddress string `json:"email_address"`
	DisplayName  string `json:"display_name"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPTLSMode  string `json:"imap_tls_mode"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPTLSMode  string `json:"smtp_tls_mode"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	AuthMethod   string `json:"auth_method"`
	SmtpUsername string `json:"smtp_username"`
	SmtpPassword string `json:"smtp_password"`
}
