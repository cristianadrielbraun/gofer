package mail

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The learned real-time folder limit of an account is kept in the database
// that holds the account. Stores are plain values, so every watch of one
// account shares one.

// dbIdleLimitStore keeps it in the shared database.
type dbIdleLimitStore struct {
	db        *storage.DB
	accountID string
}

func (s dbIdleLimitStore) LoadIdleLimit(ctx context.Context) (models.IdleConnectionLimit, bool, error) {
	return s.db.GetIdleConnectionLimit(ctx, s.accountID)
}

func (s dbIdleLimitStore) SaveIdleLimit(ctx context.Context, limit models.IdleConnectionLimit) error {
	return s.db.SaveIdleConnectionLimit(ctx, s.accountID, limit)
}

// userIdleLimitStore keeps it in its owner's storage.
type userIdleLimitStore struct {
	accounts         *config.UserAccountStore
	owner, accountID string
}

func (s userIdleLimitStore) LoadIdleLimit(ctx context.Context) (limit models.IdleConnectionLimit, found bool, err error) {
	err = s.accounts.Routing().WithAccountForUser(ctx, s.owner, s.accountID, func(db *storage.DB) error {
		limit, found, err = db.GetIdleConnectionLimit(ctx, s.accountID)
		return err
	})
	return limit, found, err
}

func (s userIdleLimitStore) SaveIdleLimit(ctx context.Context, limit models.IdleConnectionLimit) error {
	return s.accounts.Routing().WithAccountForUser(ctx, s.owner, s.accountID, func(db *storage.DB) error {
		return db.SaveIdleConnectionLimit(ctx, s.accountID, limit)
	})
}

// idleFolderRank is a folder's place in line for real-time watching; folders
// the ranks don't know come last.
func idleFolderRank(ranks map[string]int, folderID string) int {
	if rank, ok := ranks[folderID]; ok {
		return rank
	}
	return 1 << 30
}
