package config

import (
	"context"
	"fmt"

	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// IdentitySMTPConfig is the configuration for sending through an identity's
// own SMTP server, with the instance's security exceptions applied. It logs
// in as the identity's SMTP username, or its address when that is empty.
func (s *AccountStore) IdentitySMTPConfig(ctx context.Context, identity models.Identity) (*models.AccountConfig, error) {
	if !identity.HasOwnSMTP() {
		return nil, fmt.Errorf("identity %s has no SMTP server of its own", identity.ID)
	}
	cfg := &models.AccountConfig{
		AccountID:    identity.AccountID,
		Provider:     "imap",
		SMTPHost:     identity.SMTPHost,
		SMTPPort:     identity.SMTPPort,
		Username:     identity.Email,
		SmtpUsername: identity.SMTPUsername,
		AuthMethod:   "plain",
	}
	var err error
	cfg.SMTPAllowPlaintext, err = s.policyDB.IsPlaintextTransportAllowed(ctx, "smtp", cfg.SMTPHost, cfg.SMTPPort)
	if err != nil {
		return nil, fmt.Errorf("check SMTP security exception: %w", err)
	}
	if cfg.SMTPTLSMode, err = mailtransport.RequireTLSModeWithPlaintext("SMTP", identity.SMTPTLSMode, cfg.SMTPAllowPlaintext); err != nil {
		return nil, err
	}
	if cfg.SMTPAllowPrivate, err = s.policyDB.IsPrivateTargetAllowed(ctx, "smtp", cfg.SMTPHost, cfg.SMTPPort); err != nil {
		return nil, fmt.Errorf("check SMTP private target exception: %w", err)
	}
	return cfg, nil
}
