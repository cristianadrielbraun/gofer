package providers

import "github.com/cristianadrielbraun/gofer/internal/models"

const (
	ProviderIMAP    = "imap"
	ProviderGmail   = "gmail"
	ProviderOutlook = "outlook"
	ProviderCardDAV = "carddav"
	OAuthGoogle     = "google"
	OAuthMicrosoft  = "microsoft"
)

func GmailAccountRequest(email, displayName, providerAccountID string) *models.CreateAccountRequest {
	return &models.CreateAccountRequest{
		Provider:          ProviderGmail,
		ProviderAccountID: providerAccountID,
		EmailAddress:      email,
		DisplayName:       displayName,
		IMAPHost:          "imap.gmail.com",
		IMAPPort:          993,
		IMAPTLSMode:       "tls",
		SMTPHost:          "smtp.gmail.com",
		SMTPPort:          465,
		SMTPTLSMode:       "tls",
		Username:          email,
		AuthMethod:        "oauth2",
	}
}

func OutlookAccountRequest(email, displayName, providerAccountID string) *models.CreateAccountRequest {
	return &models.CreateAccountRequest{
		Provider:          ProviderOutlook,
		ProviderAccountID: providerAccountID,
		EmailAddress:      email,
		DisplayName:       displayName,
		IMAPHost:          "outlook.office365.com",
		IMAPPort:          993,
		IMAPTLSMode:       "tls",
		SMTPHost:          "smtp-mail.outlook.com",
		SMTPPort:          587,
		SMTPTLSMode:       "starttls",
		Username:          email,
		AuthMethod:        "oauth2",
	}
}
