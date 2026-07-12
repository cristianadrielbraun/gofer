package mail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const gmailAPIMessagePageSize = 500
const gmailAPIRecentCatchupMaxPages = 2
const gmailAPIRecentCatchupScope = "messages_recent_catchup"
const gmailAPIHistoricalMetadataWorkers = 6
const gmailAPIHistoricalMetadataPerMinute = 240
const gmailAPIHistoricalMetadataBatchSize = 100
const gmailAPIMessageMetadataMaxAttempts = 5

var gmailAPIMessageMetadataHeaders = []string{
	"Message-ID",
	"Subject",
	"From",
	"To",
	"Cc",
	"Bcc",
	"Date",
	"In-Reply-To",
	"References",
}

type gmailAPILabel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	MessagesTotal   int    `json:"messagesTotal"`
	MessagesUnread  int    `json:"messagesUnread"`
	MessageListView string `json:"messageListVisibility"`
	LabelListView   string `json:"labelListVisibility"`
}

type gmailAPILabelsResponse struct {
	Labels []gmailAPILabel `json:"labels"`
}

type gmailAPIMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type gmailAPIMessageListResponse struct {
	Messages      []gmailAPIMessageRef `json:"messages"`
	NextPageToken string               `json:"nextPageToken"`
	ResultSize    int                  `json:"resultSizeEstimate"`
}

type gmailAPIHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailAPIBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"`
}

type gmailAPIPart struct {
	PartID   string           `json:"partId"`
	MimeType string           `json:"mimeType"`
	Filename string           `json:"filename"`
	Headers  []gmailAPIHeader `json:"headers"`
	Body     gmailAPIBody     `json:"body"`
	Parts    []gmailAPIPart   `json:"parts"`
}

type gmailAPIMessage struct {
	ID           string       `json:"id"`
	ThreadID     string       `json:"threadId"`
	LabelIDs     []string     `json:"labelIds"`
	Snippet      string       `json:"snippet"`
	HistoryID    string       `json:"historyId"`
	InternalDate string       `json:"internalDate"`
	Payload      gmailAPIPart `json:"payload"`
	SizeEstimate int64        `json:"sizeEstimate"`
}

type gmailAPIFolderSyncTarget struct {
	Folder  storage.FolderSyncInfo
	LabelID string
}

type gmailAPIMessageSyncResult struct {
	ProviderMessageID string
	LocalMessageID    int64
	HistoryID         string
	FolderIDs         []string
	Synced            bool
	Skipped           bool
}

type gmailAPIMessageFetchResult struct {
	ProviderMessageID string
	Message           gmailAPIMessage
	Err               error
}

type gmailAPISharedToken struct {
	mu       sync.Mutex
	token    string
	external *string
}

type gmailAPIMetadataLimiter struct {
	tokens chan struct{}
	once   sync.Once
	stop   func()
}

func gmailAPIMailEnabled() bool {
	return true
}

func (o *SyncOrchestrator) shouldUseGmailAPIMail(cfg *models.AccountConfig) bool {
	if cfg == nil || strings.TrimSpace(cfg.Provider) != providers.ProviderGmail || !gmailAPIMailEnabled() {
		return false
	}
	return o != nil && o.tokenProvider != nil
}

func (o *SyncOrchestrator) syncGmailAPIAccount(ctx context.Context, accountID string, includeIDLEFolders bool) error {
	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}

	targets, labelsByID, err := o.syncGmailAPIFoldersWithAuthRetry(ctx, accountID, &token)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: accountSyncProgressPayload(ctx, accountSyncBackground, map[string]any{
			"status":                "ok",
			"account_folders_total": 0,
			"provider":              "gmail_api",
		})})
		return nil
	}

	state, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, "messages")
	if err != nil {
		return err
	}
	cursor, seeded, err := o.ensureGmailAPILiveCursor(ctx, accountID, &token, state)
	if err != nil {
		return err
	}
	if !state.LastFullSyncAt.Valid {
		shouldImport, err := o.shouldRunGmailAPIInitialImport(ctx, accountID, state)
		if err != nil {
			return err
		}
		if shouldImport {
			if err := o.syncGmailAPIHistoricalImport(ctx, accountID, &token, labelsByID, targets, false); err != nil {
				return err
			}
			if seeded {
				if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, "messages", cursor, false); err != nil {
					return err
				}
			}
			return o.db.RefreshAccountFolderThreadState(ctx, accountID)
		}
		shouldCatchup, err := o.shouldRunGmailAPIRecentCatchup(ctx, accountID, state)
		if err != nil {
			return err
		}
		if shouldCatchup {
			if err := o.syncGmailAPIRecentCatchup(ctx, accountID, &token, labelsByID, targets); err != nil {
				return err
			}
			if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, gmailAPIRecentCatchupScope, "", false); err != nil {
				return err
			}
		}
	}
	if seeded {
		if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, "messages", cursor, false); err != nil {
			return err
		}
		return o.db.RefreshAccountFolderThreadState(ctx, accountID)
	}

	if err := o.syncGmailAPIHistoryChanges(ctx, accountID, token, labelsByID, targets, cursor); err != nil {
		return err
	}
	return o.db.RefreshAccountFolderThreadState(ctx, accountID)
}

func (o *SyncOrchestrator) repairGmailAPIAccount(ctx context.Context, accountID string) error {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return nil
	}
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return err
	}
	if !o.shouldUseGmailAPIMail(cfg) {
		return fmt.Errorf("full mail repair is only available for Gmail accounts")
	}
	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	targets, labelsByID, err := o.syncGmailAPIFoldersWithAuthRetry(ctx, accountID, &token)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: accountSyncProgressPayload(ctx, accountSyncManual, map[string]any{
			"status":                "ok",
			"account_folders_total": 0,
			"provider":              "gmail_api",
			"mode":                  "repair",
		})})
		return nil
	}
	if err := o.syncGmailAPIHistoricalImport(ctx, accountID, &token, labelsByID, targets, true); err != nil {
		return err
	}
	cursor, err := o.getGmailProfileHistoryIDWithAuthRetry(ctx, accountID, &token)
	if err != nil {
		return err
	}
	if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, "messages", cursor, true); err != nil {
		return err
	}
	if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, gmailAPIRecentCatchupScope, "", false); err != nil {
		return err
	}
	return o.db.RefreshAccountFolderThreadState(ctx, accountID)
}

func (o *SyncOrchestrator) syncGmailAPIFoldersWithAuthRetry(ctx context.Context, accountID string, token *string) ([]gmailAPIFolderSyncTarget, map[string]gmailAPILabel, error) {
	current := ""
	if token != nil {
		current = *token
	}
	targets, labelsByID, err := o.syncGmailAPIFolders(ctx, accountID, current)
	refreshed, ok := o.refreshGmailTokenAfterUnauthorized(ctx, accountID, token, err)
	if !ok {
		return targets, labelsByID, err
	}
	return o.syncGmailAPIFolders(ctx, accountID, refreshed)
}

func (o *SyncOrchestrator) ensureGmailAPILiveCursor(ctx context.Context, accountID string, token *string, state storage.LabelSyncState) (string, bool, error) {
	cursor := strings.TrimSpace(state.Cursor)
	if cursor != "" && state.LastSuccessAt.Valid {
		return cursor, false, nil
	}
	cursor, err := o.getGmailProfileHistoryIDWithAuthRetry(ctx, accountID, token)
	if err != nil {
		return "", false, err
	}
	return cursor, true, nil
}

func (o *SyncOrchestrator) shouldRunGmailAPIInitialImport(ctx context.Context, accountID string, state storage.LabelSyncState) (bool, error) {
	if state.LastFullSyncAt.Valid {
		return false, nil
	}
	count, err := o.db.CountProviderBackedMessages(ctx, accountID)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

func (o *SyncOrchestrator) shouldRunGmailAPIRecentCatchup(ctx context.Context, accountID string, state storage.LabelSyncState) (bool, error) {
	if state.LastFullSyncAt.Valid {
		return false, nil
	}
	catchupState, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, gmailAPIRecentCatchupScope)
	if err != nil {
		return false, err
	}
	return !catchupState.LastSuccessAt.Valid, nil
}

func (o *SyncOrchestrator) syncGmailAPIFolders(ctx context.Context, accountID, token string) ([]gmailAPIFolderSyncTarget, map[string]gmailAPILabel, error) {
	var response gmailAPILabelsResponse
	if err := providerJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", token, nil, nil, &response); err != nil {
		return nil, nil, err
	}

	labelsByID := make(map[string]gmailAPILabel, len(response.Labels))
	inputs := make([]storage.UpsertFolderInput, 0, len(response.Labels)+1)
	for i, label := range response.Labels {
		label.ID = strings.TrimSpace(label.ID)
		label.Name = strings.TrimSpace(label.Name)
		if label.ID == "" {
			continue
		}
		labelsByID[label.ID] = label
		if !gmailAPILabelSelectable(label) {
			continue
		}
		remoteName := gmailAPILabelRemoteName(label)
		role := gmailAPILabelRole(label)
		inputs = append(inputs, storage.UpsertFolderInput{
			ID:               storage.FolderIDForIdentity(accountID, "gmail", label.ID),
			AccountID:        accountID,
			RemoteID:         remoteName,
			ProviderRemoteID: label.ID,
			Name:             displayName(remoteName, role),
			Icon:             imap.RoleIcon(role),
			Role:             role,
			Selectable:       true,
			SortOrder:        gmailAPIFolderSortOrder(role, i),
		})
	}
	archiveRemote := "[Gmail]/All Mail"
	inputs = append(inputs, storage.UpsertFolderInput{
		ID:               storage.FolderIDForIdentity(accountID, "gmail", "ARCHIVE"),
		AccountID:        accountID,
		RemoteID:         archiveRemote,
		ProviderRemoteID: "ARCHIVE",
		Name:             "Archive",
		Icon:             imap.RoleIcon("archive"),
		Role:             "archive",
		Selectable:       true,
		SortOrder:        gmailAPIFolderSortOrder("archive", len(inputs)),
	})

	if len(inputs) > 0 {
		if err := o.db.UpsertFolders(ctx, inputs); err != nil {
			return nil, nil, err
		}
		if err := o.db.MarkUnlistedProviderFoldersNonSelectable(ctx, accountID, providerRemoteIDsFromFolderInputs(inputs)); err != nil {
			return nil, nil, err
		}
	}
	localFolders, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	targets := make([]gmailAPIFolderSyncTarget, 0, len(localFolders))
	for _, folder := range localFolders {
		labelID := strings.TrimSpace(folder.ProviderRemoteID)
		if labelID == "" {
			continue
		}
		if labelID != "ARCHIVE" {
			if _, ok := labelsByID[labelID]; !ok {
				continue
			}
		}
		targets = append(targets, gmailAPIFolderSyncTarget{Folder: folder, LabelID: labelID})
	}
	sort.SliceStable(targets, func(i, j int) bool {
		left := gmailAPIFolderSortOrder(targets[i].Folder.Role, i)
		right := gmailAPIFolderSortOrder(targets[j].Folder.Role, j)
		if left != right {
			return left < right
		}
		return strings.ToLower(targets[i].Folder.RemoteID) < strings.ToLower(targets[j].Folder.RemoteID)
	})
	return targets, labelsByID, nil
}

func (o *SyncOrchestrator) syncGmailAPIHistoricalImport(ctx context.Context, accountID string, token *string, labelsByID map[string]gmailAPILabel, targets []gmailAPIFolderSyncTarget, refreshKnown bool) (retErr error) {
	stats := storage.LabelSyncRunStats{
		AccountID:    accountID,
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		StartedAt:    time.Now().UTC(),
		Full:         true,
	}
	defer func() {
		retErr = o.completeProviderLabelSyncRun(stats, retErr)
	}()

	targetsByLabelID := gmailAPITargetsByLabelID(targets)
	targetsByFolderID := gmailAPITargetsByFolderID(targets)
	seenProviderIDs := map[string]bool{}
	seenProviderIDsByFolder := make(map[string]map[string]bool, len(targets))
	touchedFolders := map[string]bool{}
	syncedFolders := map[string]bool{}
	failed := 0
	processed := 0
	totalEstimate := 0
	sharedToken := newGmailAPISharedToken(token)
	metadataLimiter := newGmailAPIMetadataLimiter(gmailAPIHistoricalMetadataPerMinute, gmailAPIHistoricalMetadataWorkers)
	defer metadataLimiter.Stop()
	publishProgress := func() {
		total := totalEstimate
		if total < processed {
			total = processed
		}
		o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, true)
		o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, Current: processed, Total: total, Payload: accountSyncProgressPayload(ctx, "", map[string]any{
			"provider": "gmail_api",
		})})
	}
	processProviderIDs := func(providerIDs []string) error {
		for _, batch := range gmailAPIProviderIDChunks(providerIDs, gmailAPIHistoricalMetadataBatchSize) {
			fetched := o.fetchGmailAPIMessageMetadataBatch(ctx, accountID, sharedToken, metadataLimiter, batch)
			if err := ctx.Err(); err != nil {
				return err
			}
			messages := make([]gmailAPIMessage, 0, len(fetched))
			for _, result := range fetched {
				if result.Err == nil {
					messages = append(messages, result.Message)
					continue
				}
				if providerLabelSyncShouldStop(result.Err) {
					stats.FailedMessages++
					return result.Err
				}
				failed++
				stats.FailedMessages++
				if providerMessageNotFound(result.Err) {
					stats.MissingProviderMessages++
				}
				log.Printf("gmail api sync message account=%s provider_message=%s: %v", accountID, result.ProviderMessageID, result.Err)
			}
			results, err := o.upsertGmailAPIProviderMessageBatch(ctx, accountID, messages, labelsByID, targetsByLabelID)
			if err != nil {
				return err
			}
			for _, result := range results {
				if result.Skipped {
					stats.SkippedMessages++
					continue
				}
				stats.SyncedMessages++
				stats.WithLabels++
				recordGmailAPITouchedFolders(touchedFolders, result.FolderIDs)
				recordGmailAPITouchedFolders(syncedFolders, result.FolderIDs)
			}
			publishProgress()
		}
		return nil
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if gmailAPIHistoricalImportIsRepair(ctx) {
			o.events.Publish(Event{
				Type:       EventSyncStarted,
				AccountID:  accountID,
				FolderID:   target.Folder.ID,
				FolderRole: target.Folder.Role,
				Payload:    o.gmailAPIFolderRefreshPayload(ctx, accountID, displayName(target.Folder.RemoteID, target.Folder.Role), true),
			})
		}
		if strings.TrimSpace(target.Folder.ID) != "" && seenProviderIDsByFolder[target.Folder.ID] == nil {
			seenProviderIDsByFolder[target.Folder.ID] = map[string]bool{}
		}
		pageToken := ""
		estimateRecorded := false
		for {
			page, err := o.listGmailAPIMessageIDPageWithAuthRetry(ctx, accountID, token, target.LabelID, pageToken)
			if err != nil {
				return err
			}
			if !estimateRecorded && page.ResultSize > 0 {
				totalEstimate += page.ResultSize
				estimateRecorded = true
			}
			pageProviderIDs := gmailAPIMessageRefIDs(page.Messages)
			if folderSeen := seenProviderIDsByFolder[target.Folder.ID]; folderSeen != nil {
				for _, providerID := range pageProviderIDs {
					folderSeen[providerID] = true
				}
			}
			existingByProvider, err := o.db.UpsertExistingProviderFolderStates(ctx, accountID, target.Folder.ID, pageProviderIDs)
			if err != nil {
				return err
			}
			if len(existingByProvider) > 0 {
				touchedFolders[target.Folder.ID] = true
				syncedFolders[target.Folder.ID] = true
			}
			pendingProviderIDs := make([]string, 0, len(page.Messages))
			for _, ref := range page.Messages {
				providerID := strings.TrimSpace(ref.ID)
				if providerID == "" || seenProviderIDs[providerID] {
					continue
				}
				seenProviderIDs[providerID] = true
				processed++
				if _, ok := existingByProvider[providerID]; ok && !refreshKnown {
					stats.SyncedMessages++
					recordGmailAPITouchedFolders(touchedFolders, []string{target.Folder.ID})
					recordGmailAPITouchedFolders(syncedFolders, []string{target.Folder.ID})
					if processed%25 == 0 {
						publishProgress()
					}
					continue
				}
				pendingProviderIDs = append(pendingProviderIDs, providerID)
			}
			if err := processProviderIDs(pendingProviderIDs); err != nil {
				return err
			}
			pageToken = strings.TrimSpace(page.NextPageToken)
			if pageToken == "" {
				break
			}
		}
	}
	for folderID, providerIDs := range seenProviderIDsByFolder {
		if err := o.db.ReconcileProviderFolderSeen(ctx, accountID, folderID, gmailAPIProviderIDSetValues(providerIDs)); err != nil {
			return err
		}
	}
	stats.TotalMessages = len(seenProviderIDs)
	total := totalEstimate
	if total < processed {
		total = processed
	}
	stats.Cursor = ""
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, true)
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncComplete, accountID, syncedFolders, targetsByFolderID, processed, total, false, true)
	currentToken := ""
	if token != nil {
		currentToken = *token
	}
	o.replayGmailLabelMutationQueue(ctx, accountID, currentToken)
	if failed > 0 {
		return fmt.Errorf("%d Gmail API message import(s) failed", failed)
	}
	return nil
}

func (o *SyncOrchestrator) syncGmailAPIRecentCatchup(ctx context.Context, accountID string, token *string, labelsByID map[string]gmailAPILabel, targets []gmailAPIFolderSyncTarget) error {
	targetsByLabelID := gmailAPITargetsByLabelID(targets)
	targetsByFolderID := gmailAPITargetsByFolderID(targets)
	seenProviderIDs := map[string]bool{}
	touchedFolders := map[string]bool{}
	syncedFolders := map[string]bool{}
	processed := 0
	failed := 0

	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageToken := ""
		for pageIndex := 0; pageIndex < gmailAPIRecentCatchupMaxPages; pageIndex++ {
			page, err := o.listGmailAPIMessageIDPageWithAuthRetry(ctx, accountID, token, target.LabelID, pageToken)
			if err != nil {
				return err
			}
			pageProviderIDs := gmailAPIMessageRefIDs(page.Messages)
			existingByProvider, err := o.db.UpsertExistingProviderFolderStates(ctx, accountID, target.Folder.ID, pageProviderIDs)
			if err != nil {
				return err
			}
			if len(existingByProvider) > 0 {
				touchedFolders[target.Folder.ID] = true
				syncedFolders[target.Folder.ID] = true
			}
			for _, ref := range page.Messages {
				providerID := strings.TrimSpace(ref.ID)
				if providerID == "" || seenProviderIDs[providerID] {
					continue
				}
				seenProviderIDs[providerID] = true
				if _, ok := existingByProvider[providerID]; ok {
					continue
				}
				processed++
				result, err := o.syncGmailAPIProviderMessageWithAuthRetry(ctx, accountID, token, providerID, labelsByID, targetsByLabelID)
				if err != nil {
					if providerLabelSyncShouldStop(err) {
						return err
					}
					failed++
					log.Printf("gmail api recent catch-up account=%s provider_message=%s: %v", accountID, providerID, err)
					continue
				}
				if result.Skipped {
					continue
				}
				recordGmailAPITouchedFolders(touchedFolders, result.FolderIDs)
				recordGmailAPITouchedFolders(syncedFolders, result.FolderIDs)
				if processed%25 == 0 {
					total := processed
					if total == 0 {
						total = len(seenProviderIDs)
					}
					o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, true)
				}
			}
			pageToken = strings.TrimSpace(page.NextPageToken)
			if pageToken == "" {
				break
			}
		}
	}
	total := processed
	if total == 0 {
		total = len(seenProviderIDs)
	}
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, true)
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncComplete, accountID, syncedFolders, targetsByFolderID, processed, total, false, true)
	currentToken := ""
	if token != nil {
		currentToken = *token
	}
	o.replayGmailLabelMutationQueue(ctx, accountID, currentToken)
	if failed > 0 {
		return fmt.Errorf("%d Gmail API recent message catch-up import(s) failed", failed)
	}
	return nil
}

func gmailAPIHistoricalImportIsRepair(ctx context.Context) bool {
	scope, ok := ctx.Value(accountSyncProgressScopeKey{}).(accountSyncProgressScope)
	return ok && strings.TrimSpace(scope.mode) == "repair"
}

func (o *SyncOrchestrator) syncGmailAPIHistoryChanges(ctx context.Context, accountID, token string, labelsByID map[string]gmailAPILabel, targets []gmailAPIFolderSyncTarget, cursor string) (retErr error) {
	stats := storage.LabelSyncRunStats{
		AccountID:    accountID,
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		StartedAt:    time.Now().UTC(),
		Full:         false,
		Cursor:       strings.TrimSpace(cursor),
	}
	fallbackFull := false
	defer func() {
		if fallbackFull {
			return
		}
		retErr = o.completeProviderLabelSyncRun(stats, retErr)
	}()

	providerIDs, latestCursor, err := o.gmailHistoryChangedMessageIDsWithAuthRetry(ctx, accountID, &token, cursor)
	if err != nil {
		if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
			log.Printf("gmail api sync account=%s history cursor expired, reseeding cursor and running recent catch-up", accountID)
			fallbackFull = true
			return o.recoverGmailAPIExpiredHistory(ctx, accountID, &token, labelsByID, targets)
		}
		return err
	}
	stats.Cursor = newerGmailHistoryID(stats.Cursor, latestCursor)
	stats.TotalMessages = len(providerIDs)
	targetsByLabelID := gmailAPITargetsByLabelID(targets)
	targetsByFolderID := gmailAPITargetsByFolderID(targets)

	failed := 0
	processed := 0
	touchedFolders := map[string]bool{}
	syncedFolders := map[string]bool{}
	for _, providerID := range providerIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed++
		result, err := o.syncGmailAPIProviderMessageWithAuthRetry(ctx, accountID, &token, providerID, labelsByID, targetsByLabelID)
		if err != nil {
			if providerLabelSyncShouldStop(err) {
				stats.FailedMessages++
				return err
			}
			failed++
			stats.FailedMessages++
			if providerMessageNotFound(err) {
				stats.MissingProviderMessages++
				if confirmErr := o.db.ConfirmProviderMessageDeleted(ctx, accountID, providerID); confirmErr != nil {
					return confirmErr
				}
			}
			log.Printf("gmail api sync history account=%s provider_message=%s: %v", accountID, providerID, err)
			continue
		}
		stats.Cursor = newerGmailHistoryID(stats.Cursor, result.HistoryID)
		if result.Skipped {
			stats.SkippedMessages++
			continue
		}
		stats.SyncedMessages++
		stats.WithLabels++
		recordGmailAPITouchedFolders(touchedFolders, result.FolderIDs)
		recordGmailAPITouchedFolders(syncedFolders, result.FolderIDs)
		if processed%25 == 0 {
			total := stats.TotalMessages
			if total < processed {
				total = processed
			}
			o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, false)
		}
	}
	total := stats.TotalMessages
	if total < processed {
		total = processed
	}
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncProgress, accountID, touchedFolders, targetsByFolderID, processed, total, true, false)
	o.publishGmailAPIFolderSyncEvent(ctx, EventSyncComplete, accountID, syncedFolders, targetsByFolderID, processed, total, false, false)
	o.replayGmailLabelMutationQueue(ctx, accountID, token)
	if failed > 0 {
		return fmt.Errorf("%d Gmail API message change import(s) failed", failed)
	}
	return nil
}

func (o *SyncOrchestrator) recoverGmailAPIExpiredHistory(ctx context.Context, accountID string, token *string, labelsByID map[string]gmailAPILabel, targets []gmailAPIFolderSyncTarget) error {
	cursor, err := o.getGmailProfileHistoryIDWithAuthRetry(ctx, accountID, token)
	if err != nil {
		return err
	}
	state, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, "messages")
	if err != nil {
		return err
	}
	shouldImport, err := o.shouldRunGmailAPIInitialImport(ctx, accountID, state)
	if err != nil {
		return err
	}
	if shouldImport {
		if err := o.syncGmailAPIHistoricalImport(ctx, accountID, token, labelsByID, targets, false); err != nil {
			return err
		}
	} else {
		if err := o.syncGmailAPIRecentCatchup(ctx, accountID, token, labelsByID, targets); err != nil {
			return err
		}
		if err := o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, gmailAPIRecentCatchupScope, "", false); err != nil {
			return err
		}
	}
	return o.db.MarkLabelSyncSuccess(ctx, accountID, storage.LabelProviderGmail, "messages", cursor, false)
}

func (o *SyncOrchestrator) syncGmailAPIProviderMessage(ctx context.Context, accountID, token, providerMessageID string, labelsByID map[string]gmailAPILabel, targetsByLabelID map[string]gmailAPIFolderSyncTarget) (gmailAPIMessageSyncResult, error) {
	msg, err := getGmailAPIMessageMetadata(ctx, token, providerMessageID)
	if err != nil {
		return gmailAPIMessageSyncResult{}, err
	}
	upserts := gmailAPIMessageToProviderSyncs(accountID, msg, labelsByID, targetsByLabelID)
	if len(upserts) == 0 {
		return gmailAPIMessageSyncResult{ProviderMessageID: msg.ID, HistoryID: msg.HistoryID, Skipped: true}, nil
	}
	idsByProvider, err := o.db.UpsertProviderSyncMessages(ctx, upserts)
	if err != nil {
		return gmailAPIMessageSyncResult{}, err
	}
	localID := idsByProvider[strings.TrimSpace(msg.ID)]
	if localID == 0 {
		return gmailAPIMessageSyncResult{ProviderMessageID: msg.ID, HistoryID: msg.HistoryID, Skipped: true}, nil
	}
	folderIDs := gmailAPIProviderSyncFolderIDs(upserts)
	return gmailAPIMessageSyncResult{ProviderMessageID: msg.ID, LocalMessageID: localID, HistoryID: msg.HistoryID, FolderIDs: folderIDs, Synced: true}, nil
}

func (o *SyncOrchestrator) syncGmailAPIProviderMessageWithAuthRetry(ctx context.Context, accountID string, token *string, providerMessageID string, labelsByID map[string]gmailAPILabel, targetsByLabelID map[string]gmailAPIFolderSyncTarget) (gmailAPIMessageSyncResult, error) {
	current := ""
	if token != nil {
		current = *token
	}
	result, err := o.syncGmailAPIProviderMessage(ctx, accountID, current, providerMessageID, labelsByID, targetsByLabelID)
	if !providerAPIUnauthorized(err) {
		return result, err
	}
	refreshed, refreshErr := o.refreshOAuthTokenForAccount(ctx, accountID)
	if refreshErr != nil {
		return result, err
	}
	if token != nil {
		*token = refreshed
	}
	return o.syncGmailAPIProviderMessage(ctx, accountID, refreshed, providerMessageID, labelsByID, targetsByLabelID)
}

func (o *SyncOrchestrator) upsertGmailAPIProviderMessageBatch(ctx context.Context, accountID string, messages []gmailAPIMessage, labelsByID map[string]gmailAPILabel, targetsByLabelID map[string]gmailAPIFolderSyncTarget) ([]gmailAPIMessageSyncResult, error) {
	type messagePlan struct {
		providerID string
		historyID  string
		folderIDs  []string
		skipped    bool
	}

	plans := make([]messagePlan, 0, len(messages))
	upserts := make([]storage.ProviderSyncMessage, 0, len(messages))
	for _, msg := range messages {
		providerID := strings.TrimSpace(msg.ID)
		if providerID == "" {
			continue
		}
		messageUpserts := gmailAPIMessageToProviderSyncs(accountID, msg, labelsByID, targetsByLabelID)
		plan := messagePlan{
			providerID: providerID,
			historyID:  msg.HistoryID,
			folderIDs:  gmailAPIProviderSyncFolderIDs(messageUpserts),
			skipped:    len(messageUpserts) == 0,
		}
		plans = append(plans, plan)
		upserts = append(upserts, messageUpserts...)
	}

	idsByProvider, err := o.db.UpsertProviderSyncMessages(ctx, upserts)
	if err != nil {
		return nil, err
	}
	results := make([]gmailAPIMessageSyncResult, 0, len(plans))
	for _, plan := range plans {
		if plan.skipped {
			results = append(results, gmailAPIMessageSyncResult{ProviderMessageID: plan.providerID, HistoryID: plan.historyID, Skipped: true})
			continue
		}
		localID := idsByProvider[plan.providerID]
		if localID == 0 {
			results = append(results, gmailAPIMessageSyncResult{ProviderMessageID: plan.providerID, HistoryID: plan.historyID, Skipped: true})
			continue
		}
		results = append(results, gmailAPIMessageSyncResult{
			ProviderMessageID: plan.providerID,
			LocalMessageID:    localID,
			HistoryID:         plan.historyID,
			FolderIDs:         plan.folderIDs,
			Synced:            true,
		})
	}
	return results, nil
}

func (o *SyncOrchestrator) fetchGmailAPIMessageMetadataBatch(ctx context.Context, accountID string, token *gmailAPISharedToken, limiter *gmailAPIMetadataLimiter, providerIDs []string) []gmailAPIMessageFetchResult {
	providerIDs = compactGmailAPIProviderIDs(providerIDs)
	if len(providerIDs) == 0 {
		return nil
	}
	workers := gmailAPIHistoricalMetadataWorkers
	if workers > len(providerIDs) {
		workers = len(providerIDs)
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan string)
	results := make(chan gmailAPIMessageFetchResult, len(providerIDs))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for providerID := range jobs {
				msg, err := o.getGmailAPIMessageMetadataWithRetry(ctx, accountID, token, limiter, providerID)
				results <- gmailAPIMessageFetchResult{ProviderMessageID: providerID, Message: msg, Err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, providerID := range providerIDs {
			select {
			case <-ctx.Done():
				return
			case jobs <- providerID:
			}
		}
	}()
	wg.Wait()
	close(results)

	out := make([]gmailAPIMessageFetchResult, 0, len(providerIDs))
	for result := range results {
		if result.Err == nil || result.ProviderMessageID != "" {
			out = append(out, result)
		}
	}
	return out
}

func (o *SyncOrchestrator) getGmailAPIMessageMetadataWithRetry(ctx context.Context, accountID string, token *gmailAPISharedToken, limiter *gmailAPIMetadataLimiter, providerMessageID string) (gmailAPIMessage, error) {
	var lastErr error
	for attempt := 0; attempt < gmailAPIMessageMetadataMaxAttempts; attempt++ {
		if err := limiter.Wait(ctx); err != nil {
			return gmailAPIMessage{}, err
		}
		currentToken := token.Get()
		msg, err := getGmailAPIMessageMetadata(ctx, currentToken, providerMessageID)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		if providerAPIUnauthorized(err) {
			refreshed, refreshErr := token.RefreshIfCurrent(ctx, o, accountID, currentToken)
			if refreshErr != nil {
				return gmailAPIMessage{}, err
			}
			if refreshed != "" {
				continue
			}
			return gmailAPIMessage{}, err
		}
		if !gmailAPIShouldRetryMessageMetadata(err) {
			return gmailAPIMessage{}, err
		}
		if attempt == gmailAPIMessageMetadataMaxAttempts-1 {
			break
		}
		if err := gmailAPIWaitBeforeRetry(ctx, attempt); err != nil {
			return gmailAPIMessage{}, err
		}
	}
	if lastErr != nil {
		return gmailAPIMessage{}, lastErr
	}
	return gmailAPIMessage{}, context.Canceled
}

func newGmailAPISharedToken(token *string) *gmailAPISharedToken {
	shared := &gmailAPISharedToken{external: token}
	if token != nil {
		shared.token = *token
	}
	return shared
}

func (t *gmailAPISharedToken) Get() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.external != nil && strings.TrimSpace(*t.external) != "" && *t.external != t.token {
		t.token = *t.external
	}
	return t.token
}

func (t *gmailAPISharedToken) RefreshIfCurrent(ctx context.Context, o *SyncOrchestrator, accountID, failedToken string) (string, error) {
	if t == nil {
		return "", fmt.Errorf("gmail api token unavailable")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && failedToken != "" && t.token != failedToken {
		return t.token, nil
	}
	refreshed, err := o.refreshOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	t.token = refreshed
	if t.external != nil {
		*t.external = refreshed
	}
	return refreshed, nil
}

func newGmailAPIMetadataLimiter(perMinute, burst int) *gmailAPIMetadataLimiter {
	if perMinute <= 0 {
		perMinute = gmailAPIHistoricalMetadataPerMinute
	}
	if burst <= 0 {
		burst = 1
	}
	tokens := make(chan struct{}, burst)
	for i := 0; i < burst; i++ {
		tokens <- struct{}{}
	}
	interval := time.Minute / time.Duration(perMinute)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}()
	return &gmailAPIMetadataLimiter{
		tokens: tokens,
		stop: func() {
			close(done)
		},
	}
}

func (l *gmailAPIMetadataLimiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.tokens:
		return nil
	}
}

func (l *gmailAPIMetadataLimiter) Stop() {
	if l != nil && l.stop != nil {
		l.once.Do(l.stop)
	}
}

func gmailAPIShouldRetryMessageMetadata(err error) bool {
	status, ok := providerAPIStatus(err)
	if !ok {
		return false
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	body := strings.ToLower(providerAPIErrorBody(err))
	return strings.Contains(body, "ratelimit") ||
		strings.Contains(body, "rate limit") ||
		strings.Contains(body, "userratelimit") ||
		strings.Contains(body, "quota")
}

func providerAPIErrorBody(err error) string {
	var apiErr *providerAPIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.Body
	}
	return ""
}

func gmailAPIWaitBeforeRetry(ctx context.Context, attempt int) error {
	delay := 500 * time.Millisecond
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= 30*time.Second {
			delay = 30 * time.Second
			break
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func gmailAPIProviderIDChunks(providerIDs []string, size int) [][]string {
	if size <= 0 {
		size = gmailAPIHistoricalMetadataBatchSize
	}
	providerIDs = compactGmailAPIProviderIDs(providerIDs)
	if len(providerIDs) == 0 {
		return nil
	}
	chunks := make([][]string, 0, (len(providerIDs)+size-1)/size)
	for len(providerIDs) > 0 {
		n := size
		if n > len(providerIDs) {
			n = len(providerIDs)
		}
		chunks = append(chunks, providerIDs[:n])
		providerIDs = providerIDs[n:]
	}
	return chunks
}

func compactGmailAPIProviderIDs(providerIDs []string) []string {
	if len(providerIDs) == 0 {
		return nil
	}
	out := make([]string, 0, len(providerIDs))
	seen := map[string]bool{}
	for _, providerID := range providerIDs {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" || seen[providerID] {
			continue
		}
		seen[providerID] = true
		out = append(out, providerID)
	}
	return out
}

func recordGmailAPITouchedFolders(touched map[string]bool, folderIDs []string) {
	for _, folderID := range folderIDs {
		folderID = strings.TrimSpace(folderID)
		if folderID != "" {
			touched[folderID] = true
		}
	}
}

func (o *SyncOrchestrator) publishGmailAPIFolderSyncEvent(ctx context.Context, eventType EventType, accountID string, touched map[string]bool, targetsByFolderID map[string]gmailAPIFolderSyncTarget, current, total int, clearTouched, totalEstimated bool) {
	if len(touched) == 0 {
		return
	}
	folderIDs := make([]string, 0, len(touched))
	for folderID := range touched {
		folderIDs = append(folderIDs, folderID)
	}
	sort.Strings(folderIDs)
	for _, folderID := range folderIDs {
		if err := o.db.RefreshFolderThreadState(ctx, folderID); err != nil {
			log.Printf("gmail api refresh folder thread state account=%s folder=%s: %v", accountID, folderID, err)
			continue
		}
		folderRole, _ := o.db.GetFolderRole(ctx, folderID)
		folderName := displayName(folderID, folderRole)
		if target, ok := targetsByFolderID[folderID]; ok {
			folderRole = target.Folder.Role
			folderName = displayName(target.Folder.RemoteID, target.Folder.Role)
		}
		o.events.Publish(Event{Type: eventType, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Current: current, Total: total, Payload: o.gmailAPIFolderRefreshPayload(ctx, accountID, folderName, totalEstimated)})
	}
	if clearTouched {
		clear(touched)
	}
}

func (o *SyncOrchestrator) gmailAPIFolderRefreshPayload(ctx context.Context, accountID, folderName string, totalEstimated bool) map[string]any {
	return o.folderSyncProgressPayload(ctx, accountID, folderName, "gmail_api", map[string]any{
		"provider":        "gmail_api",
		"refresh_only":    true,
		"total_estimated": totalEstimated,
	})
}

func gmailAPIProviderSyncFolderIDs(upserts []storage.ProviderSyncMessage) []string {
	seen := map[string]bool{}
	folderIDs := make([]string, 0, len(upserts))
	for _, upsert := range upserts {
		folderID := strings.TrimSpace(upsert.FolderID)
		if folderID == "" || seen[folderID] {
			continue
		}
		seen[folderID] = true
		folderIDs = append(folderIDs, folderID)
	}
	sort.Strings(folderIDs)
	return folderIDs
}

func gmailAPIMessageRefIDs(refs []gmailAPIMessageRef) []string {
	ids := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		providerID := strings.TrimSpace(ref.ID)
		if providerID == "" || seen[providerID] {
			continue
		}
		seen[providerID] = true
		ids = append(ids, providerID)
	}
	return ids
}

func gmailAPIProviderIDSetValues(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for providerID := range set {
		providerID = strings.TrimSpace(providerID)
		if providerID != "" {
			ids = append(ids, providerID)
		}
	}
	sort.Strings(ids)
	return ids
}

func listGmailAPIMessageIDPage(ctx context.Context, token, labelID, pageToken string) (gmailAPIMessageListResponse, error) {
	values := url.Values{}
	values.Set("maxResults", strconv.Itoa(gmailAPIMessagePageSize))
	values.Set("includeSpamTrash", "true")
	if labelID == "ARCHIVE" {
		values.Set("q", "-in:inbox -in:sent -in:drafts -in:trash -in:spam")
	} else if strings.TrimSpace(labelID) != "" {
		values.Add("labelIds", strings.TrimSpace(labelID))
	}
	if pageToken != "" {
		values.Set("pageToken", pageToken)
	}
	var response gmailAPIMessageListResponse
	endpoint := gmailAPIBaseURL + "/users/me/messages?" + values.Encode()
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, nil, nil, &response); err != nil {
		return gmailAPIMessageListResponse{}, err
	}
	return response, nil
}

func (o *SyncOrchestrator) listGmailAPIMessageIDPageWithAuthRetry(ctx context.Context, accountID string, token *string, labelID, pageToken string) (gmailAPIMessageListResponse, error) {
	current := ""
	if token != nil {
		current = *token
	}
	page, err := listGmailAPIMessageIDPage(ctx, current, labelID, pageToken)
	refreshed, ok := o.refreshGmailTokenAfterUnauthorized(ctx, accountID, token, err)
	if !ok {
		return page, err
	}
	return listGmailAPIMessageIDPage(ctx, refreshed, labelID, pageToken)
}

func getGmailAPIMessageMetadata(ctx context.Context, token, providerMessageID string) (gmailAPIMessage, error) {
	values := url.Values{}
	values.Set("format", "metadata")
	for _, header := range gmailAPIMessageMetadataHeaders {
		values.Add("metadataHeaders", header)
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "?" + values.Encode()
	var msg gmailAPIMessage
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, nil, nil, &msg); err != nil {
		return gmailAPIMessage{}, err
	}
	return msg, nil
}

func gmailAPIMessageToProviderSyncs(accountID string, msg gmailAPIMessage, labelsByID map[string]gmailAPILabel, targetsByLabelID map[string]gmailAPIFolderSyncTarget) []storage.ProviderSyncMessage {
	providerID := strings.TrimSpace(msg.ID)
	if providerID == "" {
		return nil
	}
	labelSet := gmailAPILabelSet(msg.LabelIDs)
	header := gmailAPIHeaders(msg.Payload.Headers)
	internetMessageID := strings.TrimSpace(header["message-id"])
	if internetMessageID == "" {
		internetMessageID = "<gmail-" + providerID + "@sync.gofer>"
	}
	dateSent := gmailAPIMessageDate(msg, header)
	recipients := func(key string) []storage.Recipient {
		return gmailAPIRecipients(header[key])
	}
	isRead := !labelSet["UNREAD"]
	isStarred := labelSet["STARRED"]
	isDraft := labelSet["DRAFT"]
	base := storage.ProviderSyncMessage{
		AccountID:         accountID,
		ProviderMessageID: providerID,
		InternetMessageID: internetMessageID,
		ProviderThreadID:  strings.TrimSpace(msg.ThreadID),
		InReplyTo:         header["in-reply-to"],
		References:        header["references"],
		Subject:           strings.TrimSpace(header["subject"]),
		FromName:          gmailAPIFirstAddressName(header["from"]),
		FromEmail:         gmailAPIFirstAddressEmail(header["from"]),
		DateSent:          dateSent,
		DateReceived:      gmailAPIInternalDate(msg.InternalDate, dateSent),
		Snippet:           strings.TrimSpace(msg.Snippet),
		IsRead:            isRead,
		IsStarred:         isStarred,
		IsFlagged:         isStarred,
		IsDraft:           isDraft,
		HasAttachments:    len(gmailAPIAttachmentRows(msg)) > 0,
		Labels:            gmailLabelInputs(accountID, msg.LabelIDs, gmailLabelsForInputs(labelsByID)),
		LabelsKnown:       true,
		LabelProvider:     storage.LabelProviderGmail,
		ToRecipients:      recipients("to"),
		CCRecipients:      recipients("cc"),
		BCCRecipients:     recipients("bcc"),
	}
	if base.Subject == "" {
		base.Subject = "(no subject)"
	}

	var out []storage.ProviderSyncMessage
	addedFolders := map[string]bool{}
	addForLabel := func(labelID string) {
		target, ok := targetsByLabelID[labelID]
		if !ok || strings.TrimSpace(target.Folder.ID) == "" {
			return
		}
		if addedFolders[target.Folder.ID] {
			return
		}
		addedFolders[target.Folder.ID] = true
		copy := base
		copy.FolderID = target.Folder.ID
		out = append(out, copy)
	}
	for _, labelID := range msg.LabelIDs {
		addForLabel(strings.ToUpper(strings.TrimSpace(labelID)))
		addForLabel(strings.TrimSpace(labelID))
	}
	if gmailAPIMessageIsArchived(labelSet) {
		addForLabel("ARCHIVE")
	}
	return out
}

func gmailLabelsForInputs(labelsByID map[string]gmailAPILabel) map[string]gmailLabel {
	out := make(map[string]gmailLabel, len(labelsByID))
	for id, label := range labelsByID {
		out[id] = gmailLabel{ID: label.ID, Name: label.Name, Type: label.Type}
	}
	return out
}

func gmailAPIAttachmentRows(msg gmailAPIMessage) []storage.AttachmentRow {
	var rows []storage.AttachmentRow
	var walk func(part gmailAPIPart)
	walk = func(part gmailAPIPart) {
		attachmentID := strings.TrimSpace(part.Body.AttachmentID)
		filename := strings.TrimSpace(part.Filename)
		if attachmentID != "" || filename != "" {
			contentID := strings.Trim(gmailAPIHeaderValue(part.Headers, "Content-ID"), "<>")
			contentDisposition := strings.ToLower(gmailAPIHeaderValue(part.Headers, "Content-Disposition"))
			inline := contentID != "" || strings.Contains(contentDisposition, "inline")
			if filename == "" {
				filename = contentID
			}
			if filename == "" {
				filename = attachmentID
			}
			rows = append(rows, storage.AttachmentRow{
				Filename:         filename,
				ContentType:      gmailAPIPartContentType(part),
				SizeBytes:        part.Body.Size,
				ContentID:        contentID,
				Inline:           inline,
				ProviderRemoteID: attachmentID,
			})
		}
		for _, child := range part.Parts {
			walk(child)
		}
	}
	walk(msg.Payload)
	return rows
}

func gmailAPILabelSelectable(label gmailAPILabel) bool {
	id := strings.ToUpper(strings.TrimSpace(label.ID))
	switch id {
	case "", "UNREAD", "CHAT":
		return false
	case "INBOX", "SENT", "DRAFT", "TRASH", "SPAM":
		return true
	default:
		return false
	}
}

func providerRemoteIDsFromFolderInputs(inputs []storage.UpsertFolderInput) []string {
	ids := make([]string, 0, len(inputs))
	seen := map[string]bool{}
	for _, input := range inputs {
		id := strings.TrimSpace(input.ProviderRemoteID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func gmailAPILabelRole(label gmailAPILabel) string {
	switch strings.ToUpper(strings.TrimSpace(label.ID)) {
	case "INBOX":
		return "inbox"
	case "SENT":
		return "sent"
	case "DRAFT":
		return "drafts"
	case "TRASH":
		return "trash"
	case "SPAM":
		return "junk"
	case "STARRED":
		return "starred"
	default:
		return "custom"
	}
}

func gmailAPILabelRemoteName(label gmailAPILabel) string {
	switch strings.ToUpper(strings.TrimSpace(label.ID)) {
	case "INBOX":
		return "INBOX"
	case "SENT":
		return "[Gmail]/Sent Mail"
	case "DRAFT":
		return "[Gmail]/Drafts"
	case "TRASH":
		return "[Gmail]/Trash"
	case "SPAM":
		return "[Gmail]/Spam"
	case "STARRED":
		return "[Gmail]/Starred"
	case "IMPORTANT":
		return "[Gmail]/Important"
	}
	if strings.TrimSpace(label.Name) != "" {
		return strings.TrimSpace(label.Name)
	}
	return strings.TrimSpace(label.ID)
}

func gmailAPIFolderSortOrder(role string, fallback int) int {
	switch role {
	case "inbox":
		return 0
	case "starred":
		return 1
	case "sent":
		return 2
	case "drafts":
		return 3
	case "archive":
		return 4
	case "junk":
		return 5
	case "trash":
		return 6
	default:
		return 100 + fallback
	}
}

func gmailAPITargetsByLabelID(targets []gmailAPIFolderSyncTarget) map[string]gmailAPIFolderSyncTarget {
	out := make(map[string]gmailAPIFolderSyncTarget, len(targets))
	for _, target := range targets {
		labelID := strings.TrimSpace(target.LabelID)
		if labelID == "" {
			continue
		}
		out[labelID] = target
		out[strings.ToUpper(labelID)] = target
	}
	return out
}

func gmailAPITargetsByFolderID(targets []gmailAPIFolderSyncTarget) map[string]gmailAPIFolderSyncTarget {
	out := make(map[string]gmailAPIFolderSyncTarget, len(targets))
	for _, target := range targets {
		folderID := strings.TrimSpace(target.Folder.ID)
		if folderID == "" {
			continue
		}
		out[folderID] = target
	}
	return out
}

func gmailAPILabelSet(labelIDs []string) map[string]bool {
	out := make(map[string]bool, len(labelIDs))
	for _, labelID := range labelIDs {
		if value := strings.TrimSpace(labelID); value != "" {
			out[value] = true
			out[strings.ToUpper(value)] = true
		}
	}
	return out
}

func gmailAPIMessageIsArchived(labelSet map[string]bool) bool {
	return !labelSet["INBOX"] &&
		!labelSet["SENT"] &&
		!labelSet["DRAFT"] &&
		!labelSet["TRASH"] &&
		!labelSet["SPAM"]
}

func gmailAPIHeaders(headers []gmailAPIHeader) map[string]string {
	out := make(map[string]string, len(headers))
	for _, header := range headers {
		name := strings.ToLower(strings.TrimSpace(header.Name))
		if name == "" {
			continue
		}
		out[name] = strings.TrimSpace(header.Value)
	}
	return out
}

func gmailAPIHeaderValue(headers []gmailAPIHeader, name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, header := range headers {
		if strings.ToLower(strings.TrimSpace(header.Name)) == name {
			return strings.TrimSpace(header.Value)
		}
	}
	return ""
}

func gmailAPIPartContentType(part gmailAPIPart) string {
	if value := strings.TrimSpace(gmailAPIHeaderValue(part.Headers, "Content-Type")); value != "" {
		if mediaType, _, err := mime.ParseMediaType(value); err == nil && strings.TrimSpace(mediaType) != "" {
			return strings.TrimSpace(mediaType)
		}
	}
	if strings.TrimSpace(part.MimeType) != "" {
		return strings.TrimSpace(part.MimeType)
	}
	return "application/octet-stream"
}

func gmailAPIRecipients(value string) []storage.Recipient {
	addresses, err := mail.ParseAddressList(value)
	if err != nil {
		return nil
	}
	out := make([]storage.Recipient, 0, len(addresses))
	for _, addr := range addresses {
		if strings.TrimSpace(addr.Address) == "" {
			continue
		}
		out = append(out, storage.Recipient{Name: message.DecodeHeader(addr.Name), Email: strings.TrimSpace(addr.Address)})
	}
	return out
}

func gmailAPIFirstAddressName(value string) string {
	recipients := gmailAPIRecipients(value)
	if len(recipients) == 0 {
		return ""
	}
	return recipients[0].Name
}

func gmailAPIFirstAddressEmail(value string) string {
	recipients := gmailAPIRecipients(value)
	if len(recipients) == 0 {
		return ""
	}
	return recipients[0].Email
}

func gmailAPIMessageDate(msg gmailAPIMessage, headers map[string]string) time.Time {
	if value := strings.TrimSpace(headers["date"]); value != "" {
		if parsed, err := mail.ParseDate(value); err == nil {
			return parsed
		}
	}
	return gmailAPIInternalDate(msg.InternalDate, time.Now().UTC())
}

func gmailAPIInternalDate(value string, fallback time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC()
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback.UTC()
	}
	return time.UnixMilli(ms).UTC()
}
