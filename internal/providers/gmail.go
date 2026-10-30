package providers

import "github.com/cristianadrielbraun/gofer/internal/models"

const (
	ProviderIMAP    = "imap"
	ProviderGmail   = "gmail"
	ProviderOutlook = "outlook"
	ProviderJMAP    = "jmap"
	ProviderCardDAV = "carddav"
	OAuthGoogle     = "google"
	OAuthMicrosoft  = "microsoft"
	// ProviderDAV is an account with no mail, only a CalDAV or CardDAV
	// server, signed in to with the account's own login.
	ProviderDAV = "dav"
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
		AuthMethod:        "oauth2",
	}
}
