package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Uncertain draft creation must be reconciled by revision before another POST.
// A success status with an unusable identity is uncertain too.
var ErrUserProviderDraftUncertain = errors.New("provider draft acceptance is uncertain")

func IsUserProviderDraftMissing(err error) bool {
	status, ok := providerAPIStatus(err)
	return ok && status == http.StatusNotFound
}
func UserProviderDraftRetryable(err error) bool {
	status, ok := providerAPIStatus(err)
	return !ok || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

type UserProviderDraft struct {
	ID, MessageID, InternetMessageID, RevisionToken string
}

type userDraftRequest func(string, string, string, []byte, any) (bool, error)

func (p *UserProviderMail) draftSession(ctx context.Context) (context.Context, context.CancelFunc, userDraftRequest, error) {
	if p == nil || p.scope == nil || p.scope.tokens == nil || p.lifetime == nil {
		return nil, nil, nil, errors.New("provider draft session is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.lifetime, cancel)
	finish := func() { stop(); cancel() }
	if err := p.lifetime.Err(); err != nil {
		finish()
		return nil, nil, nil, err
	}
	scope := p.scope
	o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
	o.imapScope = scope
	var token string
	var err error
	switch scope.config.Provider {
	case "gmail":
		token, err = scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	case "outlook":
		graph, ok := scope.tokens.(graphMailTokenProvider)
		if !ok {
			finish()
			return nil, nil, nil, errors.New("Graph mail credentials are unavailable")
		}
		token, err = graph.GetMicrosoftGraphMailTokenForAccount(ctx, scope.id)
	default:
		err = errors.New("unsupported draft provider")
	}
	if err != nil {
		err = o.recordOutlookRetry(ctx, err)
		finish()
		return nil, nil, nil, err
	}
	request := func(method, endpoint, contentType string, body []byte, out any) (bool, error) {
		var accepted, attempted bool
		var wireErr error
		err := o.outlookRequest(ctx, token, func(access string) error {
			if err := scope.call(ctx, func(*storage.DB) error { return nil }); err != nil {
				return err
			}
			headers := map[string]string(nil)
			if scope.config.Provider == "outlook" {
				if err := validateUserGraphEndpoint(endpoint); err != nil {
					return err
				}
				headers = outlookImmutableIDHeaders()
			}
			attempted = true
			accepted, wireErr = providerMailRaw(ctx, method, endpoint, access, contentType, headers, body, out)
			return wireErr
		})
		if err != nil && attempted && method != http.MethodGet {
			_, definitive := providerAPIStatus(wireErr)
			if accepted || !definitive {
				err = errors.Join(ErrUserProviderDraftUncertain, err)
			}
		}
		return accepted, err
	}
	return ctx, finish, request, nil
}

type userGmailDraftResponse struct {
	ID      string `json:"id"`
	Message struct {
		ID      string `json:"id"`
		Payload struct {
			Headers []struct {
				Name, Value string
			} `json:"headers"`
		} `json:"payload"`
	} `json:"message"`
}

func (r userGmailDraftResponse) draft() UserProviderDraft {
	d := UserProviderDraft{ID: r.ID, MessageID: r.Message.ID}
	for _, h := range r.Message.Payload.Headers {
		switch {
		case strings.EqualFold(h.Name, "Message-ID"):
			d.InternetMessageID = strings.TrimSpace(h.Value)
		case strings.EqualFold(h.Name, "X-Gofer-Draft-Revision"):
			d.RevisionToken = strings.TrimSpace(h.Value)
		}
	}
	return d
}

type userGraphDraftResponse struct {
	ID                string `json:"id"`
	InternetMessageID string `json:"internetMessageId"`
	IsDraft           bool   `json:"isDraft"`
	Headers           []struct {
		Name, Value string
	} `json:"internetMessageHeaders"`
}

func (r userGraphDraftResponse) draft() UserProviderDraft {
	d := UserProviderDraft{ID: r.ID, MessageID: r.ID, InternetMessageID: strings.TrimSpace(r.InternetMessageID)}
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, "X-Gofer-Draft-Revision") {
			d.RevisionToken = strings.TrimSpace(h.Value)
		}
	}
	return d
}

func (p *UserProviderMail) getDraft(id string, request userDraftRequest) (UserProviderDraft, error) {
	if strings.TrimSpace(id) == "" {
		return UserProviderDraft{}, errors.New("provider draft identity is required")
	}
	var d UserProviderDraft
	if p.scope.config.Provider == "gmail" {
		var reply userGmailDraftResponse
		_, err := request(http.MethodGet, gmailAPIBaseURL+"/users/me/drafts/"+url.PathEscape(id)+"?format=metadata", "", nil, &reply)
		if err != nil {
			return d, err
		}
		d = reply.draft()
	} else {
		var reply userGraphDraftResponse
		_, err := request(http.MethodGet, outlookGraphBaseURL+"/me/messages/"+url.PathEscape(id)+"?$select=id,internetMessageId,isDraft,internetMessageHeaders", "", nil, &reply)
		if err != nil {
			return d, err
		}
		if !reply.IsDraft {
			return d, errors.New("provider message is no longer a draft")
		}
		d = reply.draft()
	}
	if d.ID != id || d.MessageID == "" || d.InternetMessageID == "" {
		return UserProviderDraft{}, errors.New("provider draft identity is invalid")
	}
	return d, nil
}

func (p *UserProviderMail) GetDraft(ctx context.Context, id string) (UserProviderDraft, error) {
	_, finish, request, err := p.draftSession(ctx)
	if err != nil {
		return UserProviderDraft{}, err
	}
	defer finish()
	return p.getDraft(id, request)
}

// FindDrafts uses the stable Internet ID, then checks the actual headers. It
// never treats a partial, repeated or excessive page sequence as an empty search.
func (p *UserProviderMail) FindDrafts(ctx context.Context, internetID string) ([]UserProviderDraft, error) {
	if strings.TrimSpace(internetID) == "" || strings.ContainsAny(internetID, "\r\n") {
		return nil, errors.New("draft Internet identity is required")
	}
	_, finish, request, err := p.draftSession(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	values := url.Values{}
	endpoint := ""
	if p.scope.config.Provider == "gmail" {
		values.Set("q", "rfc822msgid:"+internetID)
		values.Set("maxResults", "25")
		endpoint = gmailAPIBaseURL + "/users/me/drafts?" + values.Encode()
	} else {
		values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetID, "'", "''")+"' and isDraft eq true")
		values.Set("$select", "id")
		values.Set("$top", "25")
		endpoint = outlookGraphBaseURL + "/me/mailFolders/drafts/messages?" + values.Encode()
	}
	var drafts []UserProviderDraft
	seenPages, seenIDs := map[string]bool{}, map[string]bool{}
	for endpoint != "" {
		if seenPages[endpoint] || len(seenPages) >= 4 {
			return nil, errors.New("provider draft search exceeds its page bound")
		}
		seenPages[endpoint] = true
		var result struct {
			Drafts        []struct{ ID string } `json:"drafts"`
			Value         []struct{ ID string } `json:"value"`
			NextPageToken string                `json:"nextPageToken"`
			NextLink      string                `json:"@odata.nextLink"`
		}
		if _, err := request(http.MethodGet, endpoint, "", nil, &result); err != nil {
			return nil, err
		}
		ids := result.Drafts
		if p.scope.config.Provider == "outlook" {
			ids = result.Value
		}
		for _, item := range ids {
			if item.ID == "" || seenIDs[item.ID] || len(seenIDs) >= 100 {
				return nil, errors.New("provider draft search returned invalid identities")
			}
			seenIDs[item.ID] = true
			draft, err := p.getDraft(item.ID, request)
			if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			if draft.InternetMessageID != internetID {
				return nil, errors.New("provider draft search returned another message")
			}
			drafts = append(drafts, draft)
		}
		endpoint = result.NextLink
		if p.scope.config.Provider == "gmail" {
			endpoint = ""
			if result.NextPageToken != "" {
				values.Set("pageToken", result.NextPageToken)
				endpoint = gmailAPIBaseURL + "/users/me/drafts?" + values.Encode()
			}
		}
	}
	return drafts, nil
}

// SaveDraft creates a MIME revision or replaces a Gmail draft container. Graph
// MIME revisions require a new draft; callers retain and delete the old ID only
// after confirming and durably recording the new one.
func (p *UserProviderMail) SaveDraft(ctx context.Context, id string, mime []byte) (UserProviderDraft, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(mime))
	if err != nil {
		return UserProviderDraft{}, err
	}
	internetID, revision := strings.TrimSpace(msg.Header.Get("Message-ID")), strings.TrimSpace(msg.Header.Get("X-Gofer-Draft-Revision"))
	if internetID == "" || revision == "" {
		return UserProviderDraft{}, errors.New("draft MIME identity and revision are required")
	}
	_, finish, request, err := p.draftSession(ctx)
	if err != nil {
		return UserProviderDraft{}, err
	}
	defer finish()
	var d UserProviderDraft
	if p.scope.config.Provider == "gmail" {
		endpoint, method := gmailAPIBaseURL+"/users/me/drafts", http.MethodPost
		if id != "" {
			endpoint, method = endpoint+"/"+url.PathEscape(id), http.MethodPut
		}
		body, _ := json.Marshal(map[string]any{"message": map[string]string{"raw": base64.RawURLEncoding.EncodeToString(mime)}})
		var reply userGmailDraftResponse
		if _, err := request(method, endpoint, "application/json", body, &reply); err != nil {
			return d, err
		}
		d = reply.draft()
		if id != "" && d.ID != id {
			return UserProviderDraft{}, fmt.Errorf("%w: Gmail draft container changed", ErrUserProviderDraftUncertain)
		}
	} else {
		if id != "" {
			return d, errors.New("Graph MIME draft replacement requires a new revision")
		}
		var reply userGraphDraftResponse
		if _, err := request(http.MethodPost, outlookGraphBaseURL+"/me/messages", "text/plain", []byte(base64.StdEncoding.EncodeToString(mime)), &reply); err != nil {
			return d, err
		}
		d = reply.draft()
	}
	if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.MessageID) == "" {
		return UserProviderDraft{}, fmt.Errorf("%w: provider draft response missing identity", ErrUserProviderDraftUncertain)
	}
	d.InternetMessageID, d.RevisionToken = internetID, revision
	return d, nil
}

func (p *UserProviderMail) DeleteDraft(ctx context.Context, draft UserProviderDraft) error {
	_, finish, request, err := p.draftSession(ctx)
	if err != nil {
		return err
	}
	defer finish()
	current, err := p.getDraft(draft.ID, request)
	if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if draft.InternetMessageID == "" || current.InternetMessageID != draft.InternetMessageID || (draft.RevisionToken != "" && current.RevisionToken != draft.RevisionToken) {
		return errors.New("provider draft changed before deletion")
	}
	endpoint := gmailAPIBaseURL + "/users/me/drafts/" + url.PathEscape(draft.ID)
	if p.scope.config.Provider == "outlook" {
		endpoint = outlookGraphBaseURL + "/me/messages/" + url.PathEscape(draft.ID)
	}
	_, err = request(http.MethodDelete, endpoint, "", nil, nil)
	if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
		return nil
	}
	return err
}
