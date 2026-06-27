package mail

import (
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const gmailAPIMessagePageSize = 500

type gmailAPIMessageFetchMode string

const (
	gmailAPIMessageFetchFull     gmailAPIMessageFetchMode = "full"
	gmailAPIMessageFetchMetadata gmailAPIMessageFetchMode = "metadata"
)

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
	Synced            bool
	Skipped           bool
}

func gmailAPIMailEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOFER_GMAIL_API_SYNC"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
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

	targets, labelsByID, err := o.syncGmailAPIFolders(ctx, accountID, token)
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

	o.backfillGmailAPIMessageIDs(ctx, accountID, token, 250)

	if !includeIDLEFolders {
		state, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, "messages")
		if err == nil && strings.TrimSpace(state.Cursor) != "" && state.LastFullSyncAt.Valid {
			return o.syncGmailAPIHistoryChanges(ctx, accountID, token, labelsByID, targets, state.Cursor)
		}
	}
	return o.syncGmailAPIFull(ctx, accountID, token, labelsByID, targets)
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
			ID:               folderIDFromRemote(accountID, remoteName),
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
		ID:               folderIDFromRemote(accountID, archiveRemote),
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

func (o *SyncOrchestrator) syncGmailAPIFull(ctx context.Context, accountID, token string, labelsByID map[string]gmailAPILabel, targets []gmailAPIFolderSyncTarget) (retErr error) {
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
	seenProviderIDs := map[string]bool{}
	failed := 0
	processed := 0
	totalEstimate := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageToken := ""
		estimateRecorded := false
		for {
			page, err := listGmailAPIMessageIDPage(ctx, token, target.LabelID, pageToken)
			if err != nil {
				return err
			}
			if !estimateRecorded && page.ResultSize > 0 {
				totalEstimate += page.ResultSize
				estimateRecorded = true
			}
			for _, ref := range page.Messages {
				providerID := strings.TrimSpace(ref.ID)
				if providerID == "" || seenProviderIDs[providerID] {
					continue
				}
				seenProviderIDs[providerID] = true
				processed++
				result, err := o.syncGmailAPIProviderMessage(ctx, accountID, token, providerID, labelsByID, targetsByLabelID, gmailAPIMessageFetchMetadata)
				if err != nil {
					if providerLabelSyncShouldStop(err) {
						stats.FailedMessages++
						return err
					}
					failed++
					stats.FailedMessages++
					if providerMessageNotFound(err) {
						stats.MissingProviderMessages++
					}
					log.Printf("gmail api sync message account=%s provider_message=%s: %v", accountID, providerID, err)
					continue
				}
				stats.Cursor = newerGmailHistoryID(stats.Cursor, result.HistoryID)
				if result.Skipped {
					stats.SkippedMessages++
					continue
				}
				stats.SyncedMessages++
				stats.WithLabels++
				if processed%25 == 0 {
					total := totalEstimate
					if total < processed {
						total = processed
					}
					o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, Current: processed, Total: total, Payload: accountSyncProgressPayload(ctx, "", map[string]any{
						"provider": "gmail_api",
					})})
				}
			}
			pageToken = strings.TrimSpace(page.NextPageToken)
			if pageToken == "" {
				break
			}
		}
	}
	stats.TotalMessages = len(seenProviderIDs)
	o.replayGmailLabelMutationQueue(ctx, accountID, token)
	if cursor, err := getGmailProfileHistoryID(ctx, token); err == nil {
		stats.Cursor = newerGmailHistoryID(stats.Cursor, cursor)
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else {
		log.Printf("gmail api sync profile cursor account=%s: %v", accountID, err)
	}
	if failed > 0 {
		return fmt.Errorf("%d Gmail API message import(s) failed", failed)
	}
	return nil
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

	providerIDs, latestCursor, err := gmailHistoryChangedMessageIDs(ctx, token, cursor)
	if err != nil {
		if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
			log.Printf("gmail api sync account=%s history cursor expired, running full reconciliation", accountID)
			fallbackFull = true
			return o.syncGmailAPIFull(ctx, accountID, token, labelsByID, targets)
		}
		return err
	}
	stats.Cursor = newerGmailHistoryID(stats.Cursor, latestCursor)
	stats.TotalMessages = len(providerIDs)
	targetsByLabelID := gmailAPITargetsByLabelID(targets)

	failed := 0
	for _, providerID := range providerIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := o.syncGmailAPIProviderMessage(ctx, accountID, token, providerID, labelsByID, targetsByLabelID, gmailAPIMessageFetchFull)
		if err != nil {
			if providerLabelSyncShouldStop(err) {
				stats.FailedMessages++
				return err
			}
			failed++
			stats.FailedMessages++
			if providerMessageNotFound(err) {
				stats.MissingProviderMessages++
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
	}
	o.replayGmailLabelMutationQueue(ctx, accountID, token)
	if failed > 0 {
		return fmt.Errorf("%d Gmail API message change import(s) failed", failed)
	}
	return nil
}

func (o *SyncOrchestrator) syncGmailAPIProviderMessage(ctx context.Context, accountID, token, providerMessageID string, labelsByID map[string]gmailAPILabel, targetsByLabelID map[string]gmailAPIFolderSyncTarget, mode gmailAPIMessageFetchMode) (gmailAPIMessageSyncResult, error) {
	msg, err := getGmailAPIMessage(ctx, token, providerMessageID, mode)
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
	if mode == gmailAPIMessageFetchFull {
		if err := o.db.ReplaceAttachments(ctx, localID, gmailAPIAttachmentRows(msg)); err != nil {
			log.Printf("gmail api attachment metadata account=%s message=%s local=%d: %v", accountID, msg.ID, localID, err)
		}
		o.storeGmailAPIBody(ctx, accountID, localID, msg)
	}
	return gmailAPIMessageSyncResult{ProviderMessageID: msg.ID, LocalMessageID: localID, HistoryID: msg.HistoryID, Synced: true}, nil
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

func getGmailAPIMessage(ctx context.Context, token, providerMessageID string, mode gmailAPIMessageFetchMode) (gmailAPIMessage, error) {
	if mode == "" {
		mode = gmailAPIMessageFetchFull
	}
	values := url.Values{}
	values.Set("format", string(mode))
	if mode == gmailAPIMessageFetchMetadata {
		for _, header := range gmailAPIMessageMetadataHeaders {
			values.Add("metadataHeaders", header)
		}
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

func (o *SyncOrchestrator) storeGmailAPIBody(ctx context.Context, accountID string, localID int64, msg gmailAPIMessage) {
	if o.blobStore == nil || o.db == nil || o.db.IsBodyFetched(ctx, localID) {
		return
	}
	text, htmlBody := gmailAPIBodyContent(msg.Payload)
	snippet := strings.TrimSpace(msg.Snippet)
	if snippet == "" {
		snippet = strings.TrimSpace(gmailAPIHeaders(msg.Payload.Headers)["subject"])
	}
	var textPath, htmlPath, originalHTMLPath string
	if strings.TrimSpace(text) != "" {
		if p, err := o.blobStore.StoreBodyText(ctx, accountID, localID, []byte(text)); err == nil {
			textPath = p
		}
	}
	if len(htmlBody) > 0 {
		if p, err := o.blobStore.StoreBodyOriginalHTML(ctx, accountID, localID, htmlBody); err == nil {
			originalHTMLPath = p
		}
		sanitized := message.SanitizeHTML(htmlBody)
		sanitized = message.RewriteCIDReferences(sanitized, gmailAPICIDURLMap(localID, msg))
		if p, err := o.blobStore.StoreBodyHTML(ctx, accountID, localID, sanitized); err == nil {
			htmlPath = p
		}
	} else if strings.TrimSpace(text) != "" {
		wrapped := "<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit;margin:0;padding:8px\">" +
			template.HTMLEscapeString(text) + "</pre>"
		if p, err := o.blobStore.StoreBodyHTML(ctx, accountID, localID, []byte(wrapped)); err == nil {
			htmlPath = p
		}
	}
	if textPath == "" && htmlPath == "" {
		return
	}
	if err := o.db.UpdateMessageBody(ctx, localID, textPath, htmlPath, "", snippet); err != nil {
		log.Printf("gmail api body update message=%d: %v", localID, err)
		return
	}
	if originalHTMLPath != "" {
		_ = o.db.UpdateMessageOriginalHTMLPath(ctx, localID, originalHTMLPath)
	}
}

func gmailAPIBodyContent(part gmailAPIPart) (text string, htmlBody []byte) {
	var walk func(gmailAPIPart)
	walk = func(p gmailAPIPart) {
		mimeType := strings.ToLower(strings.TrimSpace(p.MimeType))
		if p.Body.Data != "" {
			data := gmailAPIDecodeBody(p.Body.Data)
			switch mimeType {
			case "text/html":
				if len(htmlBody) == 0 {
					htmlBody = data
				}
			case "text/plain":
				if text == "" {
					text = string(data)
				}
			}
		}
		for _, child := range p.Parts {
			walk(child)
		}
	}
	walk(part)
	return text, htmlBody
}

func gmailAPIDecodeBody(data string) []byte {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return raw
	}
	raw, _ := base64.URLEncoding.DecodeString(data)
	return raw
}

func gmailAPICIDURLMap(localID int64, msg gmailAPIMessage) map[string]string {
	cidToURL := map[string]string{}
	for _, attachment := range gmailAPIAttachmentRows(msg) {
		if attachment.Inline && strings.TrimSpace(attachment.ContentID) != "" {
			cidToURL[attachment.ContentID] = fmt.Sprintf("/api/inline-content/%d/%s", localID, url.PathEscape(attachment.ContentID))
		}
	}
	return cidToURL
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

func (o *SyncOrchestrator) backfillGmailAPIMessageIDs(ctx context.Context, accountID, token string, limit int) {
	candidates, err := o.db.ListProviderMessageIDBackfillCandidates(ctx, accountID, limit)
	if err != nil {
		log.Printf("gmail api id backfill %s: list candidates: %v", accountID, err)
		return
	}
	var resolved, missing, failed int
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			log.Printf("gmail api id backfill %s: context ended after %d resolved, %d missing, %d failed", accountID, resolved, missing, failed)
			return
		}
		providerID, err := gmailMessageIDForInternetID(ctx, token, candidate.InternetMessageID)
		if err != nil {
			failed++
			log.Printf("gmail api id backfill %s message=%d: %v", accountID, candidate.MessageID, err)
			continue
		}
		if providerID == "" {
			missing++
			continue
		}
		if err := o.db.SetMessageProviderMessageID(ctx, candidate.MessageID, providerID); err != nil {
			failed++
			log.Printf("gmail api id backfill %s message=%d cache: %v", accountID, candidate.MessageID, err)
			continue
		}
		resolved++
	}
	if resolved > 0 || missing > 0 || failed > 0 {
		log.Printf("gmail api id backfill %s: %d resolved, %d missing, %d failed", accountID, resolved, missing, failed)
	}
}
