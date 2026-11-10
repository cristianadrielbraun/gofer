package handler

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// What the audit log names the targets of the admin console's own changes.
const (
	auditTargetMailSecurityException = "mail_security_exception"
	auditTargetAllowlist             = "mail_allowlist"
	auditTargetProxy                 = "proxy"
	auditTargetUser                  = "user"
	auditTargetInstance              = "instance"
)

// withAdminAudit returns ctx with a recorder that writes event to the
// administration audit log inside the change's own transaction, so the
// change and its record stand or fall together. Storage's administrative
// writes run it when they change something. ctx names the administrator,
// and their browser (withAuditClient).
func (h *Handler) withAdminAudit(ctx context.Context, event audit.Event) context.Context {
	if h.auth == nil {
		return ctx
	}
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return ctx
	}
	sessionID := ""
	if session := auth.GetCurrentSession(ctx); session != nil {
		sessionID = session.ID
	}
	return storage.WithChangeRecorder(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return h.auth.RecordAdministratorChangeTx(ctx, tx, user.ID, sessionID, event)
	})
}

// exceptionAuditEvent describes a mail security exception for the log.
func exceptionAuditEvent(action, id, kind, protocol, host string, port int) audit.Event {
	label := host
	if port > 0 {
		label = host + ":" + strconv.Itoa(port)
	}
	metadata := map[string]any{"kind": kind, "host": host}
	if protocol != "" {
		metadata["protocol"] = protocol
	}
	if port > 0 {
		metadata["port"] = port
	}
	return audit.Event{
		Action:   action,
		Target:   audit.Target{Kind: auditTargetMailSecurityException, ID: id, Label: label},
		Metadata: metadata,
	}
}

// proxyAuditTarget names a proxy for the log by its id and, when it can be
// read, its name.
func proxyAuditTarget(ctx context.Context, h *Handler, id string) audit.Target {
	target := audit.Target{Kind: auditTargetProxy, ID: id}
	if proxy, err := h.db.GetOutboundProxy(ctx, id); err == nil {
		target.Label = proxy.Name
	}
	return target
}

// withAuditClient tells the audit log which browser the admin console's
// changes come from.
func withAuditClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(audit.WithClient(r.Context(), audit.Client{UserAgent: r.UserAgent()})))
	})
}
