package mail

import (
	"context"
	"errors"
	"fmt"
	"log"
	netmail "net/mail"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// providerIdentityInterval is how often a sync asks the provider again for
// the addresses an account may send from.
const providerIdentityInterval = time.Hour

// ErrProviderIdentitiesUnsupported is an account whose provider keeps no list
// of addresses it may send from.
var ErrProviderIdentitiesUnsupported = errors.New("this provider does not list the addresses it can send from")

// providerIdentityChecks holds when each account last asked its provider.
var providerIdentityChecks sync.Map

// ProviderListsIdentities reports whether accounts of provider can bring in
// the addresses they may send from.
func ProviderListsIdentities(provider string) bool {
	return provider == providers.ProviderJMAP || provider == providers.ProviderGmail
}

// syncProviderIdentities refreshes an account's provider identities as part
// of a sync, at most once per providerIdentityInterval. Failing to does not
// fail the sync.
func (o *SyncOrchestrator) syncProviderIdentities(ctx context.Context, accountID string, cfg *models.AccountConfig) {
	if cfg == nil || !ProviderListsIdentities(cfg.Provider) || (cfg.Provider == providers.ProviderJMAP && !providers.JMAPEnabled()) {
		return
	}
	if last, ok := providerIdentityChecks.Load(accountID); ok && time.Since(last.(time.Time)) < providerIdentityInterval {
		return
	}
	if err := o.refreshProviderIdentities(ctx, accountID, cfg); err != nil && ctx.Err() == nil {
		log.Printf("provider identities %s: %v", accountID, err)
	}
}

// refreshProviderIdentities asks the provider for the addresses the account
// may send from and stores them as its provider identities.
func (o *SyncOrchestrator) refreshProviderIdentities(ctx context.Context, accountID string, cfg *models.AccountConfig) error {
	if cfg == nil || !ProviderListsIdentities(cfg.Provider) {
		return ErrProviderIdentitiesUnsupported
	}
	var reported []models.Identity
	var err error
	switch cfg.Provider {
	case providers.ProviderJMAP:
		if !providers.JMAPEnabled() {
			return providers.ErrJMAPDisabled
		}
		var password string
		if password, err = o.resolvePassword(ctx, cfg, accountID); err == nil {
			reported, err = jmapProviderIdentities(ctx, cfg, password)
		}
	case providers.ProviderGmail:
		reported, err = o.gmailProviderIdentities(ctx, accountID)
	}
	// Asked, whatever came of it: a refusal is not retried on every sync.
	providerIdentityChecks.Store(accountID, time.Now())
	if err != nil {
		return err
	}
	return o.withSyncStore(ctx, func(db *storage.DB) error {
		return db.ReplaceProviderIdentities(ctx, accountID, reported)
	})
}

// RefreshProviderIdentities asks an account's provider again for the
// addresses it may send from.
func (o *SyncOrchestrator) RefreshProviderIdentities(ctx context.Context, accountID string) error {
	cfg, err := o.imapAccountConfig(ctx, accountID)
	if err != nil {
		return err
	}
	return o.refreshProviderIdentities(ctx, accountID, cfg)
}

// RefreshProviderIdentities asks the provider of one of owner's accounts
// again for the addresses it may send from.
func (s *UserIMAP) RefreshProviderIdentities(ctx context.Context, owner, id string) error {
	return s.operation(ctx, owner, id, 0, time.Minute, func(ctx context.Context) error {
		scope, err := s.snapshotAccount(ctx, owner, id, true)
		if err != nil {
			return err
		}
		o := NewSyncOrchestrator(nil, nil, s.blobs, scope.tokens)
		o.imapScope = scope
		return o.refreshProviderIdentities(ctx, id, scope.config)
	})
}

func jmapProviderIdentities(ctx context.Context, cfg *models.AccountConfig, password string) ([]models.Identity, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, fmt.Errorf("JMAP session: %w", err)
	}
	account := session.PrimaryAccount(jmap.CapabilityMail)
	if account == "" || !jmapCanSend(session) {
		// Nothing to send from: no identities.
		return nil, nil
	}
	result, err := client.GetIdentities(ctx, session, account)
	if err != nil {
		return nil, err
	}
	identities := make([]models.Identity, 0, len(result.List))
	for _, identity := range result.List {
		replyTo := make([]string, 0, len(identity.ReplyTo))
		for _, address := range identity.ReplyTo {
			if strings.TrimSpace(address.Email) != "" {
				replyTo = append(replyTo, (&netmail.Address{Name: address.Name, Address: address.Email}).String())
			}
		}
		identities = append(identities, models.Identity{
			ProviderIdentityID: identity.ID,
			Name:               identity.Name,
			Email:              identity.Email,
			ReplyTo:            strings.Join(replyTo, ", "),
		})
	}
	return identities, nil
}

// gmailSendAs is one of a Gmail account's "send mail as" addresses.
type gmailSendAs struct {
	SendAsEmail        string `json:"sendAsEmail"`
	DisplayName        string `json:"displayName"`
	ReplyToAddress     string `json:"replyToAddress"`
	IsPrimary          bool   `json:"isPrimary"`
	VerificationStatus string `json:"verificationStatus"`
}

func (o *SyncOrchestrator) gmailProviderIdentities(ctx context.Context, accountID string) ([]models.Identity, error) {
	if o.tokenProvider == nil {
		return nil, errors.New("google sign-in is not configured")
	}
	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var response struct {
		SendAs []gmailSendAs `json:"sendAs"`
	}
	endpoint := gmailAPIBaseURL + "/users/me/settings/sendAs"
	err = providerJSON(ctx, "GET", endpoint, token, nil, nil, &response)
	if refreshed, ok := o.refreshGmailTokenAfterUnauthorized(ctx, accountID, &token, err); ok {
		err = providerJSON(ctx, "GET", endpoint, refreshed, nil, nil, &response)
	}
	if err != nil {
		return nil, err
	}
	identities := make([]models.Identity, 0, len(response.SendAs))
	for _, sendAs := range response.SendAs {
		// The primary address is the account's own; an alias Gmail has not
		// verified would go out as the account.
		if strings.TrimSpace(sendAs.SendAsEmail) == "" || sendAs.IsPrimary || (sendAs.VerificationStatus != "" && sendAs.VerificationStatus != "accepted") {
			continue
		}
		identities = append(identities, models.Identity{
			ProviderIdentityID: strings.ToLower(strings.TrimSpace(sendAs.SendAsEmail)),
			Name:               sendAs.DisplayName,
			Email:              sendAs.SendAsEmail,
			ReplyTo:            sendAs.ReplyToAddress,
		})
	}
	return identities, nil
}
