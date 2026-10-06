package mail

import (
	"context"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Both layouts use the same Gmail algorithm. Each routed repository call copies
// results out of a short lease; no provider wait retains a database handle.
type gmailSyncRepository interface {
	imapSyncRepository
	AddMessageLabel(ctx context.Context, messageID int64, accountID string, label storage.LabelInput) (models.Label, error)
	CompleteGmailMessageFetch(ctx context.Context, accountID, providerMessageID string) error
	CountProviderBackedMessages(ctx context.Context, accountID string) (int, error)
	EnqueueGmailMessageFetch(ctx context.Context, accountID, providerMessageID, historyID string, fetchErr error) error
	GetLabelSyncState(ctx context.Context, accountID, providerType, scope string) (storage.LabelSyncState, error)
	GetMessageMutationInfoForFolder(ctx context.Context, messageID int64, folderID string) (*storage.MessageMutationInfo, error)
	GetMessageMutationInfoInternal(ctx context.Context, messageID int64) (*storage.MessageMutationInfo, error)
	ListDueGmailMessageFetches(ctx context.Context, accountID string, limit int) ([]storage.GmailMessageFetchQueueEntry, error)
	ListDueLabelMutations(ctx context.Context, accountID, providerType string, limit int) ([]storage.LabelMutationQueueEntry, error)
	MarkGmailMessageFetchError(ctx context.Context, entry storage.GmailMessageFetchQueueEntry, fetchErr error) error
	MarkLabelMutationError(ctx context.Context, id int64, attempts int, mutationErr error) error
	MarkLabelMutationSuccess(ctx context.Context, id int64) error
	MarkLabelSyncSuccess(ctx context.Context, accountID, providerType, scope, cursor string, full bool) error
	MarkProviderMessageDeleted(ctx context.Context, accountID, providerMessageID string) ([]string, error)
	MarkUnlistedProviderFoldersNonSelectable(ctx context.Context, accountID string, providerRemoteIDs []string) error
	ReconcileProviderFolderSeen(ctx context.Context, accountID, folderID string, providerMessageIDs []string) error
	RefreshAccountFolderThreadState(ctx context.Context, accountID string) error
	RefreshFolderThreadState(ctx context.Context, folderID string) error
	RemoveMessageLabelForProvider(ctx context.Context, messageID int64, accountID, providerType, providerID, labelName string) error
	SetMessageProviderMessageID(ctx context.Context, messageID int64, providerMessageID string) error
	UpsertExistingProviderFolderStates(ctx context.Context, accountID, folderID string, providerMessageIDs []string) (map[string]int64, error)
	UpsertProviderSyncMessages(ctx context.Context, msgs []storage.ProviderSyncMessage) (map[string]int64, error)
}

func (o *SyncOrchestrator) gmailRepository() gmailSyncRepository {
	if o.imapScope != nil {
		return o.imapScope
	}
	return o.db
}

// Unlike the legacy background finalizer, routed progress must honor session
// cancellation and revalidate the owner before saving cursor/run metadata.
func (o *SyncOrchestrator) completeGmailSyncRun(ctx context.Context, stats storage.LabelSyncRunStats, syncErr error) error {
	if o.imapScope == nil {
		return o.completeProviderLabelSyncRun(stats, syncErr)
	}
	if ctx.Err() != nil {
		return errors.Join(syncErr, ctx.Err())
	}
	stats.FinishedAt = time.Now().UTC()
	err := o.imapScope.call(ctx, func(db *storage.DB) error {
		var err error
		stats.PendingMutations, err = db.CountLabelMutations(ctx, stats.AccountID, stats.ProviderType)
		if err != nil {
			return err
		}
		return db.MarkLabelSyncRun(ctx, stats, syncErr)
	})
	return errors.Join(syncErr, err)
}

func (r *userIMAPScope) AddMessageLabel(ctx context.Context, messageID int64, accountID string, label storage.LabelInput) (models.Label, error) {
	var result models.Label
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.AddMessageLabel(ctx, messageID, accountID, label)
		return err
	})
	return result, err
}

func (r *userIMAPScope) CompleteGmailMessageFetch(ctx context.Context, accountID, providerMessageID string) error {
	return r.call(ctx, func(db *storage.DB) error { return db.CompleteGmailMessageFetch(ctx, accountID, providerMessageID) })
}

func (r *userIMAPScope) CountProviderBackedMessages(ctx context.Context, accountID string) (int, error) {
	var result int
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.CountProviderBackedMessages(ctx, accountID)
		return err
	})
	return result, err
}

func (r *userIMAPScope) EnqueueGmailMessageFetch(ctx context.Context, accountID, providerMessageID, historyID string, fetchErr error) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.EnqueueGmailMessageFetch(ctx, accountID, providerMessageID, historyID, fetchErr)
	})
}

func (r *userIMAPScope) GetLabelSyncState(ctx context.Context, accountID, providerType, scope string) (storage.LabelSyncState, error) {
	var result storage.LabelSyncState
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetLabelSyncState(ctx, accountID, providerType, scope)
		return err
	})
	return result, err
}

func (r *userIMAPScope) GetMessageMutationInfoForFolder(ctx context.Context, messageID int64, folderID string) (*storage.MessageMutationInfo, error) {
	var result *storage.MessageMutationInfo
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetMessageMutationInfoForFolder(ctx, messageID, folderID)
		return err
	})
	return result, err
}

func (r *userIMAPScope) GetMessageMutationInfoInternal(ctx context.Context, messageID int64) (*storage.MessageMutationInfo, error) {
	var result *storage.MessageMutationInfo
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.GetMessageMutationInfoInternal(ctx, messageID)
		return err
	})
	return result, err
}

func (r *userIMAPScope) ListDueGmailMessageFetches(ctx context.Context, accountID string, limit int) ([]storage.GmailMessageFetchQueueEntry, error) {
	var result []storage.GmailMessageFetchQueueEntry
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.ListDueGmailMessageFetches(ctx, accountID, limit)
		return err
	})
	return result, err
}

func (r *userIMAPScope) ListDueLabelMutations(ctx context.Context, accountID, providerType string, limit int) ([]storage.LabelMutationQueueEntry, error) {
	var result []storage.LabelMutationQueueEntry
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.ListDueLabelMutations(ctx, accountID, providerType, limit)
		return err
	})
	return result, err
}

func (r *userIMAPScope) MarkGmailMessageFetchError(ctx context.Context, entry storage.GmailMessageFetchQueueEntry, fetchErr error) error {
	return r.call(ctx, func(db *storage.DB) error { return db.MarkGmailMessageFetchError(ctx, entry, fetchErr) })
}

func (r *userIMAPScope) MarkLabelMutationError(ctx context.Context, id int64, attempts int, mutationErr error) error {
	return r.call(ctx, func(db *storage.DB) error { return db.MarkLabelMutationError(ctx, id, attempts, mutationErr) })
}

func (r *userIMAPScope) MarkLabelMutationSuccess(ctx context.Context, id int64) error {
	return r.call(ctx, func(db *storage.DB) error { return db.MarkLabelMutationSuccess(ctx, id) })
}

func (r *userIMAPScope) MarkLabelSyncSuccess(ctx context.Context, accountID, providerType, scope, cursor string, full bool) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.MarkLabelSyncSuccess(ctx, accountID, providerType, scope, cursor, full)
	})
}

func (r *userIMAPScope) MarkProviderMessageDeleted(ctx context.Context, accountID, providerMessageID string) ([]string, error) {
	var result []string
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.MarkProviderMessageDeleted(ctx, accountID, providerMessageID)
		return err
	})
	return result, err
}

func (r *userIMAPScope) MarkUnlistedProviderFoldersNonSelectable(ctx context.Context, accountID string, providerRemoteIDs []string) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.MarkUnlistedProviderFoldersNonSelectable(ctx, accountID, providerRemoteIDs)
	})
}

func (r *userIMAPScope) ReconcileProviderFolderSeen(ctx context.Context, accountID, folderID string, providerMessageIDs []string) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.ReconcileProviderFolderSeen(ctx, accountID, folderID, providerMessageIDs)
	})
}

func (r *userIMAPScope) RefreshAccountFolderThreadState(ctx context.Context, accountID string) error {
	return r.call(ctx, func(db *storage.DB) error { return db.RefreshAccountFolderThreadState(ctx, accountID) })
}

func (r *userIMAPScope) RefreshFolderThreadState(ctx context.Context, folderID string) error {
	return r.call(ctx, func(db *storage.DB) error { return db.RefreshFolderThreadState(ctx, folderID) })
}

func (r *userIMAPScope) RemoveMessageLabelForProvider(ctx context.Context, messageID int64, accountID, providerType, providerID, labelName string) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.RemoveMessageLabelForProvider(ctx, messageID, accountID, providerType, providerID, labelName)
	})
}

func (r *userIMAPScope) SetMessageProviderMessageID(ctx context.Context, messageID int64, providerMessageID string) error {
	return r.call(ctx, func(db *storage.DB) error { return db.SetMessageProviderMessageID(ctx, messageID, providerMessageID) })
}

func (r *userIMAPScope) UpsertExistingProviderFolderStates(ctx context.Context, accountID, folderID string, providerMessageIDs []string) (map[string]int64, error) {
	var result map[string]int64
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.UpsertExistingProviderFolderStates(ctx, accountID, folderID, providerMessageIDs)
		return err
	})
	return result, err
}

func (r *userIMAPScope) UpsertProviderSyncMessages(ctx context.Context, msgs []storage.ProviderSyncMessage) (map[string]int64, error) {
	var result map[string]int64
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.UpsertProviderSyncMessages(ctx, msgs)
		return err
	})
	return result, err
}
