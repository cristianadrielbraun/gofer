package storage

import (
	"context"
	"errors"
	"strings"
)

// ActiveAccountOwners validates a bounded group of watcher identities in one
// central query. It never opens a user store or returns mailbox credentials.
func (r *AccountRouting) ActiveAccountOwners(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) > 4096 {
		return nil, errors.New("too many account identities")
	}
	result := make(map[string]string)
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := r.System().Read().QueryContext(ctx, `SELECT d.account_id,d.user_id FROM gofer_account_directory d JOIN users u ON u.id=d.user_id WHERE d.account_id IN (`+marks+`) AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		result[id] = owner
	}
	return result, rows.Err()
}
