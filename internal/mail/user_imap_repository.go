package mail

import (
	"context"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Both layouts use the same IMAP synchronization algorithm. Routed repository
// methods lease a store only for the local operation, never for protocol waits.
type imapSyncRepository interface {
	GetFolderRole(ctx context.Context, folderID string) (string, error)
	ResetFolderUIDState(ctx context.Context, folderID string, uidValidity uint32) error
	GetFolderEmailCount(ctx context.Context, folderID string) (int, error)
	GetStoredUIDValidity(ctx context.Context, folderID string) (uint32, error)
	GetHighestSeenUID(ctx context.Context, folderID string) (uint32, error)
	UpsertSyncMessages(ctx context.Context, msgs []storage.SyncMessage) error
	UpdateFolderIncrementalSync(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error
	RefreshFolderUnreadCount(ctx context.Context, folderID string) (int, error)
	GetLocalUIDs(ctx context.Context, folderID string) (map[uint32]int64, error)
	RemoveExpungedUIDs(ctx context.Context, folderID string, expungedUIDs []uint32) (int, error)
	ApplyIMAPFlagChanges(ctx context.Context, folderID string, expectedUIDValidity uint32, updates []storage.FlagUpdate, highestModSeq uint64) (int, error)
	BatchUpdateFlags(ctx context.Context, folderID string, updates []storage.FlagUpdate) (int, error)
	UpsertFolders(ctx context.Context, folders []storage.UpsertFolderInput) error
	ReconcileDiscoveredFolders(ctx context.Context, accountID, providerKind string, seenIdentities []string, completedAt time.Time) (storage.FolderDiscoveryResult, error)
	GetFoldersForAccount(ctx context.Context, accountID string) ([]storage.FolderSyncInfo, error)
	UpdateFolderSyncState(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error
}

func (o *SyncOrchestrator) imapRepository() imapSyncRepository {
	if o.imapScope != nil {
		return o.imapScope
	}
	return o.db
}
func (o *SyncOrchestrator) imapAccountConfig(ctx context.Context, id string) (*models.AccountConfig, error) {
	if o.imapScope != nil {
		return o.imapScope.config, nil
	}
	return o.accountStore.GetConfig(ctx, id)
}
func (o *SyncOrchestrator) imapAccount(ctx context.Context, id string) (*models.Account, error) {
	if o.imapScope != nil {
		return o.imapScope.account, nil
	}
	return o.accountStore.GetAccountByID(ctx, id)
}
func (o *SyncOrchestrator) imapSenderAvatarURL(ctx context.Context, email string) string {
	// User avatar cache routing is a separate conversion; do not consult shared
	// private data from a routed IMAP event.
	if o.imapScope != nil {
		return ""
	}
	return o.senderAvatarURL(ctx, email)
}
func (r *userIMAPScope) GetFolderRole(ctx context.Context, folderID string) (string, error) {
	var result string
	err := r.call(ctx, func(db *storage.DB) error { var err error; result, err = db.GetFolderRole(ctx, folderID); return err })
	return result, err
}
func (r *userIMAPScope) ResetFolderUIDState(ctx context.Context, folderID string, uidValidity uint32) error {
	return r.call(ctx, func(db *storage.DB) error { return db.ResetFolderUIDState(ctx, folderID, uidValidity) })
}
func (r *userIMAPScope) GetFolderEmailCount(ctx context.Context, folderID string) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetFolderEmailCount(ctx, folderID)
		return err
	})
	return result, err
}
func (r *userIMAPScope) GetStoredUIDValidity(ctx context.Context, folderID string) (uint32, error) {
	var result uint32
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetStoredUIDValidity(ctx, folderID)
		return err
	})
	return result, err
}
func (r *userIMAPScope) GetHighestSeenUID(ctx context.Context, folderID string) (uint32, error) {
	var result uint32
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetHighestSeenUID(ctx, folderID)
		return err
	})
	return result, err
}
func (r *userIMAPScope) UpsertSyncMessages(ctx context.Context, msgs []storage.SyncMessage) error {
	return r.call(ctx, func(db *storage.DB) error { return db.UpsertSyncMessages(ctx, msgs) })
}
func (r *userIMAPScope) UpdateFolderIncrementalSync(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.UpdateFolderIncrementalSync(ctx, folderID, highestUID, uidValidity, totalCount)
	})
}
func (r *userIMAPScope) RefreshFolderUnreadCount(ctx context.Context, folderID string) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.RefreshFolderUnreadCount(ctx, folderID)
		return err
	})
	return result, err
}
func (r *userIMAPScope) GetLocalUIDs(ctx context.Context, folderID string) (map[uint32]int64, error) {
	var result map[uint32]int64
	err := r.call(ctx, func(db *storage.DB) error { var err error; result, err = db.GetLocalUIDs(ctx, folderID); return err })
	return result, err
}
func (r *userIMAPScope) RemoveExpungedUIDs(ctx context.Context, folderID string, expungedUIDs []uint32) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.RemoveExpungedUIDs(ctx, folderID, expungedUIDs)
		return err
	})
	return result, err
}
func (r *userIMAPScope) ApplyIMAPFlagChanges(ctx context.Context, folderID string, expectedUIDValidity uint32, updates []storage.FlagUpdate, highestModSeq uint64) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.ApplyIMAPFlagChanges(ctx, folderID, expectedUIDValidity, updates, highestModSeq)
		return err
	})
	return result, err
}
func (r *userIMAPScope) BatchUpdateFlags(ctx context.Context, folderID string, updates []storage.FlagUpdate) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.BatchUpdateFlags(ctx, folderID, updates)
		return err
	})
	return result, err
}
func (r *userIMAPScope) UpsertFolders(ctx context.Context, folders []storage.UpsertFolderInput) error {
	return r.call(ctx, func(db *storage.DB) error { return db.UpsertFolders(ctx, folders) })
}
func (r *userIMAPScope) ReconcileDiscoveredFolders(ctx context.Context, accountID, providerKind string, seenIdentities []string, completedAt time.Time) (storage.FolderDiscoveryResult, error) {
	var result storage.FolderDiscoveryResult
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.ReconcileDiscoveredFolders(ctx, accountID, providerKind, seenIdentities, completedAt)
		return err
	})
	return result, err
}
func (r *userIMAPScope) GetFoldersForAccount(ctx context.Context, accountID string) ([]storage.FolderSyncInfo, error) {
	var result []storage.FolderSyncInfo
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetFoldersForAccount(ctx, accountID)
		return err
	})
	return result, err
}
func (r *userIMAPScope) UpdateFolderSyncState(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.UpdateFolderSyncState(ctx, folderID, highestUID, uidValidity, totalCount)
	})
}

func (o *SyncOrchestrator) publishEvent(event Event) {
	if o.imapScope != nil {
		event.UserID = o.imapScope.owner
	}
	o.events.Publish(event)
}

func (o *SyncOrchestrator) newIMAPSyncClient(ctx context.Context, cfg *models.AccountConfig, password string) (*imap.Client, error) {
	if o.imapScope != nil {
		return imap.NewContextClient(ctx, cfg, password)
	}
	return imap.NewClient(ctx, cfg, password)
}
