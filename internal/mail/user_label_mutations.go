package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	mailimap "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

// Label replay shares the account gate and session bounds with receiving and
// other mutations. It runs before moves so IMAP training flags follow the mail.
func (s *UserIMAP) replayLabelMutations(ctx context.Context, scope *userIMAPScope) error {
	provider, err := labelProviderForUserAccount(scope.config.Provider)
	if err != nil {
		return err
	}
	var failures error
	for i := 0; i < 25; i++ {
		var entries []storage.LabelMutationQueueEntry
		if err := scope.call(ctx, func(db *storage.DB) error {
			var err error
			entries, err = db.ListDueLabelMutations(ctx, scope.id, provider, 1)
			return err
		}); err != nil {
			return errors.Join(failures, err)
		}
		if len(entries) == 0 {
			return failures
		}
		entry := entries[0]
		work, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := s.applyUserLabelMutation(work, scope, entry)
		cancel()
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		if err != nil && !errors.Is(err, storage.ErrMessageMutationSuperseded) {
			if saveErr := scope.call(ctx, func(db *storage.DB) error { return db.FailUserLabelMutation(ctx, entry, err, scope.retryAt) }); saveErr != nil {
				return errors.Join(failures, err, saveErr)
			}
			failures = errors.Join(failures, fmt.Errorf("queued label %s: %w", entry.Operation, err))
			if scope.retryAt.After(time.Now()) {
				return failures
			}
		}
	}
	return failures
}
func labelProviderForUserAccount(provider string) (string, error) {
	switch provider {
	case "imap":
		return storage.LabelProviderIMAPKeyword, nil
	case "gmail":
		return storage.LabelProviderGmail, nil
	case "outlook":
		return storage.LabelProviderOutlook, nil
	}
	return "", errors.New("unsupported label provider")
}

func (s *UserIMAP) applyUserLabelMutation(ctx context.Context, scope *userIMAPScope, entry storage.LabelMutationQueueEntry) error {
	var info *storage.MessageMutationInfo
	var validity uint32
	if err := scope.call(ctx, func(db *storage.DB) error {
		var err error
		if entry.FolderID != "" {
			// Optimistic moves hide their source, which remains remotely valid until
			// move replay. Other deleted memberships must not receive keywords.
			var pending int
			if err := db.Read().QueryRowContext(ctx, `SELECT count(*) FROM message_mutations WHERE account_id=? AND message_id=? AND folder_id=? AND kind='move' AND status!='applied'`, scope.id, entry.MessageID, entry.FolderID).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				info, err = db.GetMessageMutationInfoIncludingDeletedInFolder(ctx, entry.MessageID, entry.FolderID)
			} else {
				info, err = db.GetMessageMutationInfoInFolder(ctx, entry.MessageID, entry.FolderID)
			}
			if err != nil {
				return err
			}
		}
		if info == nil {
			info, err = db.GetMessageMutationInfoInternal(ctx, entry.MessageID)
		}
		if err != nil || info == nil {
			return err
		}
		validity, err = db.GetFolderUIDValidity(ctx, info.FolderID)
		return err
	}); err != nil {
		return err
	}
	if info == nil {
		return scope.call(ctx, func(db *storage.DB) error { return db.DiscardUserLabelMutation(ctx, entry) })
	}
	expected, err := labelProviderForUserAccount(scope.config.Provider)
	if err != nil {
		return err
	}
	if entry.AccountID != scope.id || info.AccountID != scope.id || info.AccountProvider != scope.config.Provider || entry.ProviderType != expected {
		return errors.New("queued label account or provider changed")
	}
	if entry.Operation != storage.LabelMutationAdd && entry.Operation != storage.LabelMutationRemove {
		return errors.New("invalid queued label operation")
	}
	label := storage.LabelInput{AccountID: scope.id, Name: entry.LabelName, ProviderType: entry.ProviderType}
	resolvedID := info.RemoteMessageID
	if entry.ProviderType == storage.LabelProviderIMAPKeyword {
		keyword := entry.LabelName
		if err := scope.call(ctx, func(db *storage.DB) error {
			providerID, ok, err := db.ResolveLabelAliasProviderID(ctx, scope.id, entry.ProviderType, entry.LabelName)
			if ok {
				keyword = providerID
			}
			return err
		}); err != nil {
			return err
		}
		if entry.LabelName == "$Junk" || entry.LabelName == "$NotJunk" {
			// Reserved status keywords are emitted only by the spam transaction;
			// ordinary label requests reject them before queuing.
			keyword = entry.LabelName
		} else {
			keyword, err = mailimap.ValidateKeyword(keyword)
			if err != nil {
				return err
			}
		}
		label.ProviderID = keyword
		client, err := mailimap.NewContextClient(ctx, scope.config, scope.password)
		if err != nil {
			return err
		}
		defer client.Close()
		uid, epoch := info.RemoteUID, validity
		// A cached UID without its mailbox epoch is not a usable identity. Resolve
		// the message again before STORE, including folders not yet received locally.
		if uid == 0 || epoch == 0 {
			uid, epoch, err = client.FindUniqueUIDByMessageIDWithValidity(ctx, info.FolderRemoteID, info.InternetMessageID)
			if err != nil {
				return err
			}
		}
		if uid == 0 {
			return errors.New("label message has no remote IMAP identity")
		}
		operation := goimap.StoreFlagsDel
		if entry.Operation == storage.LabelMutationAdd {
			operation = goimap.StoreFlagsAdd
		}
		if entry.LabelName == "$Junk" || entry.LabelName == "$NotJunk" {
			err = client.StoreFlagsIfUIDValidity(ctx, info.FolderRemoteID, uid, operation, []goimap.Flag{goimap.Flag(keyword)}, epoch)
		} else {
			err = client.StoreKeywordIfUIDValidity(ctx, info.FolderRemoteID, uid, operation, keyword, epoch)
		}
		if err != nil {
			return err
		}
	} else {
		request, err := userProviderMutationRequest(ctx, scope)
		if err != nil {
			return err
		}
		if resolvedID == "" {
			resolvedID, err = resolveUserProviderMutationID(ctx, scope.config.Provider, info.InternetMessageID, request)
			if err != nil {
				return err
			}
		}
		if entry.ProviderType == storage.LabelProviderGmail {
			remote, found, err := userGmailLabel(entry.LabelName, entry.Operation == storage.LabelMutationAdd, request)
			if err != nil {
				return err
			}
			if found {
				label.Name, label.ProviderID, label.IsSystem = remote.Name, remote.ID, gmailLabelIsSystem(remote)
				key := "removeLabelIds"
				if entry.Operation == storage.LabelMutationAdd {
					key = "addLabelIds"
				}
				if err := request(http.MethodPost, gmailAPIBaseURL+"/users/me/messages/"+url.PathEscape(resolvedID)+"/modify", map[string][]string{key: {remote.ID}}, nil); err != nil {
					return err
				}
			}
		} else {
			var state outlookMessageState
			if err := request(http.MethodGet, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(resolvedID)+"?$select=id,categories", nil, &state); err != nil {
				return err
			}
			if state.ID == "" || state.ID != resolvedID {
				return errors.New("Graph returned a different label message")
			}
			categories := append([]string(nil), state.Categories...)
			if entry.Operation == storage.LabelMutationAdd {
				if err := userOutlookCategory(entry.LabelName, request); err != nil {
					return err
				}
				categories = appendUniqueFold(categories, entry.LabelName)
				sort.SliceStable(categories, func(i, j int) bool { return strings.ToLower(categories[i]) < strings.ToLower(categories[j]) })
			} else {
				categories, _ = removeFold(categories, entry.LabelName)
			}
			if err := request(http.MethodPatch, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(resolvedID), map[string][]string{"categories": categories}, nil); err != nil {
				return err
			}
			label.ProviderID = entry.LabelName
		}
	}
	return scope.call(ctx, func(db *storage.DB) error {
		return db.CompleteUserLabelMutation(ctx, entry, *info, scope.config.ProviderAccountID, resolvedID, validity, label)
	})
}

func userGmailLabel(name string, create bool, request userMutationRequest) (gmailLabel, bool, error) {
	var response gmailLabelsResponse
	if err := request(http.MethodGet, gmailAPIBaseURL+"/users/me/labels", nil, &response); err != nil {
		return gmailLabel{}, false, err
	}
	for _, label := range response.Labels {
		if strings.EqualFold(strings.TrimSpace(label.Name), strings.TrimSpace(name)) && strings.TrimSpace(label.ID) != "" {
			return label, true, nil
		}
	}
	if !create {
		return gmailLabel{}, false, nil
	}
	var label gmailLabel
	if err := request(http.MethodPost, gmailAPIBaseURL+"/users/me/labels", map[string]string{"name": strings.TrimSpace(name), "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &label); err != nil {
		return label, false, err
	}
	if strings.TrimSpace(label.ID) == "" {
		return label, false, errors.New("Gmail label create returned no identity")
	}
	if strings.TrimSpace(label.Name) == "" {
		label.Name = strings.TrimSpace(name)
	}
	return label, true, nil
}
func userOutlookCategory(name string, request userMutationRequest) error {
	var response outlookCategoriesResponse
	if err := request(http.MethodGet, outlookGraphBaseURL+"/me/outlook/masterCategories", nil, &response); err != nil {
		return err
	}
	for _, category := range response.Value {
		if strings.EqualFold(strings.TrimSpace(category.DisplayName), strings.TrimSpace(name)) {
			return nil
		}
	}
	return request(http.MethodPost, outlookGraphBaseURL+"/me/outlook/masterCategories", map[string]string{"displayName": strings.TrimSpace(name), "color": "preset0"}, nil)
}
