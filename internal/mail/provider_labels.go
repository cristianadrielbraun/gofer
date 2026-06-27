package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	mailimap "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

var (
	gmailAPIBaseURL     = "https://gmail.googleapis.com/gmail/v1"
	outlookGraphBaseURL = "https://graph.microsoft.com/v1.0"
)

const providerLabelSyncBatchSize = 250
const providerLabelMutationReplayLimit = 100
const outlookProviderBatchSize = 20

var errProviderLabelAuth = errors.New("provider label auth failed")

type providerAPIError struct {
	StatusCode int
	Body       string
}

func (e *providerAPIError) Error() string {
	if e == nil {
		return ""
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("provider api returned %d", e.StatusCode)
	}
	return fmt.Sprintf("provider api returned %d: %s", e.StatusCode, body)
}

func providerAPIStatus(err error) (int, bool) {
	var apiErr *providerAPIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.StatusCode, true
	}
	return 0, false
}

func providerLabelSyncShouldStop(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, errProviderLabelAuth) {
		return true
	}
	status, ok := providerAPIStatus(err)
	if !ok {
		return false
	}
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func providerLabelSyncShouldFailAccount(providerType string, err error) bool {
	if !providerLabelSyncShouldStop(err) {
		return false
	}
	// Outlook Graph category access is separate from IMAP XOAUTH2 mail access.
	// Keep category failures in label_sync_state instead of marking mail sync broken.
	return providerType != storage.LabelProviderOutlook
}

func (o *SyncOrchestrator) syncProviderLabels(ctx context.Context, accountID, accountProvider string) error {
	if o.db == nil {
		return nil
	}
	if o.tokenProvider == nil {
		switch strings.TrimSpace(accountProvider) {
		case providers.ProviderGmail, providers.ProviderOutlook:
			return nil
		default:
			o.replayIMAPLabelMutationQueue(ctx, accountID)
		}
		return nil
	}

	var err error
	var providerType string
	switch strings.TrimSpace(accountProvider) {
	case providers.ProviderGmail:
		providerType = storage.LabelProviderGmail
		err = o.syncGmailLabels(ctx, accountID)
	case providers.ProviderOutlook:
		providerType = storage.LabelProviderOutlook
		err = o.syncOutlookCategories(ctx, accountID)
	default:
		o.replayIMAPLabelMutationQueue(ctx, accountID)
		return nil
	}
	if err == nil {
		return nil
	}
	log.Printf("provider label sync %s/%s: %v", accountID, providerType, err)
	if markErr := o.db.MarkLabelSyncError(context.Background(), accountID, providerType, "messages", err); markErr != nil {
		log.Printf("provider label sync error state %s/%s: %v", accountID, providerType, markErr)
	}
	if providerLabelSyncShouldFailAccount(providerType, err) {
		return err
	}
	return nil
}

func (o *SyncOrchestrator) syncProviderLabelChanges(ctx context.Context, accountID, accountProvider string) error {
	if o.db == nil {
		return nil
	}
	if o.tokenProvider == nil {
		switch strings.TrimSpace(accountProvider) {
		case providers.ProviderGmail, providers.ProviderOutlook:
			return nil
		default:
			o.replayIMAPLabelMutationQueue(ctx, accountID)
		}
		return nil
	}

	var err error
	var providerType string
	switch strings.TrimSpace(accountProvider) {
	case providers.ProviderGmail:
		providerType = storage.LabelProviderGmail
		err = o.syncGmailLabelChanges(ctx, accountID)
	case providers.ProviderOutlook:
		providerType = storage.LabelProviderOutlook
		err = o.syncOutlookCategories(ctx, accountID)
	default:
		o.replayIMAPLabelMutationQueue(ctx, accountID)
		return nil
	}
	if err == nil {
		return nil
	}
	log.Printf("provider label sync %s/%s: %v", accountID, providerType, err)
	if markErr := o.db.MarkLabelSyncError(context.Background(), accountID, providerType, "messages", err); markErr != nil {
		log.Printf("provider label sync error state %s/%s: %v", accountID, providerType, markErr)
	}
	if providerLabelSyncShouldFailAccount(providerType, err) {
		return err
	}
	return nil
}

type gmailLabelsResponse struct {
	Labels []gmailLabel `json:"labels"`
}

type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type gmailMessagesResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

type gmailMessageState struct {
	ID        string   `json:"id"`
	LabelIDs  []string `json:"labelIds"`
	HistoryID string   `json:"historyId"`
	Payload   struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

type gmailProfile struct {
	HistoryID string `json:"historyId"`
}

type gmailHistoryResponse struct {
	History       []gmailHistoryRecord `json:"history"`
	NextPageToken string               `json:"nextPageToken"`
	HistoryID     string               `json:"historyId"`
}

type gmailHistoryRecord struct {
	MessagesAdded []gmailHistoryMessageChange `json:"messagesAdded"`
	LabelsAdded   []gmailHistoryLabelChange   `json:"labelsAdded"`
	LabelsRemoved []gmailHistoryLabelChange   `json:"labelsRemoved"`
}

type gmailHistoryMessageChange struct {
	Message gmailHistoryMessage `json:"message"`
}

type gmailHistoryLabelChange struct {
	Message  gmailHistoryMessage `json:"message"`
	LabelIDs []string            `json:"labelIds"`
}

type gmailHistoryMessage struct {
	ID string `json:"id"`
}

func (o *SyncOrchestrator) syncGmailLabels(ctx context.Context, accountID string) (retErr error) {
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
	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: %w", errProviderLabelAuth, err)
	}

	labelsByID, err := o.syncGmailLabelCatalog(ctx, accountID, token)
	if err != nil {
		return err
	}

	failed := 0
	for afterID := int64(0); ; {
		messages, err := o.db.ListProviderLabelSyncMessages(ctx, accountID, afterID, providerLabelSyncBatchSize)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			break
		}
		stats.TotalMessages += len(messages)
		for _, msg := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			afterID = msg.ID
			result, err := o.syncGmailMessageLabels(ctx, token, msg, labelsByID)
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
				log.Printf("gmail label sync message account=%s message=%d: %v", accountID, msg.ID, err)
				continue
			}
			stats.SyncedMessages++
			stats.Cursor = newerGmailHistoryID(stats.Cursor, result.HistoryID)
			if result.WithLabels {
				stats.WithLabels++
			} else {
				stats.WithoutLabels++
			}
		}
		if len(messages) < providerLabelSyncBatchSize {
			break
		}
	}
	o.replayGmailLabelMutationQueue(ctx, accountID, token)
	if cursor, err := getGmailProfileHistoryID(ctx, token); err == nil {
		stats.Cursor = newerGmailHistoryID(stats.Cursor, cursor)
	} else {
		log.Printf("gmail label sync profile cursor account=%s: %v", accountID, err)
	}
	if failed > 0 {
		return fmt.Errorf("%d Gmail message label import(s) failed", failed)
	}
	return nil
}

func (o *SyncOrchestrator) syncGmailLabelChanges(ctx context.Context, accountID string) (retErr error) {
	state, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, "messages")
	if err != nil {
		return err
	}
	cursor := strings.TrimSpace(state.Cursor)
	if cursor == "" || !state.LastFullSyncAt.Valid {
		return o.syncGmailLabels(ctx, accountID)
	}

	stats := storage.LabelSyncRunStats{
		AccountID:    accountID,
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		StartedAt:    time.Now().UTC(),
		Full:         false,
		Cursor:       cursor,
	}
	fallbackFull := false
	defer func() {
		if fallbackFull {
			return
		}
		retErr = o.completeProviderLabelSyncRun(stats, retErr)
	}()

	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: %w", errProviderLabelAuth, err)
	}

	providerIDs, latestCursor, err := gmailHistoryChangedMessageIDs(ctx, token, cursor)
	if err != nil {
		if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
			log.Printf("gmail label sync account=%s history cursor expired, running full reconciliation", accountID)
			fallbackFull = true
			return o.syncGmailLabels(ctx, accountID)
		}
		return err
	}
	stats.Cursor = newerGmailHistoryID(stats.Cursor, latestCursor)
	stats.TotalMessages = len(providerIDs)

	labelsByID := map[string]gmailLabel{}
	if len(providerIDs) > 0 {
		labelsByID, err = o.syncGmailLabelCatalog(ctx, accountID, token)
		if err != nil {
			return err
		}
	}

	failed := 0
	for _, providerID := range providerIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := o.syncGmailHistoryMessageLabels(ctx, token, accountID, providerID, labelsByID)
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
			log.Printf("gmail label sync history account=%s provider_message=%s: %v", accountID, providerID, err)
			continue
		}
		stats.Cursor = newerGmailHistoryID(stats.Cursor, result.HistoryID)
		if result.Skipped {
			stats.SkippedMessages++
			continue
		}
		stats.SyncedMessages++
		if result.WithLabels {
			stats.WithLabels++
		} else {
			stats.WithoutLabels++
		}
	}
	o.replayGmailLabelMutationQueue(ctx, accountID, token)
	if failed > 0 {
		return fmt.Errorf("%d Gmail message label change import(s) failed", failed)
	}
	return nil
}

func (o *SyncOrchestrator) syncGmailLabelCatalog(ctx context.Context, accountID, token string) (map[string]gmailLabel, error) {
	var response gmailLabelsResponse
	if err := providerJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", token, nil, nil, &response); err != nil {
		return nil, err
	}

	labelsByID := make(map[string]gmailLabel, len(response.Labels))
	inputs := make([]storage.LabelInput, 0, len(response.Labels))
	for _, label := range response.Labels {
		label.ID = strings.TrimSpace(label.ID)
		label.Name = strings.TrimSpace(label.Name)
		if label.ID == "" || label.Name == "" {
			continue
		}
		labelsByID[label.ID] = label
		if gmailLabelIsSystem(label) {
			continue
		}
		inputs = append(inputs, storage.LabelInput{
			AccountID:    accountID,
			Name:         label.Name,
			ProviderID:   label.ID,
			ProviderType: storage.LabelProviderGmail,
			IsSystem:     false,
		})
	}
	if len(inputs) > 0 {
		if err := o.db.UpsertLabels(ctx, inputs); err != nil {
			return nil, err
		}
	}
	return labelsByID, nil
}

func (o *SyncOrchestrator) syncGmailMessageLabels(ctx context.Context, token string, msg storage.ProviderLabelSyncMessage, labelsByID map[string]gmailLabel) (gmailLabelSyncResult, error) {
	providerMessageID := strings.TrimSpace(msg.ProviderMessageID)
	if providerMessageID == "" {
		resolved, err := gmailMessageIDForInternetID(ctx, token, msg.InternetMessageID)
		if err != nil {
			return gmailLabelSyncResult{}, err
		}
		providerMessageID = resolved
		if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, providerMessageID); err != nil {
			log.Printf("cache gmail message id failed: %v", err)
		}
	}

	state, err := getGmailMessageState(ctx, token, providerMessageID)
	if err != nil {
		return gmailLabelSyncResult{}, err
	}
	return o.applyGmailMessageState(ctx, msg, providerMessageID, state, labelsByID)
}

func (o *SyncOrchestrator) syncGmailHistoryMessageLabels(ctx context.Context, token, accountID, providerMessageID string, labelsByID map[string]gmailLabel) (gmailLabelSyncResult, error) {
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" {
		return gmailLabelSyncResult{Skipped: true}, nil
	}
	state, err := getGmailMessageState(ctx, token, providerMessageID)
	if err != nil {
		return gmailLabelSyncResult{}, err
	}
	msg, err := o.db.GetProviderLabelSyncMessage(ctx, accountID, strings.TrimSpace(state.ID), gmailStateInternetMessageID(state))
	if err != nil {
		return gmailLabelSyncResult{}, err
	}
	if msg == nil {
		return gmailLabelSyncResult{HistoryID: state.HistoryID, Skipped: true}, nil
	}
	return o.applyGmailMessageState(ctx, *msg, providerMessageID, state, labelsByID)
}

func (o *SyncOrchestrator) applyGmailMessageState(ctx context.Context, msg storage.ProviderLabelSyncMessage, providerMessageID string, state gmailMessageState, labelsByID map[string]gmailLabel) (gmailLabelSyncResult, error) {
	if strings.TrimSpace(state.ID) != "" && strings.TrimSpace(state.ID) != strings.TrimSpace(msg.ProviderMessageID) {
		if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, strings.TrimSpace(state.ID)); err != nil {
			log.Printf("cache gmail message id failed: %v", err)
		}
	} else if strings.TrimSpace(msg.ProviderMessageID) == "" && strings.TrimSpace(providerMessageID) != "" {
		if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, strings.TrimSpace(providerMessageID)); err != nil {
			log.Printf("cache gmail message id failed: %v", err)
		}
	}
	labels := gmailLabelInputs(msg.AccountID, state.LabelIDs, labelsByID)
	if err := o.db.ReplaceMessageLabelsForProvider(ctx, msg.ID, msg.AccountID, storage.LabelProviderGmail, labels); err != nil {
		return gmailLabelSyncResult{}, err
	}
	if err := o.db.SyncGmailInboxMembership(ctx, msg.ID, msg.AccountID, state.LabelIDs); err != nil {
		return gmailLabelSyncResult{}, err
	}
	return gmailLabelSyncResult{WithLabels: len(labels) > 0, HistoryID: state.HistoryID}, nil
}

func gmailMessageIDForInternetID(ctx context.Context, token, internetMessageID string) (string, error) {
	internetMessageID = strings.TrimSpace(internetMessageID)
	if internetMessageID == "" {
		return "", fmt.Errorf("message has no internet message id")
	}
	values := url.Values{}
	values.Set("q", "rfc822msgid:"+internetMessageID)
	values.Set("includeSpamTrash", "true")
	values.Set("maxResults", "1")

	var response gmailMessagesResponse
	endpoint := gmailAPIBaseURL + "/users/me/messages?" + values.Encode()
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, nil, nil, &response); err != nil {
		return "", err
	}
	if len(response.Messages) == 0 || strings.TrimSpace(response.Messages[0].ID) == "" {
		return "", fmt.Errorf("gmail message not found")
	}
	return strings.TrimSpace(response.Messages[0].ID), nil
}

func getGmailMessageState(ctx context.Context, token, providerMessageID string) (gmailMessageState, error) {
	values := url.Values{}
	values.Set("format", "metadata")
	values.Add("metadataHeaders", "Message-ID")
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "?" + values.Encode()
	var state gmailMessageState
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, nil, nil, &state); err != nil {
		return gmailMessageState{}, err
	}
	return state, nil
}

func getGmailProfileHistoryID(ctx context.Context, token string) (string, error) {
	var profile gmailProfile
	if err := providerJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/profile", token, nil, nil, &profile); err != nil {
		return "", err
	}
	return strings.TrimSpace(profile.HistoryID), nil
}

func gmailHistoryChangedMessageIDs(ctx context.Context, token, cursor string) ([]string, string, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return nil, "", nil
	}
	seen := map[string]bool{}
	var ids []string
	latestCursor := cursor
	pageToken := ""
	for {
		values := url.Values{}
		values.Set("startHistoryId", cursor)
		values.Set("maxResults", "500")
		values.Add("historyTypes", "messageAdded")
		values.Add("historyTypes", "labelAdded")
		values.Add("historyTypes", "labelRemoved")
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}

		var response gmailHistoryResponse
		endpoint := gmailAPIBaseURL + "/users/me/history?" + values.Encode()
		if err := providerJSON(ctx, http.MethodGet, endpoint, token, nil, nil, &response); err != nil {
			return nil, "", err
		}
		latestCursor = newerGmailHistoryID(latestCursor, response.HistoryID)
		add := func(providerID string) {
			providerID = strings.TrimSpace(providerID)
			if providerID == "" || seen[providerID] {
				return
			}
			seen[providerID] = true
			ids = append(ids, providerID)
		}
		for _, record := range response.History {
			for _, change := range record.MessagesAdded {
				add(change.Message.ID)
			}
			for _, change := range record.LabelsAdded {
				add(change.Message.ID)
			}
			for _, change := range record.LabelsRemoved {
				add(change.Message.ID)
			}
		}
		if strings.TrimSpace(response.NextPageToken) == "" {
			break
		}
		pageToken = strings.TrimSpace(response.NextPageToken)
	}
	sort.Strings(ids)
	return ids, latestCursor, nil
}

func gmailStateInternetMessageID(state gmailMessageState) string {
	for _, header := range state.Payload.Headers {
		if strings.EqualFold(strings.TrimSpace(header.Name), "Message-ID") {
			return strings.TrimSpace(header.Value)
		}
	}
	return ""
}

func newerGmailHistoryID(current, candidate string) string {
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return current
	}
	if current == "" {
		return candidate
	}
	left := strings.TrimLeft(current, "0")
	right := strings.TrimLeft(candidate, "0")
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	if len(right) > len(left) || (len(right) == len(left) && right > left) {
		return candidate
	}
	return current
}

func gmailLabelInputs(accountID string, labelIDs []string, labelsByID map[string]gmailLabel) []storage.LabelInput {
	inputs := make([]storage.LabelInput, 0, len(labelIDs))
	seen := map[string]bool{}
	for _, labelID := range labelIDs {
		labelID = strings.TrimSpace(labelID)
		if labelID == "" || seen[labelID] {
			continue
		}
		seen[labelID] = true
		label, ok := labelsByID[labelID]
		if !ok || gmailLabelIsSystem(label) || strings.TrimSpace(label.Name) == "" {
			continue
		}
		inputs = append(inputs, storage.LabelInput{
			AccountID:    accountID,
			Name:         strings.TrimSpace(label.Name),
			ProviderID:   labelID,
			ProviderType: storage.LabelProviderGmail,
		})
	}
	sort.SliceStable(inputs, func(i, j int) bool {
		return strings.ToLower(inputs[i].Name) < strings.ToLower(inputs[j].Name)
	})
	return inputs
}

func gmailLabelIsSystem(label gmailLabel) bool {
	return strings.EqualFold(strings.TrimSpace(label.Type), "system")
}

type outlookCategoriesResponse struct {
	Value []outlookCategory `json:"value"`
}

type outlookCategory struct {
	DisplayName string `json:"displayName"`
	Color       string `json:"color"`
}

type outlookMessagesResponse struct {
	Value []outlookMessageState `json:"value"`
}

type outlookMessageState struct {
	ID         string   `json:"id"`
	Categories []string `json:"categories"`
}

type providerBatchRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

type providerBatchResponse struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type providerBatchResponseEnvelope struct {
	Responses []providerBatchResponse `json:"responses"`
}

type outlookCategoryBatchResult struct {
	Failed   int
	NotFound int
	Skipped  int
	Synced   int
	With     int
	Without  int
}

type gmailLabelSyncResult struct {
	WithLabels bool
	HistoryID  string
	Skipped    bool
}

func (o *SyncOrchestrator) completeProviderLabelSyncRun(stats storage.LabelSyncRunStats, syncErr error) error {
	if o.db == nil {
		return syncErr
	}
	stats.FinishedAt = time.Now().UTC()
	if pending, err := o.db.CountLabelMutations(context.Background(), stats.AccountID, stats.ProviderType); err == nil {
		stats.PendingMutations = pending
	} else {
		log.Printf("provider label sync pending mutation count %s/%s: %v", stats.AccountID, stats.ProviderType, err)
	}
	if err := o.db.MarkLabelSyncRun(context.Background(), stats, syncErr); err != nil {
		if syncErr != nil {
			log.Printf("provider label sync run state %s/%s: %v", stats.AccountID, stats.ProviderType, err)
			return syncErr
		}
		return err
	}
	return syncErr
}

func providerMessageNotFound(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "message not found")
}

func (o *SyncOrchestrator) syncOutlookCategories(ctx context.Context, accountID string) (retErr error) {
	graphTokens, ok := o.tokenProvider.(graphMailTokenProvider)
	if !ok {
		return nil
	}
	stats := storage.LabelSyncRunStats{
		AccountID:    accountID,
		ProviderType: storage.LabelProviderOutlook,
		Scope:        "messages",
		StartedAt:    time.Now().UTC(),
		Full:         true,
	}
	defer func() {
		retErr = o.completeProviderLabelSyncRun(stats, retErr)
	}()
	token, err := graphTokens.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: %w", errProviderLabelAuth, err)
	}

	categoriesByName, err := o.syncOutlookCategoryCatalog(ctx, accountID, token)
	if err != nil {
		return err
	}

	var total outlookCategoryBatchResult
	for afterID := int64(0); ; {
		messages, err := o.db.ListProviderLabelSyncMessages(ctx, accountID, afterID, providerLabelSyncBatchSize)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			break
		}
		stats.TotalMessages += len(messages)
		for _, msg := range messages {
			afterID = msg.ID
		}
		result, err := o.syncOutlookMessageCategoriesBatch(ctx, token, messages, categoriesByName)
		total.Failed += result.Failed
		total.NotFound += result.NotFound
		total.Skipped += result.Skipped
		total.Synced += result.Synced
		total.With += result.With
		total.Without += result.Without
		stats.SyncedMessages += result.Synced
		stats.WithLabels += result.With
		stats.WithoutLabels += result.Without
		stats.MissingProviderMessages += result.NotFound
		stats.SkippedMessages += result.Skipped
		stats.FailedMessages += result.Failed
		if err != nil {
			if providerLabelSyncShouldStop(err) {
				return err
			}
			total.Failed++
			stats.FailedMessages++
			log.Printf("outlook category sync batch account=%s after=%d: %v", accountID, afterID, err)
		}
		if len(messages) < providerLabelSyncBatchSize {
			break
		}
	}
	o.replayOutlookLabelMutationQueue(ctx, accountID, token)
	if total.NotFound > 0 || total.Skipped > 0 {
		log.Printf("outlook category sync account=%s: %d message(s) not found in Graph, %d message(s) skipped without provider identity", accountID, total.NotFound, total.Skipped)
	}
	if total.Failed > 0 {
		return fmt.Errorf("%d Outlook message categorization import(s) failed", total.Failed)
	}
	return nil
}

func (o *SyncOrchestrator) syncOutlookCategoryCatalog(ctx context.Context, accountID, token string) (map[string]outlookCategory, error) {
	var response outlookCategoriesResponse
	if err := providerJSON(ctx, http.MethodGet, outlookGraphBaseURL+"/me/outlook/masterCategories", token, outlookImmutableIDHeaders(), nil, &response); err != nil {
		return nil, err
	}
	categoriesByName := make(map[string]outlookCategory, len(response.Value))
	inputs := make([]storage.LabelInput, 0, len(response.Value))
	for _, category := range response.Value {
		category.DisplayName = strings.TrimSpace(category.DisplayName)
		if category.DisplayName == "" {
			continue
		}
		categoriesByName[strings.ToLower(category.DisplayName)] = category
		inputs = append(inputs, outlookCategoryLabelInput(accountID, category.DisplayName))
	}
	if len(inputs) > 0 {
		if err := o.db.UpsertLabels(ctx, inputs); err != nil {
			return nil, err
		}
	}
	return categoriesByName, nil
}

func (o *SyncOrchestrator) syncOutlookMessageCategories(ctx context.Context, token string, msg storage.ProviderLabelSyncMessage, categoriesByName map[string]outlookCategory) error {
	providerMessageID := strings.TrimSpace(msg.ProviderMessageID)
	if providerMessageID == "" {
		resolved, err := outlookMessageIDForInternetID(ctx, token, msg.InternetMessageID)
		if err != nil {
			return err
		}
		providerMessageID = resolved
		if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, providerMessageID); err != nil {
			log.Printf("cache outlook message id failed: %v", err)
		}
	}

	state, err := getOutlookMessageState(ctx, token, providerMessageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(state.ID) != "" && state.ID != providerMessageID {
		if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, strings.TrimSpace(state.ID)); err != nil {
			log.Printf("cache outlook message id failed: %v", err)
		}
	}
	return o.db.ReplaceMessageLabelsForProvider(ctx, msg.ID, msg.AccountID, storage.LabelProviderOutlook, outlookCategoryLabelInputs(msg.AccountID, state.Categories, categoriesByName))
}

func (o *SyncOrchestrator) syncOutlookMessageCategoriesBatch(ctx context.Context, token string, messages []storage.ProviderLabelSyncMessage, categoriesByName map[string]outlookCategory) (outlookCategoryBatchResult, error) {
	var result outlookCategoryBatchResult
	for start := 0; start < len(messages); start += outlookProviderBatchSize {
		end := start + outlookProviderBatchSize
		if end > len(messages) {
			end = len(messages)
		}
		batchResult, err := o.syncOutlookMessageCategoriesBatchChunk(ctx, token, messages[start:end], categoriesByName)
		result.Failed += batchResult.Failed
		result.NotFound += batchResult.NotFound
		result.Skipped += batchResult.Skipped
		result.Synced += batchResult.Synced
		result.With += batchResult.With
		result.Without += batchResult.Without
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (o *SyncOrchestrator) syncOutlookMessageCategoriesBatchChunk(ctx context.Context, token string, messages []storage.ProviderLabelSyncMessage, categoriesByName map[string]outlookCategory) (outlookCategoryBatchResult, error) {
	var result outlookCategoryBatchResult
	requests := make([]providerBatchRequest, 0, len(messages))
	byRequestID := make(map[string]storage.ProviderLabelSyncMessage, len(messages))
	lookupByRequestID := make(map[string]bool, len(messages))
	for _, msg := range messages {
		requestID := strconv.FormatInt(msg.ID, 10)
		requestURL, lookup, ok := outlookCategoryBatchRequestURL(msg)
		if !ok {
			result.Skipped++
			continue
		}
		requests = append(requests, providerBatchRequest{
			ID:      requestID,
			Method:  http.MethodGet,
			URL:     requestURL,
			Headers: outlookImmutableIDHeaders(),
		})
		byRequestID[requestID] = msg
		lookupByRequestID[requestID] = lookup
	}
	if len(requests) == 0 {
		return result, nil
	}

	var envelope providerBatchResponseEnvelope
	if err := providerJSON(ctx, http.MethodPost, outlookGraphBaseURL+"/$batch", token, nil, map[string][]providerBatchRequest{"requests": requests}, &envelope); err != nil {
		return result, err
	}
	seen := make(map[string]bool, len(envelope.Responses))
	for _, response := range envelope.Responses {
		msg, ok := byRequestID[response.ID]
		if !ok {
			continue
		}
		seen[response.ID] = true
		if response.Status == http.StatusNotFound {
			result.NotFound++
			continue
		}
		if response.Status < 200 || response.Status >= 300 {
			if response.Status == http.StatusUnauthorized ||
				response.Status == http.StatusForbidden ||
				response.Status == http.StatusTooManyRequests ||
				response.Status >= http.StatusInternalServerError {
				return result, &providerAPIError{StatusCode: response.Status, Body: strings.TrimSpace(string(response.Body))}
			}
			result.Failed++
			continue
		}

		state, found, err := outlookMessageStateFromBatchResponse(response.Body, lookupByRequestID[response.ID])
		if err != nil {
			result.Failed++
			log.Printf("outlook category sync message account=%s message=%d: decode batch response: %v", msg.AccountID, msg.ID, err)
			continue
		}
		if !found {
			result.NotFound++
			continue
		}
		if strings.TrimSpace(state.ID) != "" && strings.TrimSpace(state.ID) != strings.TrimSpace(msg.ProviderMessageID) {
			if err := o.db.SetMessageProviderMessageID(ctx, msg.ID, strings.TrimSpace(state.ID)); err != nil {
				log.Printf("cache outlook message id failed: %v", err)
			}
		}
		labels := outlookCategoryLabelInputs(msg.AccountID, state.Categories, categoriesByName)
		if err := o.db.ReplaceMessageLabelsForProvider(ctx, msg.ID, msg.AccountID, storage.LabelProviderOutlook, labels); err != nil {
			return result, err
		}
		result.Synced++
		if len(labels) > 0 {
			result.With++
		} else {
			result.Without++
		}
	}
	for requestID := range byRequestID {
		if !seen[requestID] {
			result.Failed++
		}
	}
	return result, nil
}

func outlookCategoryBatchRequestURL(msg storage.ProviderLabelSyncMessage) (string, bool, bool) {
	providerMessageID := strings.TrimSpace(msg.ProviderMessageID)
	if providerMessageID != "" {
		return "/me/messages/" + url.PathEscape(providerMessageID) + "?$select=id,categories", false, true
	}
	internetMessageID := strings.TrimSpace(msg.InternetMessageID)
	if internetMessageID == "" || isSyntheticGoferMessageID(internetMessageID) {
		return "", false, false
	}
	values := url.Values{}
	values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetMessageID, "'", "''")+"'")
	values.Set("$select", "id,internetMessageId,categories")
	values.Set("$top", "1")
	return "/me/messages?" + values.Encode(), true, true
}

func outlookMessageStateFromBatchResponse(raw json.RawMessage, lookup bool) (outlookMessageState, bool, error) {
	if lookup {
		var response outlookMessagesResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return outlookMessageState{}, false, err
		}
		if len(response.Value) == 0 || strings.TrimSpace(response.Value[0].ID) == "" {
			return outlookMessageState{}, false, nil
		}
		return response.Value[0], true, nil
	}
	var state outlookMessageState
	if err := json.Unmarshal(raw, &state); err != nil {
		return outlookMessageState{}, false, err
	}
	if strings.TrimSpace(state.ID) == "" {
		return outlookMessageState{}, false, nil
	}
	return state, true, nil
}

func isSyntheticGoferMessageID(messageID string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(messageID)), "<>")
	return strings.HasSuffix(normalized, "@sync.gofer")
}

func outlookMessageIDForInternetID(ctx context.Context, token, internetMessageID string) (string, error) {
	internetMessageID = strings.TrimSpace(internetMessageID)
	if internetMessageID == "" {
		return "", fmt.Errorf("message has no internet message id")
	}
	values := url.Values{}
	values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetMessageID, "'", "''")+"'")
	values.Set("$select", "id,internetMessageId,categories")
	values.Set("$top", "1")

	var response outlookMessagesResponse
	endpoint := outlookGraphBaseURL + "/me/messages?" + values.Encode()
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookImmutableIDHeaders(), nil, &response); err != nil {
		return "", err
	}
	if len(response.Value) == 0 || strings.TrimSpace(response.Value[0].ID) == "" {
		return "", fmt.Errorf("outlook message not found")
	}
	return strings.TrimSpace(response.Value[0].ID), nil
}

func getOutlookMessageState(ctx context.Context, token, providerMessageID string) (outlookMessageState, error) {
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "?$select=id,categories"
	var state outlookMessageState
	if err := providerJSON(ctx, http.MethodGet, endpoint, token, outlookImmutableIDHeaders(), nil, &state); err != nil {
		return outlookMessageState{}, err
	}
	return state, nil
}

func outlookCategoryLabelInputs(accountID string, names []string, categoriesByName map[string]outlookCategory) []storage.LabelInput {
	inputs := make([]storage.LabelInput, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		category, ok := categoriesByName[key]
		if ok && strings.TrimSpace(category.DisplayName) != "" {
			name = strings.TrimSpace(category.DisplayName)
		}
		inputs = append(inputs, outlookCategoryLabelInput(accountID, name))
	}
	sort.SliceStable(inputs, func(i, j int) bool {
		return strings.ToLower(inputs[i].Name) < strings.ToLower(inputs[j].Name)
	})
	return inputs
}

func outlookCategoryLabelInput(accountID, name string) storage.LabelInput {
	name = strings.TrimSpace(name)
	return storage.LabelInput{
		AccountID:    accountID,
		Name:         name,
		ProviderID:   name,
		ProviderType: storage.LabelProviderOutlook,
	}
}

func outlookImmutableIDHeaders() map[string]string {
	return map[string]string{"Prefer": `IdType="ImmutableId"`}
}

func (o *SyncOrchestrator) replayGmailLabelMutationQueue(ctx context.Context, accountID, token string) {
	entries, err := o.db.ListDueLabelMutations(ctx, accountID, storage.LabelProviderGmail, providerLabelMutationReplayLimit)
	if err != nil {
		log.Printf("gmail label mutation queue list account=%s: %v", accountID, err)
		return
	}
	for _, entry := range entries {
		if err := o.applyQueuedGmailLabelMutation(ctx, token, entry); err != nil {
			log.Printf("gmail label mutation replay account=%s message=%d label=%q op=%s: %v", entry.AccountID, entry.MessageID, entry.LabelName, entry.Operation, err)
			if markErr := o.db.MarkLabelMutationError(ctx, entry.ID, entry.Attempts, err); markErr != nil {
				log.Printf("gmail label mutation queue mark error id=%d: %v", entry.ID, markErr)
			}
			continue
		}
		if err := o.db.MarkLabelMutationSuccess(ctx, entry.ID); err != nil {
			log.Printf("gmail label mutation queue mark success id=%d: %v", entry.ID, err)
		}
	}
}

func (o *SyncOrchestrator) applyQueuedGmailLabelMutation(ctx context.Context, token string, entry storage.LabelMutationQueueEntry) error {
	info, err := o.queuedMessageMutationInfo(ctx, entry)
	if err != nil || info == nil {
		return err
	}
	providerMessageID, err := o.queuedGmailMessageID(ctx, token, entry.MessageID, info)
	if err != nil {
		return err
	}

	switch entry.Operation {
	case storage.LabelMutationAdd:
		label, err := ensureGmailProviderLabel(ctx, token, entry.LabelName)
		if err != nil {
			return err
		}
		endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/modify"
		if err := providerJSON(ctx, http.MethodPost, endpoint, token, nil, map[string][]string{"addLabelIds": []string{label.ID}}, nil); err != nil {
			return err
		}
		if _, err := o.db.AddMessageLabel(ctx, entry.MessageID, entry.AccountID, storage.LabelInput{
			AccountID:    entry.AccountID,
			Name:         label.Name,
			ProviderID:   label.ID,
			ProviderType: storage.LabelProviderGmail,
			IsSystem:     gmailLabelIsSystem(label),
		}); err != nil {
			return err
		}
		return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
	case storage.LabelMutationRemove:
		label, ok, err := findGmailProviderLabel(ctx, token, entry.LabelName)
		if err != nil {
			return err
		}
		if ok {
			endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/modify"
			if err := providerJSON(ctx, http.MethodPost, endpoint, token, nil, map[string][]string{"removeLabelIds": []string{label.ID}}, nil); err != nil {
				return err
			}
			if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderGmail, label.ID, label.Name); err != nil {
				return err
			}
		} else if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderGmail, "", entry.LabelName); err != nil {
			return err
		}
		return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
	default:
		return fmt.Errorf("unsupported label mutation operation %q", entry.Operation)
	}
}

func (o *SyncOrchestrator) queuedGmailMessageID(ctx context.Context, token string, messageID int64, info *storage.MessageMutationInfo) (string, error) {
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID != "" {
		return providerMessageID, nil
	}
	resolved, err := gmailMessageIDForInternetID(ctx, token, info.InternetMessageID)
	if err != nil {
		return "", err
	}
	if err := o.db.SetMessageProviderMessageID(ctx, messageID, resolved); err != nil {
		log.Printf("cache gmail message id failed: %v", err)
	}
	return resolved, nil
}

func ensureGmailProviderLabel(ctx context.Context, token, labelName string) (gmailLabel, error) {
	label, ok, err := findGmailProviderLabel(ctx, token, labelName)
	if err != nil || ok {
		return label, err
	}
	labelName = strings.TrimSpace(labelName)
	var created gmailLabel
	payload := map[string]string{
		"name":                  labelName,
		"labelListVisibility":   "labelShow",
		"messageListVisibility": "show",
	}
	if err := providerJSON(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/labels", token, nil, payload, &created); err != nil {
		return gmailLabel{}, err
	}
	if strings.TrimSpace(created.ID) == "" {
		return gmailLabel{}, fmt.Errorf("gmail label create returned no id")
	}
	if strings.TrimSpace(created.Name) == "" {
		created.Name = labelName
	}
	return created, nil
}

func findGmailProviderLabel(ctx context.Context, token, labelName string) (gmailLabel, bool, error) {
	labelName = strings.TrimSpace(labelName)
	var response gmailLabelsResponse
	if err := providerJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", token, nil, nil, &response); err != nil {
		return gmailLabel{}, false, err
	}
	for _, label := range response.Labels {
		if strings.EqualFold(strings.TrimSpace(label.Name), labelName) && strings.TrimSpace(label.ID) != "" {
			return label, true, nil
		}
	}
	return gmailLabel{}, false, nil
}

func (o *SyncOrchestrator) replayOutlookLabelMutationQueue(ctx context.Context, accountID, token string) {
	entries, err := o.db.ListDueLabelMutations(ctx, accountID, storage.LabelProviderOutlook, providerLabelMutationReplayLimit)
	if err != nil {
		log.Printf("outlook label mutation queue list account=%s: %v", accountID, err)
		return
	}
	for _, entry := range entries {
		if err := o.applyQueuedOutlookLabelMutation(ctx, token, entry); err != nil {
			log.Printf("outlook label mutation replay account=%s message=%d label=%q op=%s: %v", entry.AccountID, entry.MessageID, entry.LabelName, entry.Operation, err)
			if markErr := o.db.MarkLabelMutationError(ctx, entry.ID, entry.Attempts, err); markErr != nil {
				log.Printf("outlook label mutation queue mark error id=%d: %v", entry.ID, markErr)
			}
			continue
		}
		if err := o.db.MarkLabelMutationSuccess(ctx, entry.ID); err != nil {
			log.Printf("outlook label mutation queue mark success id=%d: %v", entry.ID, err)
		}
	}
}

func (o *SyncOrchestrator) applyQueuedOutlookLabelMutation(ctx context.Context, token string, entry storage.LabelMutationQueueEntry) error {
	info, err := o.queuedMessageMutationInfo(ctx, entry)
	if err != nil || info == nil {
		return err
	}
	providerMessageID, err := o.queuedOutlookMessageID(ctx, token, entry.MessageID, info)
	if err != nil {
		return err
	}

	state, err := getOutlookMessageState(ctx, token, providerMessageID)
	if err != nil {
		return err
	}
	categories := append([]string(nil), state.Categories...)
	switch entry.Operation {
	case storage.LabelMutationAdd:
		if err := ensureOutlookProviderCategory(ctx, token, entry.LabelName); err != nil {
			return err
		}
		categories = appendUniqueFold(categories, entry.LabelName)
		sort.SliceStable(categories, func(i, j int) bool {
			return strings.ToLower(categories[i]) < strings.ToLower(categories[j])
		})
	case storage.LabelMutationRemove:
		var removed bool
		categories, removed = removeFold(categories, entry.LabelName)
		if !removed {
			if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderOutlook, "", entry.LabelName); err != nil {
				return err
			}
			return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
		}
	default:
		return fmt.Errorf("unsupported label mutation operation %q", entry.Operation)
	}

	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	if err := providerJSON(ctx, http.MethodPatch, endpoint, token, outlookImmutableIDHeaders(), map[string][]string{"categories": categories}, nil); err != nil {
		return err
	}
	if entry.Operation == storage.LabelMutationAdd {
		if _, err := o.db.AddMessageLabel(ctx, entry.MessageID, entry.AccountID, outlookCategoryLabelInput(entry.AccountID, entry.LabelName)); err != nil {
			return err
		}
		return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
	}
	if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderOutlook, entry.LabelName, entry.LabelName); err != nil {
		return err
	}
	return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
}

func (o *SyncOrchestrator) queuedOutlookMessageID(ctx context.Context, token string, messageID int64, info *storage.MessageMutationInfo) (string, error) {
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID != "" {
		return providerMessageID, nil
	}
	resolved, err := outlookMessageIDForInternetID(ctx, token, info.InternetMessageID)
	if err != nil {
		return "", err
	}
	if err := o.db.SetMessageProviderMessageID(ctx, messageID, resolved); err != nil {
		log.Printf("cache outlook message id failed: %v", err)
	}
	return resolved, nil
}

func ensureOutlookProviderCategory(ctx context.Context, token, labelName string) error {
	var response outlookCategoriesResponse
	if err := providerJSON(ctx, http.MethodGet, outlookGraphBaseURL+"/me/outlook/masterCategories", token, outlookImmutableIDHeaders(), nil, &response); err != nil {
		return err
	}
	for _, category := range response.Value {
		if strings.EqualFold(strings.TrimSpace(category.DisplayName), strings.TrimSpace(labelName)) {
			return nil
		}
	}
	return providerJSON(ctx, http.MethodPost, outlookGraphBaseURL+"/me/outlook/masterCategories", token, outlookImmutableIDHeaders(), map[string]string{
		"displayName": strings.TrimSpace(labelName),
		"color":       "preset0",
	}, nil)
}

func (o *SyncOrchestrator) replayIMAPLabelMutationQueue(ctx context.Context, accountID string) {
	if o.db == nil || o.accountStore == nil {
		return
	}
	entries, err := o.db.ListDueLabelMutations(ctx, accountID, storage.LabelProviderIMAPKeyword, providerLabelMutationReplayLimit)
	if err != nil {
		log.Printf("imap label mutation queue list account=%s: %v", accountID, err)
		return
	}
	if len(entries) == 0 {
		return
	}

	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		o.markLabelMutationBatchError(ctx, entries, err)
		return
	}
	password, err := o.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		o.markLabelMutationBatchError(ctx, entries, err)
		return
	}
	client, err := mailimap.NewClient(ctx, cfg, password)
	if err != nil {
		o.markLabelMutationBatchError(ctx, entries, err)
		return
	}
	defer client.Close()

	for _, entry := range entries {
		if err := o.applyQueuedIMAPLabelMutation(ctx, client, entry); err != nil {
			log.Printf("imap label mutation replay account=%s message=%d label=%q op=%s: %v", entry.AccountID, entry.MessageID, entry.LabelName, entry.Operation, err)
			if markErr := o.db.MarkLabelMutationError(ctx, entry.ID, entry.Attempts, err); markErr != nil {
				log.Printf("imap label mutation queue mark error id=%d: %v", entry.ID, markErr)
			}
			continue
		}
		if err := o.db.MarkLabelMutationSuccess(ctx, entry.ID); err != nil {
			log.Printf("imap label mutation queue mark success id=%d: %v", entry.ID, err)
		}
	}
}

func (o *SyncOrchestrator) applyQueuedIMAPLabelMutation(ctx context.Context, client *mailimap.Client, entry storage.LabelMutationQueueEntry) error {
	info, err := o.queuedMessageMutationInfo(ctx, entry)
	if err != nil || info == nil {
		return err
	}
	keyword, err := imapKeywordFromLabel(entry.LabelName)
	if err != nil {
		return err
	}
	remoteName := strings.TrimSpace(info.FolderRemoteID)
	if remoteName == "" {
		return fmt.Errorf("message has no remote IMAP folder identity")
	}
	uid := info.RemoteUID
	if uid == 0 {
		uid, err = client.FindUIDByMessageID(ctx, remoteName, info.InternetMessageID)
		if err != nil {
			return err
		}
		if uid == 0 {
			return fmt.Errorf("message has no remote IMAP identity")
		}
	}

	switch entry.Operation {
	case storage.LabelMutationAdd:
		if err := client.StoreKeyword(ctx, remoteName, uid, goimap.StoreFlagsAdd, keyword); err != nil {
			return err
		}
		if _, err := o.db.AddMessageLabel(ctx, entry.MessageID, entry.AccountID, storage.LabelInput{
			AccountID:    entry.AccountID,
			Name:         keyword,
			ProviderID:   keyword,
			ProviderType: storage.LabelProviderIMAPKeyword,
		}); err != nil {
			return err
		}
		return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
	case storage.LabelMutationRemove:
		if err := client.StoreKeyword(ctx, remoteName, uid, goimap.StoreFlagsDel, keyword); err != nil {
			return err
		}
		if err := o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderIMAPKeyword, keyword, keyword); err != nil {
			return err
		}
		return o.db.RemoveMessageLabelForProvider(ctx, entry.MessageID, entry.AccountID, storage.LabelProviderLocal, "", entry.LabelName)
	default:
		return fmt.Errorf("unsupported label mutation operation %q", entry.Operation)
	}
}

func (o *SyncOrchestrator) queuedMessageMutationInfo(ctx context.Context, entry storage.LabelMutationQueueEntry) (*storage.MessageMutationInfo, error) {
	if strings.TrimSpace(entry.FolderID) != "" {
		info, err := o.db.GetMessageMutationInfoForFolder(ctx, entry.MessageID, entry.FolderID)
		if err != nil || info != nil {
			return info, err
		}
	}
	return o.db.GetMessageMutationInfo(ctx, entry.MessageID)
}

func (o *SyncOrchestrator) markLabelMutationBatchError(ctx context.Context, entries []storage.LabelMutationQueueEntry, err error) {
	for _, entry := range entries {
		if markErr := o.db.MarkLabelMutationError(ctx, entry.ID, entry.Attempts, err); markErr != nil {
			log.Printf("label mutation queue mark batch error id=%d: %v", entry.ID, markErr)
		}
	}
}

func imapKeywordFromLabel(labelName string) (string, error) {
	keyword := strings.TrimSpace(labelName)
	if keyword == "" {
		return "", fmt.Errorf("label is required")
	}
	if strings.HasPrefix(keyword, "\\") {
		return "", fmt.Errorf("label cannot be an IMAP system flag")
	}
	for _, r := range keyword {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "", fmt.Errorf("label %q is not a portable IMAP keyword", labelName)
	}
	return keyword, nil
}

func appendUniqueFold(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(strings.TrimSpace(existing), value) {
			return values
		}
	}
	return append(values, value)
}

func removeFold(values []string, value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return values, false
	}
	next := values[:0]
	removed := false
	for _, existing := range values {
		if strings.EqualFold(strings.TrimSpace(existing), value) {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	return next, removed
}

func providerJSON(ctx context.Context, method, endpoint, token string, headers map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return err
		}
		reader = buf
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &providerAPIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
