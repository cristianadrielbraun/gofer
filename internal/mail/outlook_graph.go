package mail

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	netmail "net/mail"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const (
	outlookGraphMessagePageSize = 250
)

type outlookGraphFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	ChildFolderCount int    `json:"childFolderCount"`
	TotalItemCount   int    `json:"totalItemCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	IsHidden         bool   `json:"isHidden"`
	Role             string `json:"-"`
	DisplayPath      string `json:"-"`
}

type outlookGraphFoldersResponse struct {
	Value    []outlookGraphFolder `json:"value"`
	NextLink string               `json:"@odata.nextLink"`
}

type outlookGraphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type outlookGraphRecipient struct {
	EmailAddress outlookGraphEmailAddress `json:"emailAddress"`
}

type outlookGraphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type outlookGraphFlag struct {
	FlagStatus string `json:"flagStatus"`
}

type outlookGraphHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type outlookGraphRemoved struct {
	Reason string `json:"reason"`
}

type outlookGraphMessage struct {
	ID                     string                  `json:"id"`
	InternetMessageID      string                  `json:"internetMessageId"`
	ConversationID         string                  `json:"conversationId"`
	ParentFolderID         string                  `json:"parentFolderId"`
	Subject                string                  `json:"subject"`
	BodyPreview            string                  `json:"bodyPreview"`
	Body                   outlookGraphBody        `json:"body"`
	Categories             []string                `json:"categories"`
	From                   outlookGraphRecipient   `json:"from"`
	Sender                 outlookGraphRecipient   `json:"sender"`
	ToRecipients           []outlookGraphRecipient `json:"toRecipients"`
	CCRecipients           []outlookGraphRecipient `json:"ccRecipients"`
	BCCRecipients          []outlookGraphRecipient `json:"bccRecipients"`
	ReceivedDateTime       time.Time               `json:"receivedDateTime"`
	SentDateTime           time.Time               `json:"sentDateTime"`
	IsRead                 bool                    `json:"isRead"`
	IsDraft                bool                    `json:"isDraft"`
	HasAttachments         bool                    `json:"hasAttachments"`
	Flag                   outlookGraphFlag        `json:"flag"`
	InternetMessageHeaders []outlookGraphHeader    `json:"internetMessageHeaders"`
	Removed                *outlookGraphRemoved    `json:"@removed,omitempty"`
}

type outlookGraphMessagesDeltaResponse struct {
	Value     []outlookGraphMessage `json:"value"`
	NextLink  string                `json:"@odata.nextLink"`
	DeltaLink string                `json:"@odata.deltaLink"`
}

type outlookGraphAttachment struct {
	ODataType   string `json:"@odata.type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"isInline"`
	ContentID   string `json:"contentId"`
}

type outlookGraphAttachmentsResponse struct {
	Value    []outlookGraphAttachment `json:"value"`
	NextLink string                   `json:"@odata.nextLink"`
}

type outlookGraphFolderSyncTarget struct {
	Folder storage.FolderSyncInfo
	Graph  outlookGraphFolder
}

func (o *SyncOrchestrator) shouldUseOutlookGraphMail(cfg *models.AccountConfig) bool {
	return cfg != nil && strings.TrimSpace(cfg.Provider) == providers.ProviderOutlook
}

func (o *SyncOrchestrator) syncOutlookGraphAccount(ctx context.Context, accountID string, includeIDLEFolders bool) error {
	graphTokens, ok := o.tokenProvider.(graphMailTokenProvider)
	if !ok {
		return fmt.Errorf("outlook graph mail token provider is unavailable")
	}
	token, err := graphTokens.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}

	targets, err := o.syncOutlookGraphFolders(ctx, accountID, token)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: accountSyncProgressPayload(ctx, accountSyncBackground, map[string]any{
			"status":                "ok",
			"account_folders_total": 0,
			"provider":              "graph",
		})})
		return nil
	}

	o.backfillOutlookGraphMessageIDs(ctx, accountID, token, 250)

	categoriesByName, err := o.syncOutlookCategoryCatalog(ctx, accountID, token)
	if err != nil {
		log.Printf("outlook graph sync %s: category catalog: %v", accountID, err)
		categoriesByName = map[string]outlookCategory{}
	}

	var firstFolderErr error
	failedFolders := 0
	for i, target := range targets {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := o.syncOutlookGraphFolder(ctx, accountID, token, target, categoriesByName, i+1, len(targets)); err != nil {
			log.Printf("outlook graph sync folder %s/%s: %v", accountID, target.Graph.DisplayName, err)
			failedFolders++
			if firstFolderErr == nil {
				firstFolderErr = err
			}
			_, _ = o.db.Write().ExecContext(ctx,
				`UPDATE folders SET sync_error = ? WHERE id = ?`, err.Error(), target.Folder.ID)
		}
	}

	o.replayOutlookLabelMutationQueue(ctx, accountID, token)
	if failedFolders > 0 {
		return fmt.Errorf("%d Outlook Graph folder sync(s) failed: %w", failedFolders, firstFolderErr)
	}
	return nil
}

func (o *SyncOrchestrator) backfillOutlookGraphMessageIDs(ctx context.Context, accountID, token string, limit int) {
	if o.db == nil {
		return
	}
	candidates, err := o.db.ListProviderMessageIDBackfillCandidates(ctx, accountID, limit)
	if err != nil {
		log.Printf("outlook graph id backfill %s: list candidates: %v", accountID, err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	var resolved, missing, failed int
	for _, candidate := range candidates {
		select {
		case <-ctx.Done():
			log.Printf("outlook graph id backfill %s: context canceled after %d resolved, %d missing, %d failed", accountID, resolved, missing, failed)
			return
		default:
		}
		providerID, err := resolveOutlookGraphMessageIDByInternetMessageID(ctx, token, candidate.InternetMessageID)
		if err != nil {
			failed++
			log.Printf("outlook graph id backfill %s message=%d: %v", accountID, candidate.MessageID, err)
			continue
		}
		if strings.TrimSpace(providerID) == "" {
			missing++
			continue
		}
		if err := o.db.SetMessageProviderMessageID(ctx, candidate.MessageID, providerID); err != nil {
			failed++
			log.Printf("outlook graph id backfill %s message=%d cache: %v", accountID, candidate.MessageID, err)
			continue
		}
		resolved++
	}
	log.Printf("outlook graph id backfill %s: %d resolved, %d missing, %d failed", accountID, resolved, missing, failed)
}

func resolveOutlookGraphMessageIDByInternetMessageID(ctx context.Context, token, internetMessageID string) (string, error) {
	internetMessageID = strings.TrimSpace(internetMessageID)
	if internetMessageID == "" {
		return "", nil
	}
	values := url.Values{}
	values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetMessageID, "'", "''")+"'")
	values.Set("$select", "id,internetMessageId,parentFolderId")
	values.Set("$top", "1")
	var response struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	endpoint := outlookGraphBaseURL + "/me/messages?" + values.Encode()
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookGraphHeaders(0), nil, &response); err != nil {
		return "", err
	}
	if len(response.Value) == 0 {
		return "", nil
	}
	return strings.TrimSpace(response.Value[0].ID), nil
}

func (o *SyncOrchestrator) syncOutlookGraphFolders(ctx context.Context, accountID, token string) ([]outlookGraphFolderSyncTarget, error) {
	folders, err := listOutlookGraphFolders(ctx, token)
	if err != nil {
		return nil, err
	}
	assignOutlookGraphFolderPaths(folders)
	sortOutlookGraphFolders(folders)

	inputs := make([]storage.UpsertFolderInput, 0, len(folders))
	for i, folder := range folders {
		if strings.TrimSpace(folder.ID) == "" || folder.IsHidden {
			continue
		}
		localID := outlookGraphLocalFolderID(accountID, folder)
		parentID := ""
		if parent := outlookGraphParentFolder(folders, folder.ParentFolderID); parent != nil {
			parentID = outlookGraphLocalFolderID(accountID, *parent)
		}
		role := outlookGraphFolderRole(folder)
		order := outlookGraphFolderSortOrder(role, i)
		remoteName := outlookGraphFolderRemoteName(folder)
		inputs = append(inputs, storage.UpsertFolderInput{
			ID:               localID,
			AccountID:        accountID,
			ParentID:         parentID,
			RemoteID:         remoteName,
			ProviderRemoteID: folder.ID,
			Name:             displayName(remoteName, role),
			Icon:             imap.RoleIcon(role),
			Role:             role,
			Selectable:       true,
			SortOrder:        order,
			CountsKnown:      true,
			TotalCount:       graphFolderCount(folder.TotalItemCount),
			UnreadCount:      graphFolderCount(folder.UnreadItemCount),
		})
	}
	if len(inputs) > 0 {
		if err := o.db.UpsertFolders(ctx, inputs); err != nil {
			return nil, err
		}
	}

	localFolders, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	graphByID := make(map[string]outlookGraphFolder, len(folders))
	for _, folder := range folders {
		if strings.TrimSpace(folder.ID) != "" {
			graphByID[folder.ID] = folder
		}
	}
	targets := make([]outlookGraphFolderSyncTarget, 0, len(localFolders))
	for _, folder := range localFolders {
		if strings.TrimSpace(folder.ProviderRemoteID) == "" {
			continue
		}
		graphFolder, ok := graphByID[folder.ProviderRemoteID]
		if !ok {
			continue
		}
		targets = append(targets, outlookGraphFolderSyncTarget{Folder: folder, Graph: graphFolder})
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return outlookGraphFolderSortOrder(targets[i].Folder.Role, i) < outlookGraphFolderSortOrder(targets[j].Folder.Role, j)
	})
	return targets, nil
}

func listOutlookGraphFolders(ctx context.Context, token string) ([]outlookGraphFolder, error) {
	foldersByID := map[string]outlookGraphFolder{}
	queue := []string{outlookGraphMailFoldersEndpoint()}
	for len(queue) > 0 {
		endpoint := queue[0]
		queue = queue[1:]
		for endpoint != "" {
			var response outlookGraphFoldersResponse
			if err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookGraphHeaders(0), nil, &response); err != nil {
				return nil, err
			}
			for _, folder := range response.Value {
				folder.ID = strings.TrimSpace(folder.ID)
				folder.DisplayName = strings.TrimSpace(folder.DisplayName)
				if folder.ID == "" || folder.DisplayName == "" {
					continue
				}
				if existing, ok := foldersByID[folder.ID]; ok && existing.Role != "" {
					folder.Role = existing.Role
				}
				foldersByID[folder.ID] = folder
				if folder.ChildFolderCount > 0 {
					queue = append(queue, outlookGraphChildFoldersEndpoint(folder.ID))
				}
			}
			endpoint = strings.TrimSpace(response.NextLink)
		}
	}

	for wellKnown, role := range outlookGraphWellKnownFolderRoles() {
		var folder outlookGraphFolder
		err := providerJSON(ctx, http.MethodGet, outlookGraphWellKnownFolderEndpoint(wellKnown), token, outlookGraphHeaders(0), nil, &folder)
		if err != nil {
			if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		folder.ID = strings.TrimSpace(folder.ID)
		if folder.ID == "" {
			continue
		}
		folder.Role = role
		if existing, ok := foldersByID[folder.ID]; ok {
			if strings.TrimSpace(folder.DisplayName) == "" {
				folder.DisplayName = existing.DisplayName
			}
			if strings.TrimSpace(folder.ParentFolderID) == "" {
				folder.ParentFolderID = existing.ParentFolderID
			}
			if folder.ChildFolderCount == 0 {
				folder.ChildFolderCount = existing.ChildFolderCount
			}
			if folder.TotalItemCount == 0 {
				folder.TotalItemCount = existing.TotalItemCount
			}
			if folder.UnreadItemCount == 0 {
				folder.UnreadItemCount = existing.UnreadItemCount
			}
			folder.IsHidden = existing.IsHidden
		}
		foldersByID[folder.ID] = folder
	}

	folders := make([]outlookGraphFolder, 0, len(foldersByID))
	for _, folder := range foldersByID {
		folders = append(folders, folder)
	}
	return folders, nil
}

func outlookGraphMailFoldersEndpoint() string {
	values := url.Values{}
	values.Set("$select", "id,displayName,parentFolderId,childFolderCount,totalItemCount,unreadItemCount,isHidden")
	values.Set("$top", "100")
	return outlookGraphBaseURL + "/me/mailFolders?" + values.Encode()
}

func outlookGraphChildFoldersEndpoint(folderID string) string {
	values := url.Values{}
	values.Set("$select", "id,displayName,parentFolderId,childFolderCount,totalItemCount,unreadItemCount,isHidden")
	values.Set("$top", "100")
	return outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folderID) + "/childFolders?" + values.Encode()
}

func outlookGraphWellKnownFolderEndpoint(wellKnown string) string {
	values := url.Values{}
	values.Set("$select", "id,displayName,parentFolderId,childFolderCount,totalItemCount,unreadItemCount,isHidden")
	return outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(wellKnown) + "?" + values.Encode()
}

func outlookGraphFolderEndpoint(folderID string) string {
	values := url.Values{}
	values.Set("$select", "id,displayName,parentFolderId,childFolderCount,totalItemCount,unreadItemCount,isHidden")
	return outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folderID) + "?" + values.Encode()
}

func outlookGraphMessagesDeltaEndpoint(folderID string, includeBody bool) string {
	values := url.Values{}
	values.Set("$select", outlookGraphMessageSelect(includeBody))
	return outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folderID) + "/messages/delta?" + values.Encode()
}

func outlookGraphMessageEndpoint(providerMessageID string, includeBody bool) string {
	values := url.Values{}
	values.Set("$select", outlookGraphMessageSelect(includeBody))
	return outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "?" + values.Encode()
}

func outlookGraphMessageSelect(includeBody bool) string {
	fields := []string{
		"id",
		"internetMessageId",
		"conversationId",
		"parentFolderId",
		"subject",
		"from",
		"sender",
		"toRecipients",
		"ccRecipients",
		"bccRecipients",
		"receivedDateTime",
		"sentDateTime",
		"bodyPreview",
		"categories",
		"isRead",
		"isDraft",
		"flag",
		"hasAttachments",
		"internetMessageHeaders",
	}
	if includeBody {
		fields = append(fields, "body")
	}
	return strings.Join(fields, ",")
}

func outlookGraphHeaders(pageSize int) map[string]string {
	headers := outlookImmutableIDHeaders()
	if pageSize > 0 {
		headers["Prefer"] = fmt.Sprintf(`%s, odata.maxpagesize=%d`, headers["Prefer"], pageSize)
	}
	return headers
}

func outlookGraphWellKnownFolderRoles() map[string]string {
	return map[string]string{
		"inbox":        "inbox",
		"sentitems":    "sent",
		"drafts":       "drafts",
		"deleteditems": "trash",
		"junkemail":    "junk",
		"archive":      "archive",
	}
}

func syncOutlookGraphFolderRoleFromDisplayName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "inbox":
		return "inbox"
	case "sent", "sent items":
		return "sent"
	case "drafts":
		return "drafts"
	case "deleted items", "trash":
		return "trash"
	case "junk email", "junk", "spam":
		return "junk"
	case "archive", "archives":
		return "archive"
	default:
		return "custom"
	}
}

func outlookGraphFolderRole(folder outlookGraphFolder) string {
	if strings.TrimSpace(folder.Role) != "" {
		return strings.TrimSpace(folder.Role)
	}
	return syncOutlookGraphFolderRoleFromDisplayName(folder.DisplayName)
}

func outlookGraphFolderRemoteName(folder outlookGraphFolder) string {
	switch outlookGraphFolderRole(folder) {
	case "inbox":
		return "Inbox"
	case "sent":
		return "Sent Items"
	case "drafts":
		return "Drafts"
	case "trash":
		return "Deleted Items"
	case "junk":
		return "Junk Email"
	case "archive":
		return "Archive"
	}
	if strings.TrimSpace(folder.DisplayPath) != "" {
		return strings.TrimSpace(folder.DisplayPath)
	}
	return strings.TrimSpace(folder.DisplayName)
}

func outlookGraphLocalFolderID(accountID string, folder outlookGraphFolder) string {
	return folderIDFromRemote(accountID, outlookGraphFolderRemoteName(folder))
}

func outlookGraphFolderSortOrder(role string, fallback int) int {
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

func graphFolderCount(count int) int {
	if count < 0 {
		return 0
	}
	return count
}

func fetchOutlookGraphFolder(ctx context.Context, token, folderID string) (outlookGraphFolder, error) {
	var folder outlookGraphFolder
	if err := providerJSON(ctx, http.MethodGet, outlookGraphFolderEndpoint(folderID), token, outlookGraphHeaders(0), nil, &folder); err != nil {
		return outlookGraphFolder{}, err
	}
	folder.ID = strings.TrimSpace(folder.ID)
	folder.DisplayName = strings.TrimSpace(folder.DisplayName)
	return folder, nil
}

func outlookGraphParentFolder(folders []outlookGraphFolder, parentID string) *outlookGraphFolder {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil
	}
	for i := range folders {
		if folders[i].ID == parentID {
			return &folders[i]
		}
	}
	return nil
}

func assignOutlookGraphFolderPaths(folders []outlookGraphFolder) {
	byID := make(map[string]*outlookGraphFolder, len(folders))
	for i := range folders {
		byID[folders[i].ID] = &folders[i]
	}
	var pathFor func(*outlookGraphFolder, map[string]bool) string
	pathFor = func(folder *outlookGraphFolder, seen map[string]bool) string {
		if folder == nil {
			return ""
		}
		if strings.TrimSpace(folder.DisplayPath) != "" {
			return folder.DisplayPath
		}
		name := strings.TrimSpace(folder.DisplayName)
		if name == "" {
			name = strings.TrimSpace(folder.ID)
		}
		if folder.ParentFolderID == "" || seen[folder.ID] {
			folder.DisplayPath = name
			return folder.DisplayPath
		}
		seen[folder.ID] = true
		parentPath := pathFor(byID[folder.ParentFolderID], seen)
		if parentPath == "" {
			folder.DisplayPath = name
		} else {
			folder.DisplayPath = parentPath + "/" + name
		}
		return folder.DisplayPath
	}
	for i := range folders {
		pathFor(&folders[i], map[string]bool{})
	}
}

func sortOutlookGraphFolders(folders []outlookGraphFolder) {
	sort.SliceStable(folders, func(i, j int) bool {
		leftRole := outlookGraphFolderRole(folders[i])
		rightRole := outlookGraphFolderRole(folders[j])
		leftOrder := outlookGraphFolderSortOrder(leftRole, i)
		rightOrder := outlookGraphFolderSortOrder(rightRole, j)
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return strings.ToLower(outlookGraphFolderRemoteName(folders[i])) < strings.ToLower(outlookGraphFolderRemoteName(folders[j]))
	})
}

func (o *SyncOrchestrator) syncOutlookGraphFolder(ctx context.Context, accountID, token string, target outlookGraphFolderSyncTarget, categoriesByName map[string]outlookCategory, folderIndex, folderTotal int) (syncErr error) {
	folder := target.Folder
	graphFolder := target.Graph
	full := outlookGraphFolderNeedsFullReconcile(folder)
	if !full && o.db != nil {
		missingHeaders, err := o.db.HasProviderFolderMessagesMissingSender(ctx, accountID, folder.ID)
		if err != nil {
			return fmt.Errorf("outlook graph metadata completeness check %s/%s: %w", accountID, graphFolder.DisplayName, err)
		}
		if missingHeaders {
			log.Printf("outlook graph metadata incomplete for %s/%s, restarting full baseline", accountID, graphFolder.DisplayName)
			full = true
		}
	}
	endpoint := strings.TrimSpace(folder.SyncCursor)
	resumingFull := full && outlookGraphDeltaNextLink(endpoint)
	fullStartedFromBaseline := full && !resumingFull
	if full {
		if !resumingFull {
			endpoint = outlookGraphMessagesDeltaEndpoint(graphFolder.ID, false)
			if err := o.db.StartProviderFolderBaseline(context.WithoutCancel(ctx), folder.ID); err != nil {
				return err
			}
		}
	} else if endpoint == "" {
		endpoint = outlookGraphMessagesDeltaEndpoint(graphFolder.ID, true)
	}

	eventTotal := 0
	totalEstimated := true
	folderName := displayName(folder.RemoteID, folder.Role)
	startedCurrent := 0
	if resumingFull {
		startedCurrent = folder.SyncProgressCurrent
	}
	o.events.Publish(Event{Type: EventSyncStarted, AccountID: accountID, FolderID: folder.ID, FolderRole: folder.Role, Current: startedCurrent, Total: eventTotal, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, "graph", map[string]any{
		"account_folders_total": folderTotal,
		"account_folders_done":  folderIndex - 1,
		"total_estimated":       totalEstimated,
	})})
	defer func() {
		payload := map[string]any{
			"account_folders_total": folderTotal,
			"account_folders_done":  folderIndex,
			"status":                "complete",
		}
		if syncErr != nil {
			payload["status"] = "error"
			payload["error"] = syncErr.Error()
			if ctx.Err() != nil {
				payload["status"] = "cancelled"
			}
		}
		o.events.Publish(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folder.ID, FolderRole: folder.Role, Payload: o.folderSyncProgressPayload(context.WithoutCancel(ctx), accountID, folderName, "graph", payload)})
	}()

	fetched := startedCurrent
	seenProviderIDs := map[string]bool{}
	var postPageErr error
	for endpoint != "" {
		var response outlookGraphMessagesDeltaResponse
		err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookGraphHeaders(outlookGraphMessagePageSize), nil, &response)
		if err != nil {
			if !full {
				if status, ok := providerAPIStatus(err); ok && status == http.StatusGone {
					log.Printf("outlook graph delta cursor expired for %s/%s, restarting full delta", accountID, graphFolder.DisplayName)
					full = true
					fullStartedFromBaseline = true
					seenProviderIDs = map[string]bool{}
					fetched = 0
					endpoint = outlookGraphMessagesDeltaEndpoint(graphFolder.ID, false)
					if startErr := o.db.StartProviderFolderBaseline(context.WithoutCancel(ctx), folder.ID); startErr != nil {
						return startErr
					}
					o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folder.ID, FolderRole: folder.Role, Current: 0, Total: 0, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, "graph", map[string]any{"total_estimated": true})})
					continue
				}
			}
			return err
		}

		persistCtx := context.WithoutCancel(ctx)
		upserts := make([]storage.ProviderSyncMessage, 0, len(response.Value))
		bodySources := make([]outlookGraphMessage, 0, len(response.Value))
		folderStateDirty := false
		for _, msg := range response.Value {
			msg.ID = strings.TrimSpace(msg.ID)
			if msg.ID == "" {
				continue
			}
			if msg.Removed != nil {
				if err := o.db.MarkProviderMessageRemovedFromFolder(persistCtx, accountID, folder.ID, msg.ID); err != nil {
					return fmt.Errorf("outlook graph delta remove %s/%s message=%s: %w", accountID, graphFolder.DisplayName, msg.ID, err)
				}
				folderStateDirty = true
				continue
			}
			if full {
				seenProviderIDs[msg.ID] = true
			}
			hydrated, found, err := fetchOutlookGraphMessageDetailIfNeeded(ctx, token, msg, !full)
			if err != nil {
				return fmt.Errorf("outlook graph message detail %s/%s message=%s: %w", accountID, graphFolder.DisplayName, msg.ID, err)
			}
			if !found {
				continue
			}
			msg = hydrated
			upserts = append(upserts, outlookGraphMessageToProviderSync(accountID, folder.ID, msg, categoriesByName))
			bodySources = append(bodySources, msg)
		}
		if len(upserts) > 0 {
			idsByProvider, err := o.db.UpsertProviderSyncMessages(persistCtx, upserts)
			if err != nil {
				return err
			}
			folderStateDirty = true
			if full {
				messageIDs := make([]int64, 0, len(idsByProvider))
				for _, messageID := range idsByProvider {
					messageIDs = append(messageIDs, messageID)
				}
				if err := o.db.RefreshFolderThreadsForMessages(persistCtx, folder.ID, messageIDs); err != nil {
					return fmt.Errorf("refresh outlook graph folder page %s/%s: %w", accountID, graphFolder.DisplayName, err)
				}
			} else if err := o.db.RefreshFolderThreadState(persistCtx, folder.ID); err != nil {
				return fmt.Errorf("refresh incremental outlook graph folder %s/%s: %w", accountID, graphFolder.DisplayName, err)
			}
			if !full {
				if err := o.syncOutlookGraphAttachmentMetadata(ctx, token, idsByProvider, bodySources); err != nil {
					if postPageErr == nil {
						postPageErr = err
					}
					log.Printf("outlook graph folder metadata side effect %s/%s: %v", accountID, graphFolder.DisplayName, err)
				}
				if err := o.storeOutlookGraphBodies(ctx, accountID, idsByProvider, bodySources); err != nil {
					if postPageErr == nil {
						postPageErr = err
					}
					log.Printf("outlook graph folder body side effect %s/%s: %v", accountID, graphFolder.DisplayName, err)
				}
			}
		}
		if full {
			fetched += len(response.Value)
		} else {
			fetched += len(upserts)
		}
		if len(response.Value) > 0 {
			o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folder.ID, FolderRole: folder.Role, Current: fetched, Total: eventTotal, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, "graph", map[string]any{"total_estimated": totalEstimated})})
		}
		if folderStateDirty && len(upserts) == 0 {
			if err := o.db.RefreshFolderThreadState(persistCtx, folder.ID); err != nil {
				return fmt.Errorf("refresh outlook graph folder removals %s/%s: %w", accountID, graphFolder.DisplayName, err)
			}
		}

		if strings.TrimSpace(response.NextLink) != "" {
			endpoint = strings.TrimSpace(response.NextLink)
			if full {
				if err := o.db.UpdateProviderFolderPageCursor(persistCtx, folder.ID, endpoint, fetched, graphFolder.TotalItemCount, graphFolder.UnreadItemCount); err != nil {
					return err
				}
			}
			continue
		}
		if strings.TrimSpace(response.DeltaLink) != "" {
			deltaLink := strings.TrimSpace(response.DeltaLink)
			if postPageErr != nil {
				if err := o.db.RefreshFolderThreadState(persistCtx, folder.ID); err != nil {
					return fmt.Errorf("refresh folder after partial outlook graph sync %s/%s: %w", accountID, graphFolder.DisplayName, err)
				}
				return postPageErr
			}
			if !full {
				if err := o.db.UpdateProviderFolderSyncState(persistCtx, folder.ID, deltaLink, graphFolder.TotalItemCount, graphFolder.UnreadItemCount, false); err != nil {
					return err
				}
				return nil
			}
			if full && fullStartedFromBaseline {
				if _, err := o.db.MarkProviderMessagesMissingFromFolder(persistCtx, accountID, folder.ID, seenProviderIDs); err != nil {
					return err
				}
			}
			if err := o.db.RefreshFolderThreadState(persistCtx, folder.ID); err != nil {
				return fmt.Errorf("finalize outlook graph folder threads %s/%s: %w", accountID, graphFolder.DisplayName, err)
			}
			return o.db.UpdateProviderFolderSyncState(persistCtx, folder.ID, deltaLink, graphFolder.TotalItemCount, graphFolder.UnreadItemCount, full)
		}
		endpoint = ""
	}
	return nil
}

func outlookGraphFolderNeedsFullReconcile(folder storage.FolderSyncInfo) bool {
	if strings.TrimSpace(folder.SyncCursor) == "" {
		return true
	}
	if outlookGraphDeltaNextLink(folder.SyncCursor) {
		return true
	}
	if !folder.LastFullSyncAt.Valid || folder.LastFullSyncAt.Time.IsZero() {
		return true
	}
	return false
}

func outlookGraphDeltaNextLink(cursor string) bool {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return false
	}
	parsed, err := url.Parse(cursor)
	if err == nil {
		query := parsed.Query()
		if query.Get("$skiptoken") != "" || query.Get("skiptoken") != "" {
			return true
		}
	}
	lower := strings.ToLower(cursor)
	return strings.Contains(lower, "$skiptoken=") || strings.Contains(lower, "%24skiptoken=")
}

func outlookGraphMessageToProviderSync(accountID, folderID string, msg outlookGraphMessage, categoriesByName map[string]outlookCategory) storage.ProviderSyncMessage {
	from := msg.From.EmailAddress
	if strings.TrimSpace(from.Address) == "" {
		from = msg.Sender.EmailAddress
	}
	if strings.TrimSpace(from.Address) == "" {
		from = outlookGraphAddressHeaderValue(msg.InternetMessageHeaders, "From")
	}
	subject := strings.TrimSpace(msg.Subject)
	if subject == "" {
		subject = strings.TrimSpace(message.DecodeHeader(outlookGraphHeaderValue(msg.InternetMessageHeaders, "Subject")))
	}
	return storage.ProviderSyncMessage{
		AccountID:         accountID,
		FolderID:          folderID,
		ProviderMessageID: strings.TrimSpace(msg.ID),
		InternetMessageID: strings.TrimSpace(msg.InternetMessageID),
		ProviderThreadID:  strings.TrimSpace(msg.ConversationID),
		InReplyTo:         outlookGraphHeaderValue(msg.InternetMessageHeaders, "In-Reply-To"),
		References:        outlookGraphHeaderValue(msg.InternetMessageHeaders, "References"),
		Subject:           subject,
		FromName:          strings.TrimSpace(from.Name),
		FromEmail:         strings.TrimSpace(from.Address),
		DateSent:          msg.SentDateTime,
		DateReceived:      msg.ReceivedDateTime,
		Snippet:           strings.TrimSpace(msg.BodyPreview),
		IsRead:            msg.IsRead,
		IsStarred:         strings.EqualFold(strings.TrimSpace(msg.Flag.FlagStatus), "flagged"),
		IsFlagged:         strings.EqualFold(strings.TrimSpace(msg.Flag.FlagStatus), "flagged"),
		IsDraft:           msg.IsDraft,
		HasAttachments:    msg.HasAttachments,
		Labels:            outlookCategoryLabelInputs(accountID, msg.Categories, categoriesByName),
		LabelsKnown:       true,
		LabelProvider:     storage.LabelProviderOutlook,
		ToRecipients:      outlookGraphRecipientsWithHeaderFallback(msg.ToRecipients, msg.InternetMessageHeaders, "To"),
		CCRecipients:      outlookGraphRecipientsWithHeaderFallback(msg.CCRecipients, msg.InternetMessageHeaders, "Cc"),
		BCCRecipients:     outlookGraphRecipientsWithHeaderFallback(msg.BCCRecipients, msg.InternetMessageHeaders, "Bcc"),
	}
}

func fetchOutlookGraphMessageDetailIfNeeded(ctx context.Context, token string, msg outlookGraphMessage, includeBody bool) (outlookGraphMessage, bool, error) {
	if !outlookGraphMessageNeedsDetailFetch(msg, includeBody) {
		return msg, true, nil
	}
	providerMessageID := strings.TrimSpace(msg.ID)
	if providerMessageID == "" {
		return msg, false, nil
	}
	var detail outlookGraphMessage
	err := providerJSON(ctx, http.MethodGet, outlookGraphMessageEndpoint(providerMessageID, includeBody), token, outlookGraphHeaders(0), nil, &detail)
	if err != nil {
		if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
			return msg, false, nil
		}
		return msg, false, err
	}
	if strings.TrimSpace(detail.ID) == "" {
		detail.ID = providerMessageID
	}
	return mergeOutlookGraphMessageDetail(msg, detail), true, nil
}

func outlookGraphMessageNeedsDetailFetch(msg outlookGraphMessage, includeBody bool) bool {
	if strings.TrimSpace(msg.InternetMessageID) == "" {
		return true
	}
	if strings.TrimSpace(msg.Subject) == "" && strings.TrimSpace(outlookGraphHeaderValue(msg.InternetMessageHeaders, "Subject")) == "" {
		return true
	}
	if strings.TrimSpace(msg.From.EmailAddress.Address) == "" &&
		strings.TrimSpace(msg.Sender.EmailAddress.Address) == "" &&
		strings.TrimSpace(outlookGraphHeaderValue(msg.InternetMessageHeaders, "From")) == "" {
		return true
	}
	if includeBody && msg.ReceivedDateTime.IsZero() {
		return true
	}
	if includeBody &&
		!msg.IsDraft &&
		len(msg.ToRecipients) == 0 &&
		len(msg.CCRecipients) == 0 &&
		len(msg.BCCRecipients) == 0 &&
		strings.TrimSpace(outlookGraphHeaderValue(msg.InternetMessageHeaders, "To")) == "" &&
		strings.TrimSpace(outlookGraphHeaderValue(msg.InternetMessageHeaders, "Cc")) == "" &&
		strings.TrimSpace(outlookGraphHeaderValue(msg.InternetMessageHeaders, "Bcc")) == "" {
		return true
	}
	return false
}

func mergeOutlookGraphMessageDetail(delta, detail outlookGraphMessage) outlookGraphMessage {
	if strings.TrimSpace(detail.ID) == "" {
		detail.ID = delta.ID
	}
	if strings.TrimSpace(detail.InternetMessageID) == "" {
		detail.InternetMessageID = delta.InternetMessageID
	}
	if strings.TrimSpace(detail.ConversationID) == "" {
		detail.ConversationID = delta.ConversationID
	}
	if strings.TrimSpace(detail.ParentFolderID) == "" {
		detail.ParentFolderID = delta.ParentFolderID
	}
	if strings.TrimSpace(detail.Subject) == "" {
		detail.Subject = delta.Subject
	}
	if strings.TrimSpace(detail.BodyPreview) == "" {
		detail.BodyPreview = delta.BodyPreview
	}
	if strings.TrimSpace(detail.Body.Content) == "" {
		detail.Body = delta.Body
	}
	if len(detail.Categories) == 0 {
		detail.Categories = delta.Categories
	}
	if strings.TrimSpace(detail.From.EmailAddress.Address) == "" {
		detail.From = delta.From
	}
	if strings.TrimSpace(detail.Sender.EmailAddress.Address) == "" {
		detail.Sender = delta.Sender
	}
	if len(detail.ToRecipients) == 0 {
		detail.ToRecipients = delta.ToRecipients
	}
	if len(detail.CCRecipients) == 0 {
		detail.CCRecipients = delta.CCRecipients
	}
	if len(detail.BCCRecipients) == 0 {
		detail.BCCRecipients = delta.BCCRecipients
	}
	if detail.ReceivedDateTime.IsZero() {
		detail.ReceivedDateTime = delta.ReceivedDateTime
	}
	if detail.SentDateTime.IsZero() {
		detail.SentDateTime = delta.SentDateTime
	}
	if len(detail.InternetMessageHeaders) == 0 {
		detail.InternetMessageHeaders = delta.InternetMessageHeaders
	}
	return detail
}

func (o *SyncOrchestrator) syncOutlookGraphAttachmentMetadata(ctx context.Context, token string, idsByProvider map[string]int64, messages []outlookGraphMessage) error {
	if o.db == nil {
		return nil
	}
	for _, msg := range messages {
		providerMessageID := strings.TrimSpace(msg.ID)
		localID := idsByProvider[providerMessageID]
		if providerMessageID == "" || localID == 0 {
			continue
		}
		if !msg.HasAttachments {
			if err := o.db.ReplaceAttachments(ctx, localID, nil); err != nil {
				return fmt.Errorf("outlook graph attachment metadata clear message=%s local=%d: %w", providerMessageID, localID, err)
			}
			continue
		}
		attachments, err := listOutlookGraphMessageAttachments(ctx, token, providerMessageID)
		if err != nil {
			return fmt.Errorf("outlook graph attachment metadata message=%s local=%d: %w", providerMessageID, localID, err)
		}
		rows := make([]storage.AttachmentRow, 0, len(attachments))
		for _, att := range attachments {
			providerAttachmentID := strings.TrimSpace(att.ID)
			if providerAttachmentID == "" {
				continue
			}
			filename := strings.TrimSpace(att.Name)
			if filename == "" {
				filename = providerAttachmentID
			}
			contentType := strings.TrimSpace(att.ContentType)
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			rows = append(rows, storage.AttachmentRow{
				Filename:         filename,
				ContentType:      contentType,
				SizeBytes:        att.Size,
				ContentID:        strings.Trim(strings.TrimSpace(att.ContentID), "<>"),
				Inline:           att.IsInline,
				StoragePath:      "",
				ProviderRemoteID: providerAttachmentID,
			})
		}
		if err := o.db.ReplaceAttachments(ctx, localID, rows); err != nil {
			return fmt.Errorf("outlook graph attachment metadata store message=%s local=%d: %w", providerMessageID, localID, err)
		}
	}
	return nil
}

func listOutlookGraphMessageAttachments(ctx context.Context, token, providerMessageID string) ([]outlookGraphAttachment, error) {
	endpoint := outlookGraphMessageAttachmentsEndpoint(providerMessageID)
	var out []outlookGraphAttachment
	for endpoint != "" {
		var response outlookGraphAttachmentsResponse
		if err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookGraphHeaders(0), nil, &response); err != nil {
			return nil, err
		}
		out = append(out, response.Value...)
		endpoint = strings.TrimSpace(response.NextLink)
	}
	if err := enrichOutlookGraphFileAttachmentContentIDs(ctx, token, providerMessageID, out); err != nil {
		return nil, err
	}
	return out, nil
}

func outlookGraphMessageAttachmentsEndpoint(providerMessageID string) string {
	values := url.Values{}
	values.Set("$select", "id,name,contentType,size,isInline")
	return outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/attachments?" + values.Encode()
}

func enrichOutlookGraphFileAttachmentContentIDs(ctx context.Context, token, providerMessageID string, attachments []outlookGraphAttachment) error {
	for i := range attachments {
		if !attachments[i].IsInline || strings.TrimSpace(attachments[i].ContentID) != "" || !outlookGraphAttachmentMayHaveContentID(attachments[i]) {
			continue
		}
		var detail outlookGraphAttachment
		if err := providerJSON(ctx, http.MethodGet, outlookGraphAttachmentContentIDEndpoint(providerMessageID, attachments[i].ID), token, outlookGraphHeaders(0), nil, &detail); err != nil {
			if status, ok := providerAPIStatus(err); ok && (status == http.StatusBadRequest || status == http.StatusNotFound) {
				continue
			}
			return fmt.Errorf("outlook graph inline attachment content id message=%s attachment=%s: %w", providerMessageID, attachments[i].ID, err)
		}
		attachments[i].ContentID = strings.TrimSpace(detail.ContentID)
	}
	return nil
}

func outlookGraphAttachmentMayHaveContentID(att outlookGraphAttachment) bool {
	odataType := strings.Trim(strings.TrimSpace(att.ODataType), "#")
	return odataType == "" || strings.EqualFold(odataType, "microsoft.graph.fileAttachment")
}

func outlookGraphAttachmentContentIDEndpoint(providerMessageID, attachmentID string) string {
	values := url.Values{}
	values.Set("$select", "id,contentId")
	return outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/attachments/" + url.PathEscape(attachmentID) + "?" + values.Encode()
}

func outlookGraphHeaderValue(headers []outlookGraphHeader, name string) string {
	for _, header := range headers {
		if strings.EqualFold(strings.TrimSpace(header.Name), name) {
			return strings.TrimSpace(header.Value)
		}
	}
	return ""
}

func outlookGraphAddressHeaderValue(headers []outlookGraphHeader, name string) outlookGraphEmailAddress {
	value := outlookGraphHeaderValue(headers, name)
	if value == "" {
		return outlookGraphEmailAddress{}
	}
	addresses, err := netmail.ParseAddressList(value)
	if err != nil || len(addresses) == 0 {
		return outlookGraphEmailAddress{}
	}
	return outlookGraphEmailAddress{
		Name:    strings.TrimSpace(message.DecodeHeader(addresses[0].Name)),
		Address: strings.TrimSpace(addresses[0].Address),
	}
}

func outlookGraphRecipients(recipients []outlookGraphRecipient) []storage.Recipient {
	out := make([]storage.Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		email := strings.TrimSpace(recipient.EmailAddress.Address)
		if email == "" {
			continue
		}
		out = append(out, storage.Recipient{
			Name:  strings.TrimSpace(recipient.EmailAddress.Name),
			Email: email,
		})
	}
	return out
}

func outlookGraphRecipientsWithHeaderFallback(recipients []outlookGraphRecipient, headers []outlookGraphHeader, headerName string) []storage.Recipient {
	out := outlookGraphRecipients(recipients)
	if len(out) > 0 {
		return out
	}
	value := outlookGraphHeaderValue(headers, headerName)
	if value == "" {
		return out
	}
	addresses, err := netmail.ParseAddressList(value)
	if err != nil {
		return out
	}
	out = make([]storage.Recipient, 0, len(addresses))
	for _, address := range addresses {
		if strings.TrimSpace(address.Address) == "" {
			continue
		}
		out = append(out, storage.Recipient{
			Name:  strings.TrimSpace(message.DecodeHeader(address.Name)),
			Email: strings.TrimSpace(address.Address),
		})
	}
	return out
}

func (o *SyncOrchestrator) storeOutlookGraphBodies(ctx context.Context, accountID string, idsByProvider map[string]int64, messages []outlookGraphMessage) error {
	if o.blobStore == nil || o.db == nil {
		return nil
	}
	for _, msg := range messages {
		localID := idsByProvider[strings.TrimSpace(msg.ID)]
		if localID == 0 || strings.TrimSpace(msg.Body.Content) == "" || o.db.IsBodyFetched(ctx, localID) {
			continue
		}
		snippet := strings.TrimSpace(msg.BodyPreview)
		if snippet == "" {
			snippet = strings.TrimSpace(msg.Subject)
		}

		var textPath, htmlPath, originalHTMLPath string
		switch strings.ToLower(strings.TrimSpace(msg.Body.ContentType)) {
		case "html":
			htmlBody := []byte(msg.Body.Content)
			p, err := o.blobStore.StoreBodyOriginalHTML(ctx, accountID, localID, htmlBody)
			if err != nil {
				return fmt.Errorf("outlook graph body original html store message=%d: %w", localID, err)
			}
			originalHTMLPath = p
			sanitized := message.SanitizeHTML(htmlBody)
			sanitized = message.RewriteCIDReferences(sanitized, o.outlookGraphCIDURLMap(ctx, localID))
			p, err = o.blobStore.StoreBodyHTML(ctx, accountID, localID, sanitized)
			if err != nil {
				return fmt.Errorf("outlook graph body html store message=%d: %w", localID, err)
			}
			htmlPath = p
		default:
			text := strings.TrimSpace(msg.Body.Content)
			p, err := o.blobStore.StoreBodyText(ctx, accountID, localID, []byte(text))
			if err != nil {
				return fmt.Errorf("outlook graph body text store message=%d: %w", localID, err)
			}
			textPath = p
			if text != "" {
				wrapped := "<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit;margin:0;padding:8px\">" +
					template.HTMLEscapeString(text) + "</pre>"
				p, err := o.blobStore.StoreBodyHTML(ctx, accountID, localID, []byte(wrapped))
				if err != nil {
					return fmt.Errorf("outlook graph body html wrapper store message=%d: %w", localID, err)
				}
				htmlPath = p
			}
		}
		if textPath == "" && htmlPath == "" {
			continue
		}
		if originalHTMLPath != "" {
			if err := o.db.UpdateMessageOriginalHTMLPath(ctx, localID, originalHTMLPath); err != nil {
				return fmt.Errorf("outlook graph body original html update message=%d: %w", localID, err)
			}
		}
		if err := o.db.UpdateMessageBody(ctx, localID, textPath, htmlPath, "", snippet); err != nil {
			return fmt.Errorf("outlook graph body update message=%d: %w", localID, err)
		}
	}
	return nil
}

func (o *SyncOrchestrator) outlookGraphCIDURLMap(ctx context.Context, localID int64) map[string]string {
	cidToURL := map[string]string{}
	if o.db == nil || localID == 0 {
		return cidToURL
	}
	attachments, err := o.db.GetAttachments(ctx, localID)
	if err != nil {
		return cidToURL
	}
	for _, attachment := range attachments {
		if attachment.Inline && strings.TrimSpace(attachment.ContentID) != "" {
			cidToURL[attachment.ContentID] = outlookInlineContentURL(localID, attachment.ContentID)
		}
	}
	return cidToURL
}

func outlookInlineContentURL(messageID int64, contentID string) string {
	return fmt.Sprintf("/api/inline-content/%d/%s", messageID, url.PathEscape(contentID))
}
