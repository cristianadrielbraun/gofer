package mail

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Graph and Gmail share the provider message/tag SQL adapter; Graph adds folder
// delta checkpoints. Every callback ends before HTTP or token refresh begins.
type outlookSyncRepository interface {
	gmailSyncRepository
	ListProviderMessageIDBackfillCandidates(ctx context.Context, accountID string, limit int) ([]storage.ProviderMessageIDBackfillCandidate, error)
	HasProviderFolderMessagesMissingSender(ctx context.Context, accountID, folderID string) (bool, error)
	StartProviderFolderBaseline(ctx context.Context, folderID string) error
	MarkProviderMessageRemovedFromFolder(ctx context.Context, accountID, folderID, providerMessageID string) error
	RefreshFolderThreadsForMessages(ctx context.Context, folderID string, messageIDs []int64) error
	UpdateProviderFolderPageCursor(ctx context.Context, folderID, cursor string, current, totalCount, unreadCount int) error
	UpdateProviderFolderSyncState(ctx context.Context, folderID, cursor string, totalCount, unreadCount int, full bool) error
	MarkProviderMessagesMissingFromFolder(ctx context.Context, accountID, folderID string, seenProviderIDs map[string]bool) (int64, error)
	UpsertLabels(ctx context.Context, labels []storage.LabelInput) error
}

func (o *SyncOrchestrator) outlookRepository() outlookSyncRepository {
	if o.imapScope != nil {
		return o.imapScope
	}
	return o.db
}

func (o *SyncOrchestrator) outlookPersistContext(ctx context.Context) context.Context {
	if o.imapScope != nil {
		return ctx
	}
	return context.WithoutCancel(ctx)
}

func (o *SyncOrchestrator) setOutlookFolderError(ctx context.Context, folderID string, failure error) error {
	save := func(db *storage.DB) error {
		_, err := db.Write().ExecContext(ctx, `UPDATE folders SET sync_error=? WHERE id=?`, failure.Error(), folderID)
		return err
	}
	if o.imapScope != nil {
		return o.imapScope.call(ctx, save)
	}
	return save(o.db)
}

func (r *userIMAPScope) ListProviderMessageIDBackfillCandidates(ctx context.Context, accountID string, limit int) ([]storage.ProviderMessageIDBackfillCandidate, error) {
	var result []storage.ProviderMessageIDBackfillCandidate
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.ListProviderMessageIDBackfillCandidates(ctx, accountID, limit)
		return err
	})
	return result, err
}

func (r *userIMAPScope) HasProviderFolderMessagesMissingSender(ctx context.Context, accountID, folderID string) (bool, error) {
	var result bool
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.HasProviderFolderMessagesMissingSender(ctx, accountID, folderID)
		return err
	})
	return result, err
}

func (r *userIMAPScope) StartProviderFolderBaseline(ctx context.Context, folderID string) error {
	return r.call(ctx, func(db *storage.DB) error { return db.StartProviderFolderBaseline(ctx, folderID) })
}

func (r *userIMAPScope) MarkProviderMessageRemovedFromFolder(ctx context.Context, accountID, folderID, providerMessageID string) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.MarkProviderMessageRemovedFromFolder(ctx, accountID, folderID, providerMessageID)
	})
}

func (r *userIMAPScope) RefreshFolderThreadsForMessages(ctx context.Context, folderID string, messageIDs []int64) error {
	return r.call(ctx, func(db *storage.DB) error { return db.RefreshFolderThreadsForMessages(ctx, folderID, messageIDs) })
}

func (r *userIMAPScope) UpdateProviderFolderPageCursor(ctx context.Context, folderID, cursor string, current, totalCount, unreadCount int) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.UpdateProviderFolderPageCursor(ctx, folderID, cursor, current, totalCount, unreadCount)
	})
}

func (r *userIMAPScope) UpdateProviderFolderSyncState(ctx context.Context, folderID, cursor string, totalCount, unreadCount int, full bool) error {
	return r.call(ctx, func(db *storage.DB) error {
		return db.UpdateProviderFolderSyncState(ctx, folderID, cursor, totalCount, unreadCount, full)
	})
}

func (r *userIMAPScope) MarkProviderMessagesMissingFromFolder(ctx context.Context, accountID, folderID string, seenProviderIDs map[string]bool) (int64, error) {
	var result int64
	err := r.call(ctx, func(db *storage.DB) error {
		var err error
		result, err = db.MarkProviderMessagesMissingFromFolder(ctx, accountID, folderID, seenProviderIDs)
		return err
	})
	return result, err
}

func (r *userIMAPScope) UpsertLabels(ctx context.Context, labels []storage.LabelInput) error {
	return r.call(ctx, func(db *storage.DB) error { return db.UpsertLabels(ctx, labels) })
}

type graphJSONRequest func(context.Context, string, string, string, map[string]string, any, any) error

// Opaque Graph cursors are replayed unchanged, but may only address this API.
func validateUserGraphEndpoint(endpoint string) error {
	base, err := url.Parse(outlookGraphBaseURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if target.User != nil || target.Scheme != base.Scheme || target.Host != base.Host ||
		!strings.HasPrefix(target.Path, strings.TrimRight(base.Path, "/")+"/") || target.Fragment != "" {
		return fmt.Errorf("Graph cursor is outside the configured API")
	}
	return nil
}

func (o *SyncOrchestrator) outlookRequest(ctx context.Context, token string, request func(string) error) error {
	if o.imapScope == nil {
		return request(token)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.userGraphRetryErr != nil {
		return o.userGraphRetryErr
	}
	until, retryErr := o.imapScope.accounts.Routing().ProviderRetryUntil(ctx, o.imapScope.owner, o.imapScope.id)
	if retryErr != nil {
		return retryErr
	}
	if until.After(time.Now()) {
		o.imapScope.retryAt = until
		return fmt.Errorf("provider retry is deferred until %s", until.Format(time.RFC3339))
	}
	if o.userGraphToken != "" {
		token = o.userGraphToken
	}
	err := request(token)
	if !providerAPIUnauthorized(err) {
		return o.recordOutlookRetry(ctx, err)
	}
	refresh, ok := o.tokenProvider.(refreshingTokenProvider)
	if !ok {
		return err
	}
	token, err = refresh.RefreshOAuthTokenForAccount(ctx, o.imapScope.id)
	if err != nil {
		return o.recordOutlookRetry(ctx, err)
	}
	o.userGraphToken = token
	return o.recordOutlookRetry(ctx, request(token))
}

func (o *SyncOrchestrator) recordOutlookRetry(ctx context.Context, err error) error {
	if o.imapScope == nil {
		return err
	}
	var at time.Time
	var api *providerAPIError
	if errors.As(err, &api) {
		at = api.RetryAt
	}
	var retryHint interface{ RetryAfter() (time.Time, bool) }
	if errors.As(err, &retryHint) {
		if hint, ok := retryHint.RetryAfter(); ok && hint.After(at) {
			at = hint
		}
	}
	if at.After(time.Now()) {
		o.imapScope.retryAt = at
		o.userGraphRetryErr = err
		if saveErr := o.imapScope.accounts.Routing().DeferProviderRetry(ctx, o.imapScope.owner, o.imapScope.id, at); saveErr != nil {
			return errors.Join(err, saveErr)
		}
	}
	return err
}

func (o *SyncOrchestrator) outlookJSON(ctx context.Context, method, endpoint, token string, headers map[string]string, body, out any) error {
	if o.imapScope != nil {
		if err := validateUserGraphEndpoint(endpoint); err != nil {
			return err
		}
	}
	return o.outlookRequest(ctx, token, func(access string) error { return providerJSON(ctx, method, endpoint, access, headers, body, out) })
}
