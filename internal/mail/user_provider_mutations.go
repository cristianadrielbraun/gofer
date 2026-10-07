package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errProviderMutationGone = errors.New("provider message no longer exists")

// Provider replay shares the receive account gate, cancellation and session
// bounds. Repository calls end before HTTP; result publication rechecks the
// account subject, original message ID, destination and exact queue attempt.
func (s *UserIMAP) applyProviderMutation(ctx context.Context, scope *userIMAPScope, m storage.MessageMutation, info storage.MessageMutationInfo) error {
	provider := scope.config.Provider
	if m.AccountID != scope.id || info.AccountID != scope.id || m.ProviderType != provider || info.AccountProvider != provider {
		return errors.New("queued mutation account or provider changed")
	}
	request, err := userProviderMutationRequest(ctx, scope)
	if err != nil {
		return err
	}

	original := strings.TrimSpace(info.RemoteMessageID)
	id := original
	if id == "" {
		id, err = resolveUserProviderMutationID(ctx, provider, info.InternetMessageID, request)
		if errors.Is(err, errProviderMutationGone) && m.Kind == storage.MessageMutationDelete {
			return scope.call(ctx, func(db *storage.DB) error {
				return db.CompleteProviderMutation(ctx, m, scope.config.ProviderAccountID, original, info.InternetMessageID, "", "", true)
			})
		}
		if err != nil {
			return err
		}
	}
	movedID, destinationID := "", ""
	gone := false
	switch m.Kind {
	case storage.MessageMutationRead, storage.MessageMutationStarred:
		if provider == "gmail" {
			label := "UNREAD"
			add := !m.TargetValue
			if m.Kind == storage.MessageMutationStarred {
				label = "STARRED"
				add = m.TargetValue
			}
			key := "removeLabelIds"
			if add {
				key = "addLabelIds"
			}
			err = request(http.MethodPost, gmailAPIBaseURL+"/users/me/messages/"+url.PathEscape(id)+"/modify", map[string][]string{key: {label}}, nil)
		} else {
			var payload any = map[string]bool{"isRead": m.TargetValue}
			if m.Kind == storage.MessageMutationStarred {
				status := "notFlagged"
				if m.TargetValue {
					status = "flagged"
				}
				payload = map[string]any{"flag": map[string]string{"flagStatus": status}}
			}
			err = request(http.MethodPatch, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(id), payload, nil)
		}
	case storage.MessageMutationDelete:
		endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(id)
		if provider == "outlook" {
			endpoint = outlookGraphBaseURL + "/me/messages/" + url.PathEscape(id)
		}
		err = request(http.MethodDelete, endpoint, nil, nil)
		if status, ok := providerAPIStatus(err); ok && status == 404 {
			gone = true
			err = nil
		}
	case storage.MessageMutationMove:
		var role, sourceID string
		err = scope.call(ctx, func(db *storage.DB) error {
			var err error
			destinationID, role, err = db.GetFolderProviderRemoteInfo(ctx, m.DestinationFolderID)
			if err != nil {
				return err
			}
			sourceID, err = db.GetFolderProviderRemoteID(ctx, m.FolderID)
			return err
		})
		if err != nil {
			return err
		}
		if provider == "gmail" {
			err = applyUserGmailMove(id, destinationID, role, sourceID, info.FolderRole, request)
			movedID = id
		} else {
			if destinationID == "" {
				return errors.New("Outlook move destination has no provider identity")
			}
			// Retry/finalization recovery checks the current parent before repeating a
			// move. Immutable IDs remain stable, but older cached IDs may need lookup.
			if m.AttemptCount > 1 {
				var current struct {
					ID     string `json:"id"`
					Parent string `json:"parentFolderId"`
				}
				err = request(http.MethodGet, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(id)+"?$select=id,parentFolderId", nil, &current)
				if status, ok := providerAPIStatus(err); ok && status == 404 {
					id, err = resolveUserProviderMutationID(ctx, provider, info.InternetMessageID, request)
					if err == nil {
						err = request(http.MethodGet, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(id)+"?$select=id,parentFolderId", nil, &current)
					}
				}
				if err != nil {
					return err
				}
				if current.ID == "" || current.ID != id {
					return errors.New("Outlook move recovery returned a different message")
				}
				if current.Parent == destinationID {
					movedID = id
					break
				}
			}
			var moved struct {
				ID string `json:"id"`
			}
			err = request(http.MethodPost, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": destinationID}, &moved)
			if err == nil {
				movedID = strings.TrimSpace(moved.ID)
				if movedID == "" {
					return errors.New("Outlook move returned no message identity")
				}
			}
		}
	default:
		return fmt.Errorf("unsupported provider mutation %q", m.Kind)
	}
	if err != nil {
		return err
	}
	if movedID == "" {
		movedID = id
	}
	err = scope.call(ctx, func(db *storage.DB) error {
		return db.CompleteProviderMutation(ctx, m, scope.config.ProviderAccountID, original, info.InternetMessageID, movedID, destinationID, gone)
	})
	if errors.Is(err, storage.ErrMessageMutationSuperseded) {
		return nil
	} // Newer intent remains durable.
	return err
}

func userProviderMutationRequest(ctx context.Context, scope *userIMAPScope) (userMutationRequest, error) {
	provider := scope.config.Provider
	o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
	o.imapScope = scope
	var token string
	var err error
	if provider == "outlook" {
		graph, ok := scope.tokens.(graphMailTokenProvider)
		if !ok {
			return nil, errors.New("Graph mail credentials are unavailable")
		}
		token, err = graph.GetMicrosoftGraphMailTokenForAccount(ctx, scope.id)
	} else {
		token, err = scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	}
	if err != nil {
		return nil, o.recordOutlookRetry(ctx, err)
	}
	request := func(method, endpoint string, body, out any) error {
		// Revalidate identity before each provider call without holding the lease.
		if err := scope.call(ctx, func(*storage.DB) error { return nil }); err != nil {
			return err
		}
		return o.outlookRequest(ctx, token, func(access string) error {
			headers := map[string]string(nil)
			if provider == "outlook" {
				if err := validateUserGraphEndpoint(endpoint); err != nil {
					return err
				}
				headers = outlookImmutableIDHeaders()
			}
			return providerJSON(ctx, method, endpoint, access, headers, body, out)
		})
	}
	return request, nil
}

type userMutationRequest func(string, string, any, any) error

func resolveUserProviderMutationID(ctx context.Context, provider, internetID string, request userMutationRequest) (string, error) {
	if strings.TrimSpace(internetID) == "" {
		return "", errors.New("provider message identity is unavailable")
	}
	values := url.Values{}
	if provider == "gmail" {
		values.Set("q", "rfc822msgid:"+internetID)
		values.Set("includeSpamTrash", "true")
		values.Set("maxResults", "2")
		var result struct {
			NextPage string `json:"nextPageToken"`
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := request(http.MethodGet, gmailAPIBaseURL+"/users/me/messages?"+values.Encode(), nil, &result); err != nil {
			return "", err
		}
		if result.NextPage != "" {
			return "", errors.New("Gmail message identity search is incomplete")
		}
		if len(result.Messages) == 0 {
			return "", errProviderMutationGone
		}
		if len(result.Messages) != 1 || result.Messages[0].ID == "" {
			return "", errors.New("Gmail message identity is ambiguous")
		}
		return result.Messages[0].ID, nil
	}
	values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetID, "'", "''")+"'")
	values.Set("$select", "id,internetMessageId")
	values.Set("$top", "2")
	var result struct {
		NextLink string `json:"@odata.nextLink"`
		Value    []struct {
			ID         string `json:"id"`
			InternetID string `json:"internetMessageId"`
		} `json:"value"`
	}
	if err := request(http.MethodGet, outlookGraphBaseURL+"/me/messages?"+values.Encode(), nil, &result); err != nil {
		return "", err
	}
	if result.NextLink != "" {
		return "", errors.New("Outlook message identity search is incomplete")
	}
	if len(result.Value) == 0 {
		return "", errProviderMutationGone
	}
	if len(result.Value) != 1 || result.Value[0].ID == "" || result.Value[0].InternetID != internetID {
		return "", errors.New("Outlook message identity is ambiguous")
	}
	return result.Value[0].ID, nil
}

func applyUserGmailMove(id, destination, role, source, sourceRole string, request userMutationRequest) error {
	base := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(id)
	if sourceRole == "trash" && role != "trash" {
		if err := request(http.MethodPost, base+"/untrash", map[string]any{}, nil); err != nil {
			return err
		}
	}
	remove := []string{}
	if sourceRole == "custom" && source != "" && source != "ARCHIVE" && source != destination {
		remove = append(remove, source)
	}
	add := []string{}
	switch role {
	case "trash":
		return request(http.MethodPost, base+"/trash", map[string]any{}, nil)
	case "inbox":
		add = append(add, "INBOX")
		remove = append(remove, "SPAM", "TRASH")
	case "junk", "spam":
		add = append(add, "SPAM")
		remove = append(remove, "INBOX", "TRASH")
	case "archive":
		remove = append(remove, "INBOX")
	case "starred":
		add = append(add, "STARRED")
		remove = append(remove, "INBOX")
	default:
		if destination == "" || destination == "ARCHIVE" {
			return errors.New("Gmail destination has no provider identity")
		}
		add = append(add, destination)
		remove = append(remove, "INBOX")
	}
	payload := map[string][]string{}
	if len(add) > 0 {
		payload["addLabelIds"] = add
	}
	if len(remove) > 0 {
		payload["removeLabelIds"] = remove
	}
	return request(http.MethodPost, base+"/modify", payload, nil)
}
