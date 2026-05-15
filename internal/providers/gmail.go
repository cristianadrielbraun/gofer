package providers

import "github.com/cristianadrielbraun/gofer/internal/models"

const (
	ProviderIMAP  = "imap"
	ProviderGmail = "gmail"
	OAuthGoogle   = "google"
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
