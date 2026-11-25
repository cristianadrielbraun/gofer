package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ProviderDrafts is a mailbox's drafts as the delivery queue syncs them: Gmail
// and Outlook through UserProviderMail, JMAP through JMAPDrafts.
type ProviderDrafts interface {
	GetDraft(ctx context.Context, id string) (UserProviderDraft, error)
	// FindDrafts returns the drafts kept under internetID. saved is when the
	// version being looked for was saved, or zero; only Gmail needs it.
	FindDrafts(ctx context.Context, internetID string, saved time.Time) ([]UserProviderDraft, error)
	SaveDraft(ctx context.Context, id string, mime []byte) (UserProviderDraft, error)
	DeleteDraft(ctx context.Context, draft UserProviderDraft) error
}

// errJMAPDraftMissing is a draft the server no longer has.
var errJMAPDraftMissing = errors.New("the JMAP draft no longer exists")

// JMAPDrafts syncs a JMAP account's drafts. Emails cannot change on a JMAP
// server, so every version is a new draft, and the queue removes the previous
// one once the new one is recorded, as it does for Outlook. A draft's id
// serves as both its draft and message identity.
type JMAPDrafts struct {
	config   *models.AccountConfig
	password string
}

func NewJMAPDrafts(cfg *models.AccountConfig, password string) *JMAPDrafts {
	return &JMAPDrafts{config: cfg, password: password}
}

// open loads the session, the mail account and its Drafts mailbox.
func (d *JMAPDrafts) open(ctx context.Context) (*jmap.Client, *jmap.Session, string, string, error) {
	client, session, account, err := openJMAPMail(ctx, d.config, d.password)
	if err != nil {
		return nil, nil, "", "", err
	}
	mailboxes, err := client.GetMailboxes(ctx, session, account, nil)
	if err != nil {
		return nil, nil, "", "", err
	}
	for _, mailbox := range mailboxes.List {
		if strings.EqualFold(mailbox.Role, "drafts") {
			return client, session, account, mailbox.ID, nil
		}
	}
	return nil, nil, "", "", errors.New("the JMAP account has no Drafts mailbox")
}

func (d *JMAPDrafts) GetDraft(ctx context.Context, id string) (UserProviderDraft, error) {
	if strings.TrimSpace(id) == "" {
		return UserProviderDraft{}, errors.New("provider draft identity is required")
	}
	client, session, account, err := openJMAPMail(ctx, d.config, d.password)
	if err != nil {
		return UserProviderDraft{}, err
	}
	revision := "header:" + jmap.DraftRevisionHeader + ":asText"
	responses, err := client.Call(ctx, session, []string{jmap.CapabilityMail}, jmap.Invocation{Name: "Email/get", Args: map[string]any{
		"accountId": account, "ids": []string{id}, "properties": []string{"id", "messageId", "keywords", revision},
	}, CallID: "0"})
	if err != nil {
		return UserProviderDraft{}, err
	}
	var got jmap.GetResponse[struct {
		ID        string          `json:"id"`
		MessageID []string        `json:"messageId"`
		Keywords  map[string]bool `json:"keywords"`
		Revision  *string         `json:"header:X-Gofer-Draft-Revision:asText"`
	}]
	if err := responses.Get("0", &got); err != nil {
		return UserProviderDraft{}, fmt.Errorf("Email/get: %w", err)
	}
	if len(got.List) == 0 {
		return UserProviderDraft{}, errJMAPDraftMissing
	}
	email := got.List[0]
	if email.ID != id || len(email.MessageID) != 1 {
		return UserProviderDraft{}, errors.New("provider draft identity is invalid")
	}
	if !email.Keywords[jmap.KeywordDraft] {
		return UserProviderDraft{}, errors.New("provider message is no longer a draft")
	}
	draft := UserProviderDraft{ID: id, MessageID: id, InternetMessageID: "<" + strings.Trim(email.MessageID[0], "<>") + ">"}
	if email.Revision != nil {
		draft.RevisionToken = strings.TrimSpace(*email.Revision)
	}
	return draft, nil
}

// FindDrafts returns the drafts in the Drafts mailbox whose Message-ID is
// internetID. JMAP servers keep the Message-ID, so saved is not needed.
func (d *JMAPDrafts) FindDrafts(ctx context.Context, internetID string, _ time.Time) ([]UserProviderDraft, error) {
	if strings.TrimSpace(internetID) == "" || strings.ContainsAny(internetID, "\r\n") {
		return nil, errors.New("draft Internet identity is required")
	}
	client, session, account, drafts, err := d.open(ctx)
	if err != nil {
		return nil, err
	}
	found, err := client.FindDrafts(ctx, session, account, drafts, internetID)
	if err != nil {
		return nil, err
	}
	out := make([]UserProviderDraft, 0, len(found))
	for _, draft := range found {
		out = append(out, UserProviderDraft{ID: draft.ID, MessageID: draft.ID, InternetMessageID: draft.MessageID, RevisionToken: draft.Revision})
	}
	return out, nil
}

// SaveDraft stores mime as a new draft. Like Graph, it cannot replace a draft
// in place, so id must be empty; the queue removes the old version itself.
// A save cut off before the server answered is uncertain, and is reconciled
// by revision before another attempt.
func (d *JMAPDrafts) SaveDraft(ctx context.Context, id string, mime []byte) (UserProviderDraft, error) {
	if id != "" {
		return UserProviderDraft{}, errors.New("JMAP draft replacement requires a new revision")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(mime))
	if err != nil {
		return UserProviderDraft{}, err
	}
	internetID, revision := strings.TrimSpace(msg.Header.Get("Message-ID")), strings.TrimSpace(msg.Header.Get(jmap.DraftRevisionHeader))
	if internetID == "" || revision == "" {
		return UserProviderDraft{}, errors.New("draft MIME identity and revision are required")
	}
	client, session, account, drafts, err := d.open(ctx)
	if err != nil {
		return UserProviderDraft{}, err
	}
	blob, err := client.UploadBlob(ctx, session, account, jmap.ContentTypeMessage, mime)
	if err != nil {
		return UserProviderDraft{}, err
	}
	saved, err := client.SaveDraft(ctx, session, account, blob, drafts, nil)
	var refused *jmap.DraftError
	var httpErr *jmap.HTTPError
	switch {
	case errors.As(err, &refused), errors.As(err, &httpErr):
		// The server answered and stored nothing.
		return UserProviderDraft{}, err
	case err != nil:
		return UserProviderDraft{}, fmt.Errorf("%w: %v", ErrUserProviderDraftUncertain, err)
	}
	return UserProviderDraft{ID: saved, MessageID: saved, InternetMessageID: internetID, RevisionToken: revision}, nil
}

// ReplaceDraft stores mime as the draft's new version and removes the
// previous one, if any, in the same request, for callers that save drafts
// directly rather than through the queue. It returns the new draft's id. When
// the new version was stored but the previous one could not be removed, it
// returns the id and an error too.
func (d *JMAPDrafts) ReplaceDraft(ctx context.Context, previousID string, mime []byte) (string, error) {
	client, session, account, drafts, err := d.open(ctx)
	if err != nil {
		return "", err
	}
	blob, err := client.UploadBlob(ctx, session, account, jmap.ContentTypeMessage, mime)
	if err != nil {
		return "", err
	}
	var replace []string
	if previousID != "" {
		replace = []string{previousID}
	}
	return client.SaveDraft(ctx, session, account, blob, drafts, replace)
}

// DeleteDraft removes a draft if it is still the version recorded for it; one
// already gone needs nothing more.
func (d *JMAPDrafts) DeleteDraft(ctx context.Context, draft UserProviderDraft) error {
	current, err := d.GetDraft(ctx, draft.ID)
	if errors.Is(err, errJMAPDraftMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if draft.InternetMessageID == "" || current.InternetMessageID != draft.InternetMessageID || (draft.RevisionToken != "" && current.RevisionToken != draft.RevisionToken) {
		return errors.New("provider draft changed before deletion")
	}
	client, session, account, err := openJMAPMail(ctx, d.config, d.password)
	if err != nil {
		return err
	}
	result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: account, Destroy: []string{draft.ID}})
	if err != nil {
		return err
	}
	if failure, failed := result.NotDestroyed[draft.ID]; failed && failure.Type != "notFound" {
		return &failure
	}
	return nil
}
