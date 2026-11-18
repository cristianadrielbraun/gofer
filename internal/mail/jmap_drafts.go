package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ProviderDrafts is a mailbox's drafts as the delivery queue syncs them: Gmail
// and Outlook through UserProviderMail, JMAP through JMAPDrafts.
type ProviderDrafts interface {
	GetDraft(ctx context.Context, id string) (UserProviderDraft, error)
	FindDrafts(ctx context.Context, internetID string) ([]UserProviderDraft, error)
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

// open loads the session, the JMAP account a draft from the address is kept
// in (a shared mailbox's own, for its address) and that account's Drafts
// mailbox.
func (d *JMAPDrafts) open(ctx context.Context, from string) (*jmap.Client, *jmap.Session, string, string, error) {
	// From is a header, "Name <address>"; the account goes by the address.
	if parsed, err := mail.ParseAddress(from); err == nil {
		from = parsed.Address
	}
	client, session, err := openJMAPSession(ctx, d.config, d.password)
	if err != nil {
		return nil, nil, "", "", err
	}
	if session.PrimaryAccount(jmap.CapabilityMail) == "" {
		return nil, nil, "", "", errors.New("the JMAP server offers no mail account for this login")
	}
	account := jmapSenderAccount(session, from).account
	return d.openIn(ctx, client, session, account)
}

// openIn finds the Drafts mailbox of one JMAP account.
func (d *JMAPDrafts) openIn(ctx context.Context, client *jmap.Client, session *jmap.Session, account string) (*jmap.Client, *jmap.Session, string, string, error) {
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
	client, session, err := openJMAPSession(ctx, d.config, d.password)
	if err != nil {
		return UserProviderDraft{}, err
	}
	stored := id
	account, id, err := jmapAccountOf(session, stored)
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
	draft := UserProviderDraft{ID: stored, MessageID: stored, InternetMessageID: "<" + strings.Trim(email.MessageID[0], "<>") + ">"}
	if email.Revision != nil {
		draft.RevisionToken = strings.TrimSpace(*email.Revision)
	}
	return draft, nil
}

func (d *JMAPDrafts) FindDrafts(ctx context.Context, internetID string) ([]UserProviderDraft, error) {
	if strings.TrimSpace(internetID) == "" || strings.ContainsAny(internetID, "\r\n") {
		return nil, errors.New("draft Internet identity is required")
	}
	client, session, err := openJMAPSession(ctx, d.config, d.password)
	if err != nil {
		return nil, err
	}
	own := session.PrimaryAccount(jmap.CapabilityMail)
	if own == "" {
		return nil, errors.New("the JMAP server offers no mail account for this login")
	}
	// A draft may be in the login's own Drafts or a shared mailbox's.
	accounts := []string{own}
	for _, target := range jmapSharedAccounts(session) {
		accounts = append(accounts, target.account)
	}
	var out []UserProviderDraft
	for i, account := range accounts {
		_, _, _, drafts, err := d.openIn(ctx, client, session, account)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		found, err := client.FindDrafts(ctx, session, account, drafts, internetID)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		for _, draft := range found {
			ref := jmapRefIn(session, account, draft.ID)
			out = append(out, UserProviderDraft{ID: ref, MessageID: ref, InternetMessageID: draft.MessageID, RevisionToken: draft.Revision})
		}
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
	client, session, account, drafts, err := d.open(ctx, msg.Header.Get("From"))
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
	ref := jmapRefIn(session, account, saved)
	return UserProviderDraft{ID: ref, MessageID: ref, InternetMessageID: internetID, RevisionToken: revision}, nil
}

// ReplaceDraft stores mime as the draft's new version and removes the
// previous one, if any, in the same request, for callers that save drafts
// directly rather than through the queue. It returns the new draft's id. When
// the new version was stored but the previous one could not be removed, it
// returns the id and an error too.
func (d *JMAPDrafts) ReplaceDraft(ctx context.Context, previousID string, mime []byte) (string, error) {
	from := ""
	if msg, err := mail.ReadMessage(bytes.NewReader(mime)); err == nil {
		from = msg.Header.Get("From")
	}
	client, session, account, drafts, err := d.open(ctx, from)
	if err != nil {
		return "", err
	}
	blob, err := client.UploadBlob(ctx, session, account, jmap.ContentTypeMessage, mime)
	if err != nil {
		return "", err
	}
	var replace []string
	var removeElsewhere string
	if previousID != "" {
		// The previous version goes in the same request when it is in the
		// same account; one in another, when the From changed between a
		// shared mailbox and the login's own, is removed after.
		if in, id, err := jmapAccountOf(session, previousID); err == nil && in == account {
			replace = []string{id}
		} else if err == nil {
			removeElsewhere = previousID
		}
	}
	saved, err := client.SaveDraft(ctx, session, account, blob, drafts, replace)
	if err != nil {
		return "", err
	}
	ref := jmapRefIn(session, account, saved)
	if removeElsewhere != "" {
		in, id, _ := jmapAccountOf(session, removeElsewhere)
		if _, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: in, Destroy: []string{id}}); err != nil {
			return ref, fmt.Errorf("remove the previous draft: %w", err)
		}
	}
	return ref, nil
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
	client, session, err := openJMAPSession(ctx, d.config, d.password)
	if err != nil {
		return err
	}
	account, id, err := jmapAccountOf(session, draft.ID)
	if err != nil {
		return err
	}
	result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: account, Destroy: []string{id}})
	if err != nil {
		return err
	}
	if failure, failed := result.NotDestroyed[id]; failed && failure.Type != "notFound" {
		return &failure
	}
	return nil
}
