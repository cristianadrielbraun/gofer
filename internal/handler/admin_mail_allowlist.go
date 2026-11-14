package handler

import (
	"context"
	"errors"
	"sort"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The allowed mail lists are the policies' (internal/policy): the Mail
// security page of a managed installation shows the Default policy's, who
// the exemption policy is assigned to, and the mailboxes the lists refuse,
// and the Policies pages change them. People in personal and open mode
// connect only their own mailboxes, so they have no lists.

// mailAllowlistAdminData loads the lists, the people they don't apply to and
// the mailboxes they stop from connecting. It is nil when Gofer doesn't keep
// people's mail in their own stores.
func (h *Handler) mailAllowlistAdminData(ctx context.Context) (*models.MailAllowlistAdminData, error) {
	if h.ownedMailbox == nil {
		return nil, nil
	}
	lists, err := h.auth.MailLists(ctx, h.userID(ctx))
	if err != nil {
		return nil, err
	}
	data := &models.MailAllowlistAdminData{Servers: lists.Servers, Domains: lists.Domains, Gmail: lists.Gmail, Outlook: lists.Outlook}
	if lists.Exemption != nil {
		data.ExemptionPolicy, data.ExemptionPolicyID, data.ExemptionPolicyName = true, lists.Exemption.ID, lists.Exemption.Name
	}
	scope, err := h.adminWebmailScope(ctx, "")
	if err != nil {
		return nil, err
	}
	refused, checkedAt, err := h.currentMailRefusals(ctx)
	if err != nil {
		return nil, err
	}
	data.RefusalsCheckedAt = checkedAt
	if data.Limited, err = policy.MailListsLimitAnyone(ctx, h.db.Read()); err != nil {
		return nil, err
	}
	for _, user := range scope.Users {
		if _, ok := slicesIndex(lists.Exempt, user.ID); ok {
			data.Exempt = append(data.Exempt, user)
		}
	}
	for _, account := range refused {
		data.Refused = append(data.Refused, account.MailAllowlistRefusedAccount)
	}
	return data, nil
}

func slicesIndex(sorted []string, value string) (int, bool) {
	i := sort.SearchStrings(sorted, value)
	return i, i < len(sorted) && sorted[i] == value
}

type mailAllowlistRefusal struct {
	models.MailAllowlistRefusedAccount
	owner string
}

// mailAllowlistRefusalReason says, for the administrator, which rule a
// mailbox breaks.
func mailAllowlistRefusalReason(ctx context.Context, err error) string {
	var refusal *storage.MailAllowlistError
	if !errors.As(err, &refusal) {
		return err.Error()
	}
	switch refusal.Rule {
	case storage.MailAllowlistDomain:
		return i18n.T(ctx, "auth.allowlist.refused_domain", "domain", refusal.Value)
	case storage.MailAllowlistProvider:
		return i18n.T(ctx, "auth.allowlist.refused_provider", "provider", refusal.Value)
	default:
		return i18n.T(ctx, "auth.allowlist.refused_server", "server", refusal.Value)
	}
}

// markExceptionsOutsideAllowlist flags the exceptions for servers the allowed
// mail servers leave out. HTTP discovery reaches the domain itself or its
// autoconfig host.
func (h *Handler) markExceptionsOutsideAllowlist(ctx context.Context, exceptions []models.MailSecurityException) error {
	list, err := policy.DefaultMailAllowlist(ctx, h.db.Read())
	if err != nil || !list.ServersLimited() {
		return err
	}
	for i := range exceptions {
		item := &exceptions[i]
		if item.Kind == models.MailSecurityExceptionHTTPDiscovery {
			item.OutsideAllowlist = !list.AllowsServer(item.Host) && !list.AllowsServer("autoconfig."+item.Host)
		} else {
			item.OutsideAllowlist = !list.AllowsServer(item.Host)
		}
	}
	return nil
}
