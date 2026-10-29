package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errComposeIdentityNotFound = errors.New("identity not found")

// composeSender is who a message is sent as: an account's own address or one
// of its identities.
type composeSender struct {
	Name       string
	Email      string
	ReplyTo    []*mail.Address
	IdentityID string
	// OwnSMTP is set for an identity that sends through its own server.
	OwnSMTP bool
}

func accountComposeSender(account *models.Account) composeSender {
	return composeSender{Name: account.Name, Email: account.Email}
}

func identityComposeSender(identity models.Identity) composeSender {
	sender := composeSender{Name: identity.Name, Email: identity.Email, IdentityID: identity.ID, OwnSMTP: identity.HasOwnSMTP()}
	if identity.ReplyTo != "" {
		// Stored lists were checked when saved; a bad one sends without Reply-To.
		sender.ReplyTo, _ = message.ParseAddressList(identity.ReplyTo)
	}
	return sender
}

func (s composeSender) apply(msg *message.OutgoingMessage) {
	msg.FromName, msg.FromEmail, msg.ReplyTo = s.Name, s.Email, s.ReplyTo
	msg.IdentityID, msg.IdentitySMTP = s.IdentityID, s.OwnSMTP
}

// errComposeFromNotAllowed is a one-time From the chosen way of sending is
// known to refuse or rewrite.
var errComposeFromNotAllowed = errors.New("This account can't send from that address. Choose another way to send it, or another address.")

// errComposeSendViaUnavailable is a one-time From sent through an identity's
// server when that identity is gone or no longer has its own server.
var errComposeSendViaUnavailable = errors.New("That identity no longer sends through its own server. Choose another way to send it.")

// composeSenderError is the status and message a compose request refused for
// err, an error of composeSenderFor, gets.
func composeSenderError(err error) (int, string) {
	var invalid composeFromAddressError
	switch {
	case errors.Is(err, errComposeIdentityNotFound):
		return http.StatusNotFound, "identity not found"
	case errors.As(err, &invalid), errors.Is(err, errComposeFromNotAllowed), errors.Is(err, errComposeSendViaUnavailable):
		return http.StatusBadRequest, err.Error()
	}
	return http.StatusInternalServerError, "failed to load identity"
}

// composeFromAddressError is a one-time From that is not an email address.
type composeFromAddressError struct{ email string }

func (e composeFromAddressError) Error() string {
	return fmt.Sprintf("%q is not an email address.", e.email)
}

// composeSenderFor returns who a compose form sends as: a one-time From
// typed into it (from_name, from_email), the identity named by identity_id,
// which must be one of the account's, or else the account. Only a message
// being sent has its one-time From checked against the way it is sent: a
// draft may keep an address that can't be sent yet.
func (h *Handler) composeSenderFor(ctx context.Context, account *models.Account, r *http.Request, sending bool) (composeSender, error) {
	if strings.TrimSpace(r.FormValue("from_email")) != "" {
		return h.oneTimeComposeSender(ctx, account, r.FormValue("from_name"), r.FormValue("from_email"), r.FormValue("send_via"), sending)
	}
	identityID := strings.TrimSpace(r.FormValue("identity_id"))
	if identityID == "" {
		return accountComposeSender(account), nil
	}
	identity, err := h.db.GetAccountIdentity(ctx, h.userID(ctx), account.ID, identityID)
	if errors.Is(err, sql.ErrNoRows) {
		return composeSender{}, errComposeIdentityNotFound
	}
	if err != nil {
		return composeSender{}, err
	}
	return identityComposeSender(identity), nil
}

// oneTimeComposeSender is a From typed for one message, sent the account's
// way, or through the own server of the identity named by sendVia. It has no
// Reply-To and takes nothing else from that identity. When sending, the way
// of sending must not be one known to refuse the address.
func (h *Handler) oneTimeComposeSender(ctx context.Context, account *models.Account, name, typed, sendVia string, sending bool) (composeSender, error) {
	email, _, err := storage.NormalizeIdentityAddresses(typed, "")
	if err != nil {
		return composeSender{}, composeFromAddressError{email: strings.TrimSpace(typed)}
	}
	sender := composeSender{Name: strings.TrimSpace(name), Email: email}
	if sendVia = strings.TrimSpace(sendVia); sendVia != "" {
		identity, err := h.db.GetAccountIdentity(ctx, h.userID(ctx), account.ID, sendVia)
		if errors.Is(err, sql.ErrNoRows) {
			return composeSender{}, errComposeSendViaUnavailable
		}
		if err != nil {
			return composeSender{}, err
		}
		if !identity.HasOwnSMTP() {
			return composeSender{}, errComposeSendViaUnavailable
		}
		sender.IdentityID, sender.OwnSMTP = identity.ID, true
		return sender, nil
	}
	if !sending {
		return sender, nil
	}
	rule, err := h.accountFromRule(ctx, account)
	if err != nil {
		return composeSender{}, err
	}
	if !rule.Accepts(email) {
		listed, err := h.providerListsAddress(ctx, account, email)
		if err != nil {
			return composeSender{}, err
		}
		if !listed {
			return composeSender{}, errComposeFromNotAllowed
		}
	}
	return sender, nil
}

// providerListsAddress reports whether email is the account's own address or
// one of the identities its provider reported, which the account's way of
// sending takes under any name.
func (h *Handler) providerListsAddress(ctx context.Context, account *models.Account, email string) (bool, error) {
	if strings.EqualFold(account.Email, email) {
		return true, nil
	}
	identities, err := h.db.ListAccountIdentities(ctx, h.userID(ctx), account.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(identities, func(identity models.Identity) bool {
		return identity.Source == models.IdentitySourceProvider && strings.EqualFold(identity.Email, email)
	}), nil
}

// accountFromRule is the rule for the From addresses the account's own way of
// sending takes.
func (h *Handler) accountFromRule(ctx context.Context, account *models.Account) (providers.FromRule, error) {
	cfg, err := h.accountStore.GetConfig(ctx, account.ID)
	if err != nil {
		return providers.FromRule{}, err
	}
	domains, err := h.db.CatchAllIdentityDomains(ctx, h.userID(ctx), account.ID)
	if err != nil {
		return providers.FromRule{}, err
	}
	return providers.AccountFromRule(account.Provider, cfg.SMTPHost, domains), nil
}

// matchSenderIdentity finds the identity a saved From address stands for,
// preferring one with the same name too. An address that is the account's
// own matches no identity unless an identity has the same name as well.
func matchSenderIdentity(account *models.Account, identities []models.Identity, name, email string) (models.Identity, bool) {
	email, name = strings.TrimSpace(email), strings.TrimSpace(name)
	if email == "" {
		return models.Identity{}, false
	}
	for _, identity := range identities {
		if strings.EqualFold(identity.Email, email) && identity.Name == name {
			return identity, true
		}
	}
	if account != nil && strings.EqualFold(account.Email, email) {
		return models.Identity{}, false
	}
	for _, identity := range identities {
		if strings.EqualFold(identity.Email, email) {
			return identity, true
		}
	}
	return models.Identity{}, false
}

// draftSender is who a saved draft whose From is name and email goes out as:
// the identity it names, the account for its own address, or else a one-time
// From, reported by oneTime. A draft keeps no way of sending, so a one-time
// From goes through the own server of the first identity at the same domain
// that has one, or else the account's way.
func draftSender(account *models.Account, identities []models.Identity, name, email string) (sender composeSender, oneTime bool) {
	if identity, ok := matchSenderIdentity(account, identities, name, email); ok {
		return identityComposeSender(identity), false
	}
	email = strings.TrimSpace(email)
	if email == "" || strings.EqualFold(email, account.Email) {
		return accountComposeSender(account), false
	}
	sender = composeSender{Name: strings.TrimSpace(name), Email: email}
	domain := email[strings.LastIndex(email, "@")+1:]
	for _, identity := range identities {
		if identity.HasOwnSMTP() && strings.EqualFold(identity.Email[strings.LastIndex(identity.Email, "@")+1:], domain) {
			sender.IdentityID, sender.OwnSMTP = identity.ID, true
			break
		}
	}
	return sender, true
}

// replyIdentity finds the identity a reply to email goes out as. A message
// sent from one of the account's identities is answered as that identity;
// otherwise the first of the account's addresses in To, then Cc, decides, and
// the account's own address there means replying as the account.
func replyIdentity(account *models.Account, identities []models.Identity, email *models.Email) (models.Identity, bool) {
	if account == nil || email == nil || len(identities) == 0 {
		return models.Identity{}, false
	}
	if identity, ok := matchSenderIdentity(account, identities, email.From.Name, email.From.Email); ok {
		return identity, true
	}
	if strings.EqualFold(strings.TrimSpace(email.From.Email), account.Email) {
		return models.Identity{}, false
	}
	for _, recipients := range [][]models.Contact{email.To, email.CC} {
		for _, recipient := range recipients {
			address := strings.TrimSpace(recipient.Email)
			if address == "" {
				continue
			}
			if strings.EqualFold(address, account.Email) {
				return models.Identity{}, false
			}
			for _, identity := range identities {
				if strings.EqualFold(identity.Email, address) {
					return identity, true
				}
			}
		}
	}
	return models.Identity{}, false
}

// ownAddresses lists the account's address and its identities' addresses,
// lowercased, for leaving them out of a reply-all.
func ownAddresses(account *models.Account, identities []models.Identity) []string {
	seen := map[string]bool{}
	addresses := []string{}
	add := func(address string) {
		address = strings.ToLower(strings.TrimSpace(address))
		if address != "" && !seen[address] {
			seen[address] = true
			addresses = append(addresses, address)
		}
	}
	if account != nil {
		add(account.Email)
	}
	for _, identity := range identities {
		add(identity.Email)
	}
	return addresses
}

// errIdentitySMTPUnavailable is a message queued to go through an identity's
// own server when that server can't be used: it is never sent the account's
// way instead, where the From would likely be refused or rewritten.
var errIdentitySMTPUnavailable = errors.New("This identity's SMTP server is no longer set up, so the message wasn't sent.")

var errIdentitySMTPNoPassword = errors.New("This identity's SMTP server has no password stored, so the message wasn't sent. Enter it in the account's Identities and send again.")

// identitySMTPRoute is the server and password a message sent as identityID
// goes out through.
func identitySMTPRoute(ctx context.Context, accounts *config.AccountStore, identities []models.Identity, identityID string) (*models.AccountConfig, string, error) {
	index := slices.IndexFunc(identities, func(identity models.Identity) bool { return identity.ID == identityID })
	if index < 0 || !identities[index].HasOwnSMTP() {
		return nil, "", errIdentitySMTPUnavailable
	}
	identity := identities[index]
	password, err := accounts.DecryptIdentitySMTPPassword(ctx, identity.AccountID, identity.ID)
	if err != nil {
		return nil, "", err
	}
	if password == "" {
		return nil, "", errIdentitySMTPNoPassword
	}
	cfg, err := accounts.IdentitySMTPConfig(ctx, identity)
	if err != nil {
		return nil, "", err
	}
	return cfg, password, nil
}
