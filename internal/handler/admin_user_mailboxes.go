package handler

import (
	"context"
	"errors"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminUserMailboxes reads the mail accounts person added, from their own
// store only, read-only. has is false for someone with no mail of their own
// or whose data is being deleted.
func (h *Handler) adminUserMailboxes(ctx context.Context, actor *auth.User, person auth.AdministratorUserSummary) (mailboxes []views.AdminUserMailboxData, has bool, err error) {
	if person.UserType != auth.UserTypeWebmail || person.IsAdmin || person.DeletionPending {
		return nil, false, nil
	}
	var stored []storage.AdminMailbox
	if h.ownedMailbox == nil {
		if stored, err = h.db.ListAdminMailboxes(ctx, person.ID); err != nil {
			return nil, false, err
		}
	} else {
		readCtx, cancel := h.ownedAdminDiagnosticContext(ctx)
		defer cancel()
		data, err := h.ownedMailbox.userStorage.ReadUserDiagnostics(readCtx, storage.DiagnosticsActor{ID: actor.ID, AuthVersion: actor.AuthVersion}, person.ID, storage.UserDiagnosticsMailboxes)
		if err != nil {
			return nil, false, err
		}
		stored = data.Mailboxes
	}
	allowlist, err := h.db.MailAllowlistForUser(ctx, person.ID)
	if err != nil {
		return nil, false, err
	}
	proxies := adminUserProxyNames{h: h, ctx: ctx, owner: person.ID}
	mailboxes = make([]views.AdminUserMailboxData, 0, len(stored))
	for _, mailbox := range stored {
		view := views.AdminUserMailboxData{
			Provider: mailbox.Provider, Email: mailbox.Email, DisplayName: mailbox.DisplayName,
			Servers:     adminUserMailboxServers(ctx, mailbox.DiagnosticTransportAccount),
			SyncEnabled: mailbox.SyncEnabled, SyncError: mailbox.SyncError, SyncErrorAt: mailbox.SyncErrorAt,
			Hidden: mailbox.Hidden, CreatedAt: mailbox.CreatedAt, Unlimited: !allowlist.Limited(),
		}
		if view.Proxy, err = proxies.label(mailbox.ProxyID); err != nil {
			return nil, false, err
		}
		if !view.Unlimited {
			var refusal *storage.MailAllowlistError
			if err := allowlist.CheckMailboxTransports(mailbox.DiagnosticTransportAccount); errors.As(err, &refusal) {
				view.Refusal = adminUserAllowlistRefusal(ctx, refusal)
			} else if err != nil {
				return nil, false, err
			}
		}
		mailboxes = append(mailboxes, view)
	}
	return mailboxes, true, nil
}

// adminUserMailboxServers says where a mailbox connects, one line a
// service. Gmail and Outlook reach their provider only.
func adminUserMailboxServers(ctx context.Context, account storage.DiagnosticTransportAccount) []views.AdminUserMailboxServer {
	var servers []views.AdminUserMailboxServer
	switch account.Provider {
	case providers.ProviderGmail:
		servers = append(servers, views.AdminUserMailboxServer{Service: i18n.T(ctx, "admin.user.mailbox_service"), Address: "Google"})
	case providers.ProviderOutlook:
		servers = append(servers, views.AdminUserMailboxServer{Service: i18n.T(ctx, "admin.user.mailbox_service"), Address: "Microsoft"})
	case providers.ProviderJMAP:
		servers = append(servers, views.AdminUserMailboxServer{Service: "JMAP", Address: account.JMAPSessionURL})
	default:
		if account.IMAPHost != "" {
			servers = append(servers, views.AdminUserMailboxServer{Service: "IMAP", Address: adminUserMailServer(ctx, account.IMAPHost, account.IMAPPort, account.IMAPTLSMode)})
		}
		if account.SMTPHost != "" {
			servers = append(servers, views.AdminUserMailboxServer{Service: "SMTP", Address: adminUserMailServer(ctx, account.SMTPHost, account.SMTPPort, account.SMTPTLSMode)})
		}
	}
	if account.CalDAVURL != "" {
		servers = append(servers, views.AdminUserMailboxServer{Service: "CalDAV", Address: account.CalDAVURL})
	}
	if account.CardDAVURL != "" {
		servers = append(servers, views.AdminUserMailboxServer{Service: "CardDAV", Address: account.CardDAVURL})
	}
	return servers
}

func adminUserMailServer(ctx context.Context, host string, port int, mode string) string {
	address := host
	if port > 0 {
		address += ":" + strconv.Itoa(port)
	}
	switch mode {
	case mailtransport.TLSModeImplicit:
		return address + " · TLS"
	case mailtransport.TLSModeStartTLS:
		return address + " · STARTTLS"
	case mailtransport.TLSModePlaintext:
		return address + " · " + i18n.T(ctx, "admin.user.mailbox_unencrypted")
	}
	return address
}

func adminUserAllowlistRefusal(ctx context.Context, refusal *storage.MailAllowlistError) string {
	switch refusal.Rule {
	case storage.MailAllowlistDomain:
		return i18n.T(ctx, "admin.user.refused_domain", "domain", refusal.Value)
	case storage.MailAllowlistProvider:
		return i18n.T(ctx, "admin.user.refused_provider", "provider", refusal.Value)
	}
	return i18n.T(ctx, "admin.user.refused_server", "server", refusal.Value)
}

// adminUserProxyNames names a person's proxy choices, reading each proxy
// once.
type adminUserProxyNames struct {
	h     *Handler
	ctx   context.Context
	owner string
	cache map[string]string
}

func (p *adminUserProxyNames) label(choice string) (string, error) {
	if p.cache == nil {
		p.cache = map[string]string{}
	}
	if label, ok := p.cache[choice]; ok {
		return label, nil
	}
	label, err := p.read(choice)
	if err == nil {
		p.cache[choice] = label
	}
	return label, err
}

func (p *adminUserProxyNames) read(choice string) (string, error) {
	ctx := p.ctx
	switch choice {
	case "direct":
		return i18n.T(ctx, "admin.user.proxy_none"), nil
	case "":
		proxy, ok, err := p.h.db.DefaultOutboundProxy(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return i18n.T(ctx, "admin.user.proxy_none"), nil
		}
		return i18n.T(ctx, "admin.user.proxy_default", "name", proxy.Name), nil
	}
	proxy, err := p.h.db.GetOutboundProxy(ctx, choice)
	if err != nil {
		if errors.Is(err, storage.ErrOutboundProxyNotFound) {
			return i18n.T(ctx, "admin.user.proxy_gone"), nil
		}
		return "", err
	}
	if proxy.OwnerUserID == p.owner {
		return i18n.T(ctx, "admin.user.proxy_own", "name", proxy.Name), nil
	}
	return proxy.Name, nil
}
