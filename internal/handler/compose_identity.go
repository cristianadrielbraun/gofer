package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

var errComposeIdentityNotFound = errors.New("identity not found")

// composeSender is who a message is sent as: an account's own address or one
// of its identities.
type composeSender struct {
	Name       string
	Email      string
	ReplyTo    []*mail.Address
	IdentityID string
}

func accountComposeSender(account *models.Account) composeSender {
	return composeSender{Name: account.Name, Email: account.Email}
}

func identityComposeSender(identity models.Identity) composeSender {
	sender := composeSender{Name: identity.Name, Email: identity.Email, IdentityID: identity.ID}
	if identity.ReplyTo != "" {
		// Stored lists were checked when saved; a bad one sends without Reply-To.
		sender.ReplyTo, _ = message.ParseAddressList(identity.ReplyTo)
	}
	return sender
}

func (s composeSender) apply(msg *message.OutgoingMessage) {
	msg.FromName, msg.FromEmail, msg.ReplyTo = s.Name, s.Email, s.ReplyTo
}

// composeSenderFor returns who a compose form sends as: the account, or the
// identity named by identityID, which must be one of the account's.
func (h *Handler) composeSenderFor(ctx context.Context, account *models.Account, identityID string) (composeSender, error) {
	identityID = strings.TrimSpace(identityID)
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
