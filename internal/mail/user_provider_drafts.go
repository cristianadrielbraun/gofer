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
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Uncertain draft creation must be reconciled by revision before another POST.
// A success status with an unusable identity is uncertain too.
var ErrUserProviderDraftUncertain = errors.New("provider draft acceptance is uncertain")

func IsUserProviderDraftMissing(err error) bool {
	if errors.Is(err, errJMAPDraftMissing) {
		return true
	}
	status, ok := providerAPIStatus(err)
	return ok && status == http.StatusNotFound
}
func UserProviderDraftRetryable(err error) bool {
	status, ok := providerAPIStatus(err)
	return !ok || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

// gmailDraftKeyHeader carries a draft's Gofer key in the drafts Gofer saves
// to Gmail. Gmail replaces the Message-ID of a draft uploaded through its API
// with one of its own, but keeps this header and X-Gofer-Draft-Revision.
const gmailDraftKeyHeader = "X-Gofer-Draft-Key"

// When Gofer looks for a Gmail draft whose create reply was lost, it reads the
// drafts dated from gmailDraftSearchBefore before that save until
// gmailDraftSearchAfter after it. The margins allow for clock differences and
// for a save that waited in the queue before it was uploaded.
const (
	gmailDraftSearchBefore = time.Hour
	gmailDraftSearchAfter  = 24 * time.Hour
)

type UserProviderDraft struct {
	ID, MessageID, InternetMessageID, RevisionToken string
	// untagged marks a Gmail draft without X-Gofer-Draft-Key, whose
	// InternetMessageID is therefore Gmail's own Message-ID: a draft written
	// elsewhere, or one Gofer saved before it added the header.
	untagged bool
}

// BelongsTo reports whether d is the draft Gofer keeps under key. revision is
// the last version of it Gofer recorded saving, or "" when there is none. A
// Gmail draft Gofer saved before it added X-Gofer-Draft-Key carries only
// Gmail's replacement Message-ID, so it belongs to key when it still holds
// that recorded revision, which only Gofer writes.
func (d UserProviderDraft) BelongsTo(key, revision string) bool {
	if key == "" {
		return false
	}
	return d.InternetMessageID == key || (d.untagged && revision != "" && d.RevisionToken == revision)
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
	key := ""
	for _, h := range r.Message.Payload.Headers {
		switch {
		case strings.EqualFold(h.Name, "Message-ID"):
			d.InternetMessageID = strings.TrimSpace(h.Value)
		case strings.EqualFold(h.Name, "X-Gofer-Draft-Revision"):
			d.RevisionToken = strings.TrimSpace(h.Value)
		case strings.EqualFold(h.Name, gmailDraftKeyHeader):
			key = strings.TrimSpace(h.Value)
		}
	}
	// Gmail's Message-ID says nothing about which Gofer draft this is.
	if key != "" {
		d.InternetMessageID = key
	} else {
		d.untagged = true
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

// FindDrafts returns the drafts Gofer keeps under internetID, checking each
// one's actual headers. It never treats a partial, repeated or excessive page
// sequence as an empty search.
//
// Gmail replaces the Message-ID of drafts Gofer uploads, so a search by it only
// finds drafts written elsewhere. When saved is not zero, FindDrafts also reads
// the Gmail drafts dated around it and keeps those whose X-Gofer-Draft-Key is
// internetID: that is how a save whose reply was lost is found again. Outlook
// keeps the Message-ID and ignores saved.
func (p *UserProviderMail) FindDrafts(ctx context.Context, internetID string, saved time.Time) ([]UserProviderDraft, error) {
	if strings.TrimSpace(internetID) == "" || strings.ContainsAny(internetID, "\r\n") {
		return nil, errors.New("draft Internet identity is required")
	}
	_, finish, request, err := p.draftSession(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	values := url.Values{}
	if p.scope.config.Provider != "gmail" {
		values.Set("$filter", "internetMessageId eq '"+strings.ReplaceAll(internetID, "'", "''")+"' and isDraft eq true")
		values.Set("$select", "id")
		values.Set("$top", "25")
		return p.searchDrafts(request, outlookGraphBaseURL+"/me/mailFolders/drafts/messages", values, internetID, true, map[string]bool{}, nil)
	}
	values.Set("q", "rfc822msgid:"+internetID)
	values.Set("maxResults", "25")
	seen := map[string]bool{}
	drafts, err := p.searchDrafts(request, gmailAPIBaseURL+"/users/me/drafts", values, internetID, true, seen, nil)
	if err != nil || saved.IsZero() {
		return drafts, err
	}
	dated := url.Values{}
	dated.Set("q", "after:"+strconv.FormatInt(saved.Add(-gmailDraftSearchBefore).Unix(), 10)+" before:"+strconv.FormatInt(saved.Add(gmailDraftSearchAfter).Unix(), 10))
	dated.Set("maxResults", "25")
	return p.searchDrafts(request, gmailAPIBaseURL+"/users/me/drafts", dated, internetID, false, seen, drafts)
}

// searchDrafts reads every page of one draft listing and appends the drafts
// that belong to internetID to drafts. A strict listing was filtered by
// internetID already, so a draft in it that belongs elsewhere is an error; a
// listing by date holds other drafts too, and those are skipped. Drafts in seen
// were read by an earlier listing and are not read again.
func (p *UserProviderMail) searchDrafts(request userDraftRequest, base string, values url.Values, internetID string, strict bool, seen map[string]bool, drafts []UserProviderDraft) ([]UserProviderDraft, error) {
	endpoint := base + "?" + values.Encode()
	seenPages, listed := map[string]bool{}, map[string]bool{}
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
			if item.ID == "" || listed[item.ID] || len(listed) >= 100 {
				return nil, errors.New("provider draft search returned invalid identities")
			}
			listed[item.ID] = true
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			draft, err := p.getDraft(item.ID, request)
			if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !draft.BelongsTo(internetID, "") {
				if strict {
					return nil, errors.New("provider draft search returned another message")
				}
				continue
			}
			drafts = append(drafts, draft)
		}
		endpoint = result.NextLink
		if p.scope.config.Provider == "gmail" {
			endpoint = ""
			if result.NextPageToken != "" {
				values.Set("pageToken", result.NextPageToken)
				endpoint = base + "?" + values.Encode()
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
	if internetID == "" || revision == "" || strings.ContainsAny(internetID, "\r\n") {
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
		// Gmail keeps this header when it replaces the Message-ID, so later
		// saves and deletes can still tell this draft is Gofer's.
		upload := mime
		if msg.Header.Get(gmailDraftKeyHeader) == "" {
			upload = append([]byte(gmailDraftKeyHeader+": "+internetID+"\r\n"), mime...)
		}
		body, _ := json.Marshal(map[string]any{"message": map[string]string{"raw": base64.RawURLEncoding.EncodeToString(upload)}})
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
	if !current.BelongsTo(draft.InternetMessageID, draft.RevisionToken) || (draft.RevisionToken != "" && current.RevisionToken != draft.RevisionToken) {
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
