package auth

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// ExportAdministratorUsers returns everyone query keeps, in its order, for
// an administrator who may export the people list. The export is recorded,
// with how many people it holds and the list's search and filters, before
// anyone is returned. Paging and exact IDs are ignored: an export is the
// whole list.
func (m *Manager) ExportAdministratorUsers(ctx context.Context, actorUserID, actorSessionID string, query AdministratorUserQuery) ([]AdministratorUserSummary, error) {
	actorUserID, actorSessionID = strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID)
	query.UserIDs, query.Offset, query.Limit = nil, 0, -1
	page, err := m.listAdministratorUsers(ctx, actorUserID, "", query)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{"format": "csv", "count": len(page.Users)}
	for key, value := range map[string]string{
		"search": strings.TrimSpace(query.Search), "status": string(query.Status),
		"attention": string(query.Attention), "sort": string(query.Sort),
	} {
		if value != "" {
			metadata[key] = value
		}
	}
	now := m.clock.Now().UTC()
	err = m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.UsersExport, now); err != nil {
			return err
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:   "user.list_exported",
			Target:   audit.Target{Kind: auditTargetInstance},
			Metadata: metadata,
		})
	})
	if err != nil {
		return nil, err
	}
	return page.Users, nil
}
