package config

import (
	"context"
	"errors"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// FindOAuthAccount uses the exact provider identity. An email match alone must
// never authorize replacing a different subject or a password-authenticated
// mailbox. Callers serialize setup through the central credential service.
func (s *UserAccountStore) FindOAuthAccount(ctx context.Context, owner, provider, subject, email string) (string, error) {
	var id string
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		rows, err := db.Read().QueryContext(ctx, `SELECT id,provider,provider_account_id,auth_method,email_address FROM accounts WHERE user_id=? AND COALESCE(is_deleting,0)=0 AND ((provider=? AND provider_account_id=?) OR email_address=? COLLATE NOCASE)`, owner, provider, subject, email)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate, p, sub, method, address string
			if err := rows.Scan(&candidate, &p, &sub, &method, &address); err != nil {
				return err
			}
			if p != provider || sub != subject || method != "oauth2" || !strings.EqualFold(strings.TrimSpace(address), strings.TrimSpace(email)) || (id != "" && id != candidate) {
				return ErrMailboxExists
			}
			id = candidate
		}
		return rows.Err()
	})
	return id, err
}

// UpdateOAuthMetadata preserves transports, service preferences and secrets.
// Identity checks belong in the write itself so a concurrent edit cannot make
// an earlier snapshot authorize a different mailbox.
func (s *UserAccountStore) UpdateOAuthMetadata(ctx context.Context, owner, id string, req *models.CreateAccountRequest) error {
	if req == nil || req.AuthMethod != "oauth2" || strings.TrimSpace(req.ProviderAccountID) == "" {
		return errors.New("mailbox OAuth identity is required")
	}
	return s.WithAccountForUser(ctx, owner, id, func(_ *AccountStore, db *storage.DB) error {
		result, err := db.Write().ExecContext(ctx, `UPDATE accounts SET display_name=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND provider=? AND provider_account_id=? AND auth_method='oauth2' AND email_address=? COLLATE NOCASE AND COALESCE(is_deleting,0)=0`, req.DisplayName, id, owner, req.Provider, req.ProviderAccountID, req.EmailAddress)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return storage.ErrAccountRoute
		}
		return nil
	})
}
