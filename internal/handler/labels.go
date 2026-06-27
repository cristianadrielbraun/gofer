package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	mailimap "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"

	goimap "github.com/emersion/go-imap/v2"
)

type labelMutationResult struct {
	Updated      int
	Messages     int
	Failed       int
	RemoteFailed int
}

func (h *Handler) handleLabelMessages(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	labelName := strings.TrimSpace(payload.Label)
	if labelName == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	ctx := context.WithoutCancel(r.Context())
	result := h.applyLabelToTargets(ctx, messageBulkTargets(payload), strings.TrimSpace(payload.FolderID), labelName)
	if result.Messages == 0 && result.Failed > 0 {
		http.Error(w, "label could not be applied", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{
		"updated":       result.Updated,
		"messages":      result.Messages,
		"failed":        result.Failed,
		"remote_failed": result.RemoteFailed,
	})
}

func (h *Handler) handleUnlabelMessages(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	labelName := strings.TrimSpace(payload.Label)
	if labelName == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	ctx := context.WithoutCancel(r.Context())
	result := h.removeLabelFromTargets(ctx, messageBulkTargets(payload), strings.TrimSpace(payload.FolderID), labelName)
	if result.Messages == 0 && result.Failed > 0 {
		http.Error(w, "label could not be removed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{
		"updated":       result.Updated,
		"messages":      result.Messages,
		"failed":        result.Failed,
		"remote_failed": result.RemoteFailed,
	})
}

func (h *Handler) handleLabelMessage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	labelName := strings.TrimSpace(r.FormValue("label"))
	if labelName == "" && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var payload struct {
			Label    string `json:"label"`
			FolderID string `json:"folder_id"`
			Thread   bool   `json:"thread"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			labelName = strings.TrimSpace(payload.Label)
			if r.Form.Get("folder_id") == "" {
				r.Form.Set("folder_id", payload.FolderID)
			}
			if payload.Thread {
				r.Form.Set("thread", "1")
			}
		}
	}
	if labelName == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	target := messageBulkTarget{
		ID:     strings.TrimSpace(r.PathValue("id")),
		Thread: r.FormValue("thread") == "1" || strings.EqualFold(r.FormValue("thread"), "true"),
	}
	result := h.applyLabelToTargets(context.WithoutCancel(r.Context()), []messageBulkTarget{target}, strings.TrimSpace(r.FormValue("folder_id")), labelName)
	if result.Messages == 0 && result.Failed > 0 {
		http.Error(w, "label could not be applied", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{
		"updated":       result.Updated,
		"messages":      result.Messages,
		"failed":        result.Failed,
		"remote_failed": result.RemoteFailed,
	})
}

func (h *Handler) handleUnlabelMessage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	labelName := strings.TrimSpace(r.FormValue("label"))
	if labelName == "" && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var payload struct {
			Label    string `json:"label"`
			FolderID string `json:"folder_id"`
			Thread   bool   `json:"thread"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			labelName = strings.TrimSpace(payload.Label)
			if r.Form.Get("folder_id") == "" {
				r.Form.Set("folder_id", payload.FolderID)
			}
			if payload.Thread {
				r.Form.Set("thread", "1")
			}
		}
	}
	if labelName == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	target := messageBulkTarget{
		ID:     strings.TrimSpace(r.PathValue("id")),
		Thread: r.FormValue("thread") == "1" || strings.EqualFold(r.FormValue("thread"), "true"),
	}
	result := h.removeLabelFromTargets(context.WithoutCancel(r.Context()), []messageBulkTarget{target}, strings.TrimSpace(r.FormValue("folder_id")), labelName)
	if result.Messages == 0 && result.Failed > 0 {
		http.Error(w, "label could not be removed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{
		"updated":       result.Updated,
		"messages":      result.Messages,
		"failed":        result.Failed,
		"remote_failed": result.RemoteFailed,
	})
}

func (h *Handler) applyLabelToTargets(ctx context.Context, targets []messageBulkTarget, sourceFolderID, labelName string) labelMutationResult {
	var result labelMutationResult
	for _, target := range targets {
		infos, err := h.spamTargetInfos(ctx, target, sourceFolderID)
		if err != nil || len(infos) == 0 {
			if err != nil {
				log.Printf("label target lookup failed: %v", err)
			}
			result.Failed++
			continue
		}

		targetUpdated := false
		for _, info := range infos {
			remoteLabel, remoteErr := h.applyMessageLabelRemote(ctx, info.MessageID, info.MessageMutationInfo, labelName)
			if remoteErr != nil {
				result.RemoteFailed++
				log.Printf("label remote apply failed for account=%s message=%d label=%q: %v", info.AccountID, info.MessageID, labelName, remoteErr)
				h.enqueueFailedLabelMutation(ctx, info.MessageID, info.MessageMutationInfo, storage.LabelMutationAdd, labelName, remoteErr)
				remoteLabel = storage.LabelInput{
					AccountID:    info.AccountID,
					Name:         labelName,
					ProviderType: storage.LabelProviderLocal,
				}
			}
			if _, err := h.db.AddMessageLabel(ctx, info.MessageID, info.AccountID, remoteLabel); err != nil {
				result.Failed++
				log.Printf("label local apply failed for account=%s message=%d label=%q: %v", info.AccountID, info.MessageID, labelName, err)
				continue
			}
			result.Messages++
			targetUpdated = true
		}
		if targetUpdated {
			result.Updated++
			h.publishThreadMutation(infos)
		}
	}
	return result
}

func (h *Handler) removeLabelFromTargets(ctx context.Context, targets []messageBulkTarget, sourceFolderID, labelName string) labelMutationResult {
	var result labelMutationResult
	for _, target := range targets {
		infos, err := h.spamTargetInfos(ctx, target, sourceFolderID)
		if err != nil || len(infos) == 0 {
			if err != nil {
				log.Printf("label target lookup failed: %v", err)
			}
			result.Failed++
			continue
		}

		targetUpdated := false
		for _, info := range infos {
			remoteLabel, remoteErr := h.removeMessageLabelRemote(ctx, info.MessageID, info.MessageMutationInfo, labelName)
			if remoteErr != nil {
				result.RemoteFailed++
				log.Printf("label remote remove failed for account=%s message=%d label=%q: %v", info.AccountID, info.MessageID, labelName, remoteErr)
				h.enqueueFailedLabelMutation(ctx, info.MessageID, info.MessageMutationInfo, storage.LabelMutationRemove, labelName, remoteErr)
				remoteLabel = storage.LabelInput{
					AccountID: info.AccountID,
					Name:      labelName,
				}
			}
			if err := h.db.RemoveMessageLabelForProvider(ctx, info.MessageID, info.AccountID, remoteLabel.ProviderType, remoteLabel.ProviderID, remoteLabel.Name); err != nil {
				result.Failed++
				log.Printf("label local remove failed for account=%s message=%d label=%q: %v", info.AccountID, info.MessageID, labelName, err)
				continue
			}
			result.Messages++
			targetUpdated = true
		}
		if targetUpdated {
			result.Updated++
			h.publishThreadMutation(infos)
		}
	}
	return result
}

func (h *Handler) enqueueFailedLabelMutation(ctx context.Context, messageID int64, info storage.MessageMutationInfo, operation, labelName string, mutationErr error) {
	providerType := labelProviderTypeForAccountProvider(info.AccountProvider)
	if providerType == "" || providerType == storage.LabelProviderLocal {
		return
	}
	if providerType == storage.LabelProviderIMAPKeyword {
		if _, err := h.imapKeywordForAccountLabel(ctx, info.AccountID, labelName); err != nil {
			return
		}
	}
	if err := h.db.EnqueueLabelMutation(ctx, info.AccountID, messageID, info.FolderID, providerType, operation, labelName, mutationErr); err != nil {
		log.Printf("label mutation queue enqueue failed for account=%s message=%d label=%q: %v", info.AccountID, messageID, labelName, err)
	}
}

func labelProviderTypeForAccountProvider(accountProvider string) string {
	switch strings.ToLower(strings.TrimSpace(accountProvider)) {
	case providers.ProviderGmail:
		return storage.LabelProviderGmail
	case providers.ProviderOutlook:
		return storage.LabelProviderOutlook
	case "", storage.LabelProviderLocal:
		return storage.LabelProviderLocal
	default:
		return storage.LabelProviderIMAPKeyword
	}
}

func (h *Handler) applyMessageLabelRemote(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	switch strings.ToLower(strings.TrimSpace(info.AccountProvider)) {
	case providers.ProviderGmail:
		return h.applyGmailMessageLabel(ctx, messageID, info, labelName)
	case providers.ProviderOutlook:
		return h.applyOutlookMessageLabel(ctx, messageID, info, labelName)
	default:
		return h.applyIMAPMessageKeyword(ctx, info, labelName)
	}
}

func (h *Handler) removeMessageLabelRemote(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	switch strings.ToLower(strings.TrimSpace(info.AccountProvider)) {
	case providers.ProviderGmail:
		return h.removeGmailMessageLabel(ctx, messageID, info, labelName)
	case providers.ProviderOutlook:
		return h.removeOutlookMessageLabel(ctx, messageID, info, labelName)
	default:
		return h.removeIMAPMessageKeyword(ctx, messageID, info, labelName)
	}
}

type gmailLabelsResponse struct {
	Labels []gmailLabel `json:"labels"`
}

type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

func (h *Handler) applyGmailMessageLabel(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	if h.auth == nil {
		return storage.LabelInput{}, fmt.Errorf("google oauth not configured")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	label, err := h.ensureGmailLabel(ctx, token, labelName)
	if err != nil {
		return storage.LabelInput{}, err
	}

	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, messageID, info)
		if err != nil {
			return storage.LabelInput{}, err
		}
	}

	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/modify"
	if err := doGoogleJSON(ctx, http.MethodPost, endpoint, token, map[string][]string{"addLabelIds": []string{label.ID}}, nil); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         label.Name,
		ProviderID:   label.ID,
		ProviderType: storage.LabelProviderGmail,
		IsSystem:     strings.EqualFold(label.Type, "system"),
	}, nil
}

func (h *Handler) ensureGmailLabel(ctx context.Context, token, labelName string) (gmailLabel, error) {
	labelName = strings.TrimSpace(labelName)
	var response gmailLabelsResponse
	if err := doGoogleJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", token, nil, &response); err != nil {
		return gmailLabel{}, err
	}
	for _, label := range response.Labels {
		if strings.EqualFold(strings.TrimSpace(label.Name), labelName) && strings.TrimSpace(label.ID) != "" {
			return label, nil
		}
	}

	var created gmailLabel
	payload := map[string]string{
		"name":                  labelName,
		"labelListVisibility":   "labelShow",
		"messageListVisibility": "show",
	}
	if err := doGoogleJSON(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/labels", token, payload, &created); err != nil {
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

func (h *Handler) findGmailLabel(ctx context.Context, token, labelName string) (gmailLabel, bool, error) {
	labelName = strings.TrimSpace(labelName)
	var response gmailLabelsResponse
	if err := doGoogleJSON(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", token, nil, &response); err != nil {
		return gmailLabel{}, false, err
	}
	for _, label := range response.Labels {
		if strings.EqualFold(strings.TrimSpace(label.Name), labelName) && strings.TrimSpace(label.ID) != "" {
			return label, true, nil
		}
	}
	return gmailLabel{}, false, nil
}

func (h *Handler) removeGmailMessageLabel(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	if h.auth == nil {
		return storage.LabelInput{}, fmt.Errorf("google oauth not configured")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	label, ok, err := h.findGmailLabel(ctx, token, labelName)
	if err != nil {
		return storage.LabelInput{}, err
	}
	if !ok {
		return storage.LabelInput{
			AccountID:    info.AccountID,
			Name:         labelName,
			ProviderType: storage.LabelProviderGmail,
		}, nil
	}

	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, messageID, info)
		if err != nil {
			return storage.LabelInput{}, err
		}
	}

	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/modify"
	if err := doGoogleJSON(ctx, http.MethodPost, endpoint, token, map[string][]string{"removeLabelIds": []string{label.ID}}, nil); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         label.Name,
		ProviderID:   label.ID,
		ProviderType: storage.LabelProviderGmail,
		IsSystem:     strings.EqualFold(label.Type, "system"),
	}, nil
}

type outlookCategoriesResponse struct {
	Value []outlookCategory `json:"value"`
}

type outlookCategory struct {
	DisplayName string `json:"displayName"`
	Color       string `json:"color"`
}

type outlookMessageCategoryState struct {
	ID         string   `json:"id"`
	Categories []string `json:"categories"`
}

func (h *Handler) applyOutlookMessageLabel(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	if h.auth == nil {
		return storage.LabelInput{}, fmt.Errorf("microsoft oauth not configured")
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, messageID, info)
		if err != nil {
			return storage.LabelInput{}, err
		}
	}
	if err := h.ensureOutlookCategory(ctx, token, labelName); err != nil {
		return storage.LabelInput{}, err
	}

	categories, err := h.getOutlookMessageCategories(ctx, token, providerMessageID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	categories = appendUniqueFold(categories, labelName)
	sort.SliceStable(categories, func(i, j int) bool {
		return strings.ToLower(categories[i]) < strings.ToLower(categories[j])
	})

	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	if err := h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string][]string{"categories": categories}, nil); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         labelName,
		ProviderID:   labelName,
		ProviderType: storage.LabelProviderOutlook,
	}, nil
}

func (h *Handler) ensureOutlookCategory(ctx context.Context, token, labelName string) error {
	var response outlookCategoriesResponse
	if err := h.doOutlookJSON(ctx, http.MethodGet, outlookGraphBaseURL+"/me/outlook/masterCategories", token, nil, &response); err != nil {
		return err
	}
	for _, category := range response.Value {
		if strings.EqualFold(strings.TrimSpace(category.DisplayName), strings.TrimSpace(labelName)) {
			return nil
		}
	}
	return h.doOutlookJSON(ctx, http.MethodPost, outlookGraphBaseURL+"/me/outlook/masterCategories", token, map[string]string{
		"displayName": labelName,
		"color":       "preset0",
	}, nil)
}

func (h *Handler) getOutlookMessageCategories(ctx context.Context, token, providerMessageID string) ([]string, error) {
	var state outlookMessageCategoryState
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "?$select=id,categories"
	if err := h.doOutlookJSON(ctx, http.MethodGet, endpoint, token, nil, &state); err != nil {
		return nil, err
	}
	return state.Categories, nil
}

func (h *Handler) removeOutlookMessageLabel(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	if h.auth == nil {
		return storage.LabelInput{}, fmt.Errorf("microsoft oauth not configured")
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, messageID, info)
		if err != nil {
			return storage.LabelInput{}, err
		}
	}

	categories, err := h.getOutlookMessageCategories(ctx, token, providerMessageID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	next, removed := removeFold(categories, labelName)
	if removed {
		endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
		if err := h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string][]string{"categories": next}, nil); err != nil {
			return storage.LabelInput{}, err
		}
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         labelName,
		ProviderID:   labelName,
		ProviderType: storage.LabelProviderOutlook,
	}, nil
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

func (h *Handler) applyIMAPMessageKeyword(ctx context.Context, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	keyword, err := h.imapKeywordForAccountLabel(ctx, info.AccountID, labelName)
	if err != nil {
		return storage.LabelInput{}, err
	}
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return storage.LabelInput{}, fmt.Errorf("message has no remote IMAP folder identity")
	}
	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	defer client.Close()

	uid := info.RemoteUID
	if uid == 0 {
		uid, err = client.FindUIDByMessageID(ctx, sourceRemoteID, info.InternetMessageID)
		if err != nil {
			return storage.LabelInput{}, err
		}
		if uid == 0 {
			return storage.LabelInput{}, fmt.Errorf("message has no remote IMAP identity")
		}
	}
	if err := client.StoreKeyword(ctx, sourceRemoteID, uid, goimap.StoreFlagsAdd, keyword); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         labelName,
		ProviderID:   keyword,
		ProviderType: storage.LabelProviderIMAPKeyword,
	}, nil
}

func (h *Handler) removeIMAPMessageKeyword(ctx context.Context, messageID int64, info storage.MessageMutationInfo, labelName string) (storage.LabelInput, error) {
	keyword, err := h.imapKeywordForMessageLabel(ctx, messageID, info.AccountID, labelName)
	if err != nil {
		return storage.LabelInput{}, err
	}
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return storage.LabelInput{}, fmt.Errorf("message has no remote IMAP folder identity")
	}
	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return storage.LabelInput{}, err
	}
	defer client.Close()

	uid := info.RemoteUID
	if uid == 0 {
		uid, err = client.FindUIDByMessageID(ctx, sourceRemoteID, info.InternetMessageID)
		if err != nil {
			return storage.LabelInput{}, err
		}
		if uid == 0 {
			return storage.LabelInput{}, fmt.Errorf("message has no remote IMAP identity")
		}
	}
	if err := client.StoreKeyword(ctx, sourceRemoteID, uid, goimap.StoreFlagsDel, keyword); err != nil {
		return storage.LabelInput{}, err
	}
	return storage.LabelInput{
		AccountID:    info.AccountID,
		Name:         labelName,
		ProviderID:   keyword,
		ProviderType: storage.LabelProviderIMAPKeyword,
	}, nil
}

func (h *Handler) imapKeywordForAccountLabel(ctx context.Context, accountID, labelName string) (string, error) {
	if h.db != nil {
		providerID, ok, err := h.db.ResolveLabelAliasProviderID(ctx, accountID, storage.LabelProviderIMAPKeyword, labelName)
		if err != nil {
			return "", err
		}
		if ok {
			return mailimap.ValidateKeyword(providerID)
		}
	}
	return imapKeywordFromLabel(labelName)
}

func (h *Handler) imapKeywordForMessageLabel(ctx context.Context, messageID int64, accountID, labelName string) (string, error) {
	if h.db != nil && messageID > 0 {
		labels, err := h.db.GetProviderMessageLabels(ctx, messageID, accountID, storage.LabelProviderIMAPKeyword)
		if err != nil {
			return "", err
		}
		for _, label := range labels {
			if !strings.EqualFold(strings.TrimSpace(label.Name), strings.TrimSpace(labelName)) {
				continue
			}
			keyword := strings.TrimSpace(label.ProviderID)
			if keyword == "" {
				keyword = strings.TrimSpace(label.Name)
			}
			return mailimap.ValidateKeyword(keyword)
		}
	}
	return h.imapKeywordForAccountLabel(ctx, accountID, labelName)
}

func imapKeywordFromLabel(labelName string) (string, error) {
	return mailimap.ValidateKeyword(labelName)
}
