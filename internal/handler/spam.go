package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"

	goimap "github.com/emersion/go-imap/v2"
)

var gmailAPIBaseURL = "https://gmail.googleapis.com/gmail/v1"

type spamDisposition string

const (
	spamDispositionSpam    spamDisposition = "spam"
	spamDispositionNotSpam spamDisposition = "not-spam"
)

func (h *Handler) handleMarkMessagesSpam(w http.ResponseWriter, r *http.Request) {
	h.handleMarkMessagesSpamState(w, r, spamDispositionSpam)
}

func (h *Handler) handleMarkMessagesNotSpam(w http.ResponseWriter, r *http.Request) {
	h.handleMarkMessagesSpamState(w, r, spamDispositionNotSpam)
}

func (h *Handler) handleMarkMessagesSpamState(w http.ResponseWriter, r *http.Request, disposition spamDisposition) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := context.WithoutCancel(r.Context())
	targets := messageBulkTargets(payload)
	sourceFolderID := strings.TrimSpace(payload.FolderID)
	updatedTargets := 0
	updatedMessages := 0
	failedMessages := 0
	remoteFailedMessages := 0
	var firstErr error

	for _, target := range targets {
		infos, err := h.spamTargetInfos(ctx, target, sourceFolderID)
		if err != nil || len(infos) == 0 {
			if err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}

		destFolderID, destRemoteID, err := h.spamDestinationFolder(ctx, infos[0].AccountID, disposition)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			failedMessages += len(infos)
			continue
		}

		targetUpdated := false
		for _, info := range infos {
			accountProvider := h.spamAccountProvider(ctx, info.MessageMutationInfo)
			destUID, remoteErr := h.reportMessageSpamRemote(ctx, disposition, info.MessageID, info.MessageMutationInfo, destRemoteID)
			if remoteErr != nil {
				remoteFailedMessages++
				if strings.EqualFold(accountProvider, providers.ProviderOutlook) {
					if firstErr == nil {
						firstErr = remoteErr
					}
					failedMessages++
					log.Printf("spam action %s Outlook Graph report failed for account=%s message=%d: %v", disposition, info.AccountID, info.MessageID, remoteErr)
					continue
				}
				log.Printf("spam action %s remote report failed for account=%s message=%d; falling back to local folder move: %v", disposition, info.AccountID, info.MessageID, remoteErr)
			}
			if err := h.moveMessageToSpamDestinationLocal(ctx, info, destFolderID, destUID); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				failedMessages++
				continue
			}
			targetUpdated = true
			updatedMessages++
		}

		if targetUpdated {
			updatedTargets++
			h.publishSpamMutation(infos, destFolderID)
		}
	}

	if updatedMessages == 0 && firstErr != nil {
		http.Error(w, firstErr.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{
		"updated":       updatedTargets,
		"messages":      updatedMessages,
		"failed":        failedMessages,
		"remote_failed": remoteFailedMessages,
	})
}

func (h *Handler) spamTargetInfos(ctx context.Context, target messageBulkTarget, sourceFolderID string) ([]storage.ThreadMessageMutationInfo, error) {
	if target.Thread {
		_, currentInfo, err := h.getMessageInfoForFolder(ctx, target.ID, sourceFolderID)
		if err != nil {
			return nil, err
		}
		email, err := h.db.GetEmailByID(ctx, target.ID)
		if err != nil {
			return nil, err
		}
		if email == nil || strings.TrimSpace(email.ThreadID) == "" {
			return nil, nil
		}
		return h.db.GetThreadMutationInfosForFolder(ctx, email.AccountID, email.ThreadID, currentInfo.FolderID)
	}

	msgID, info, err := h.getMessageInfoForFolder(ctx, target.ID, sourceFolderID)
	if err != nil {
		return nil, err
	}
	isRead, isStarred := h.messageFolderState(ctx, msgID, info.FolderID)
	return []storage.ThreadMessageMutationInfo{{
		MessageID:           msgID,
		MessageMutationInfo: *info,
		IsRead:              isRead,
		IsStarred:           isStarred,
	}}, nil
}

func (h *Handler) messageFolderState(ctx context.Context, messageID int64, folderID string) (bool, bool) {
	states, err := h.db.GetMessageAllFolderStates(ctx, messageID)
	if err != nil {
		return false, false
	}
	for _, state := range states {
		if state.FolderID == folderID {
			return state.IsRead, state.IsStarred
		}
	}
	return false, false
}

func (h *Handler) spamDestinationFolder(ctx context.Context, accountID string, disposition spamDisposition) (string, string, error) {
	if disposition == spamDispositionNotSpam {
		folderID, remoteID, err := h.db.GetFolderIDByRole(ctx, accountID, "inbox")
		if err != nil {
			return "", "", err
		}
		if folderID == "" {
			return "", "", fmt.Errorf("no inbox folder found")
		}
		return folderID, remoteID, nil
	}

	for _, role := range []string{"junk", "spam"} {
		folderID, remoteID, err := h.db.GetFolderIDByRole(ctx, accountID, role)
		if err != nil {
			return "", "", err
		}
		if folderID != "" {
			return folderID, remoteID, nil
		}
	}
	return "", "", fmt.Errorf("no spam folder found")
}

func (h *Handler) spamAccountProvider(ctx context.Context, info storage.MessageMutationInfo) string {
	provider := strings.TrimSpace(info.AccountProvider)
	if provider != "" || h.db == nil || strings.TrimSpace(info.AccountID) == "" {
		return provider
	}
	resolved, err := h.db.GetAccountProvider(ctx, info.AccountID)
	if err != nil {
		return provider
	}
	return strings.TrimSpace(resolved)
}

func (h *Handler) reportMessageSpamRemote(ctx context.Context, disposition spamDisposition, messageID int64, info storage.MessageMutationInfo, destRemoteID string) (uint32, error) {
	var providerErr error
	switch strings.ToLower(h.spamAccountProvider(ctx, info)) {
	case providers.ProviderGmail:
		if err := h.reportGmailMessageSpam(ctx, disposition, messageID, info); err == nil {
			return 0, nil
		} else {
			providerErr = err
			if gmailAPIMailRuntimeEnabled() {
				return 0, providerErr
			}
		}
	case providers.ProviderOutlook:
		if err := h.reportOutlookMessageSpam(ctx, disposition, messageID, info); err == nil {
			return 0, nil
		} else {
			return 0, err
		}
	}

	destUID, err := h.reportIMAPMessageSpam(ctx, disposition, info, destRemoteID)
	if err != nil {
		if providerErr != nil {
			return 0, fmt.Errorf("provider spam action failed: %v; imap fallback failed: %w", providerErr, err)
		}
		return 0, err
	}
	return destUID, nil
}

func (h *Handler) reportGmailMessageSpam(ctx context.Context, disposition spamDisposition, messageID int64, info storage.MessageMutationInfo) error {
	if h.auth == nil {
		return fmt.Errorf("google oauth not configured")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return err
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, messageID, info)
		if err != nil {
			return err
		}
	}

	payload := map[string][]string{}
	if disposition == spamDispositionNotSpam {
		payload["addLabelIds"] = []string{"INBOX"}
		payload["removeLabelIds"] = []string{"SPAM"}
	} else {
		payload["addLabelIds"] = []string{"SPAM"}
		payload["removeLabelIds"] = []string{"INBOX"}
	}

	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/modify"
	return doGoogleJSON(ctx, http.MethodPost, endpoint, token, payload, nil)
}

func (h *Handler) resolveGmailMessageID(ctx context.Context, token string, messageID int64, info storage.MessageMutationInfo) (string, error) {
	internetMessageID := strings.TrimSpace(info.InternetMessageID)
	if internetMessageID == "" {
		return "", fmt.Errorf("message has no internet message id")
	}

	values := url.Values{}
	values.Set("q", "rfc822msgid:"+internetMessageID)
	values.Set("includeSpamTrash", "true")
	values.Set("maxResults", "1")

	var response struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages?" + values.Encode()
	if err := doGoogleJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return "", err
	}
	if len(response.Messages) == 0 || strings.TrimSpace(response.Messages[0].ID) == "" {
		return "", fmt.Errorf("gmail message not found")
	}

	providerMessageID := strings.TrimSpace(response.Messages[0].ID)
	if err := h.db.SetMessageProviderMessageID(ctx, messageID, providerMessageID); err != nil {
		log.Printf("cache gmail message id failed: %v", err)
	}
	return providerMessageID, nil
}

func doGoogleJSON(ctx context.Context, method, endpoint, token string, body any, out any) error {
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
		return googleAPIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func googleAPIStatus(err error, status int) bool {
	if apiErr, ok := err.(googleAPIError); ok {
		return apiErr.Status == status
	}
	return false
}

func (h *Handler) reportOutlookMessageSpam(ctx context.Context, disposition spamDisposition, messageID int64, info storage.MessageMutationInfo) error {
	if h.auth == nil {
		return fmt.Errorf("microsoft oauth not configured")
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return err
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, messageID, info)
		if err != nil {
			return err
		}
	}

	destinationID := "junkemail"
	if disposition == spamDispositionNotSpam {
		destinationID = "inbox"
	}

	var moved struct {
		ID string `json:"id"`
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/move"
	if err := h.doOutlookJSON(ctx, http.MethodPost, endpoint, token, map[string]string{"destinationId": destinationID}, &moved); err != nil {
		return err
	}
	if strings.TrimSpace(moved.ID) != "" && strings.TrimSpace(moved.ID) != providerMessageID {
		if err := h.db.SetMessageProviderMessageID(ctx, messageID, strings.TrimSpace(moved.ID)); err != nil {
			log.Printf("cache outlook message id failed: %v", err)
		}
	}
	return nil
}

func (h *Handler) resolveOutlookMessageID(ctx context.Context, token string, messageID int64, info storage.MessageMutationInfo) (string, error) {
	internetMessageID := strings.TrimSpace(info.InternetMessageID)
	if internetMessageID == "" {
		return "", fmt.Errorf("message has no internet message id")
	}

	values := url.Values{}
	values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetMessageID, "'", "''")+"'")
	values.Set("$select", "id,internetMessageId")
	values.Set("$top", "1")

	var response struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	endpoint := outlookGraphBaseURL + "/me/messages?" + values.Encode()
	if err := h.doOutlookJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return "", err
	}
	if len(response.Value) == 0 || strings.TrimSpace(response.Value[0].ID) == "" {
		return "", fmt.Errorf("outlook message not found")
	}

	providerMessageID := strings.TrimSpace(response.Value[0].ID)
	if err := h.db.SetMessageProviderMessageID(ctx, messageID, providerMessageID); err != nil {
		log.Printf("cache outlook message id failed: %v", err)
	}
	return providerMessageID, nil
}

func (h *Handler) reportIMAPMessageSpam(ctx context.Context, disposition spamDisposition, info storage.MessageMutationInfo, destRemoteID string) (uint32, error) {
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return 0, fmt.Errorf("message has no remote IMAP identity; sync this folder before moving it again")
	}

	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	sourceUID := info.RemoteUID
	if sourceUID == 0 {
		sourceUID, err = client.FindUIDByMessageID(ctx, sourceRemoteID, info.InternetMessageID)
		if err != nil {
			return 0, err
		}
		if sourceUID == 0 {
			return 0, fmt.Errorf("message has no remote IMAP identity; sync this folder before moving it again")
		}
	}

	addFlag := goimap.FlagJunk
	removeFlag := goimap.FlagNotJunk
	if disposition == spamDispositionNotSpam {
		addFlag = goimap.FlagNotJunk
		removeFlag = goimap.FlagJunk
	}

	var flagErr error
	if err := client.StoreFlags(ctx, sourceRemoteID, sourceUID, goimap.StoreFlagsAdd, []goimap.Flag{addFlag}); err != nil {
		flagErr = err
	}
	if err := client.StoreFlags(ctx, sourceRemoteID, sourceUID, goimap.StoreFlagsDel, []goimap.Flag{removeFlag}); err != nil && flagErr == nil {
		flagErr = err
	}

	destRemoteID = strings.TrimSpace(destRemoteID)
	if destRemoteID == "" || destRemoteID == sourceRemoteID {
		return 0, flagErr
	}
	destUID, err := client.MoveMessageWithDestUID(ctx, sourceRemoteID, sourceUID, destRemoteID)
	if err != nil {
		return 0, err
	}
	return destUID, nil
}

func (h *Handler) moveMessageToSpamDestinationLocal(ctx context.Context, info storage.ThreadMessageMutationInfo, destFolderID string, destUID uint32) error {
	if strings.TrimSpace(destFolderID) == "" || info.FolderID == destFolderID {
		return nil
	}
	if err := h.db.RemoveMessageFromFolder(ctx, info.MessageID, info.FolderID); err != nil {
		return err
	}
	if destUID > 0 {
		return h.db.AddMessageToFolder(ctx, info.MessageID, destFolderID, destUID, info.IsRead, info.IsStarred)
	}
	return h.db.AddMessageToFolderWithoutRemoteUID(ctx, info.MessageID, destFolderID, info.IsRead, info.IsStarred)
}

func (h *Handler) publishSpamMutation(infos []storage.ThreadMessageMutationInfo, destFolderID string) {
	h.publishThreadMutation(infos)
	if len(infos) > 0 && strings.TrimSpace(destFolderID) != "" {
		h.publishMutation(infos[0].AccountID, destFolderID)
	}
}
