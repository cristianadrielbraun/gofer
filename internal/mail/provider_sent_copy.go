package mail

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// GmailRequest makes one JSON request to the Gmail API with the account's
// credentials, decoding the answer into out.
type GmailRequest func(ctx context.Context, method, endpoint string, body, out any) error

// FileGmailSentCopy files a message sent through another server in a Gmail
// mailbox as sent mail, and returns its Gmail id. Gmail only stores it; it
// doesn't send it. A copy with the same Message-ID already there is used
// instead, so a retry after an unclear result files no second one. base is
// the Gmail API's address.
func FileGmailSentCopy(ctx context.Context, base string, do GmailRequest, raw []byte, messageID string) (string, error) {
	id := message.NormalizeMessageID(messageID)
	if id == "" {
		return "", errors.New("the message has no Message-ID to file it by")
	}
	var found struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	query := url.Values{"q": {"rfc822msgid:<" + id + ">"}, "includeSpamTrash": {"true"}}
	if err := do(ctx, http.MethodGet, base+"/users/me/messages?"+query.Encode(), nil, &found); err != nil {
		return "", err
	}
	if len(found.Messages) > 0 {
		return found.Messages[0].ID, nil
	}
	var inserted struct {
		ID string `json:"id"`
	}
	body := map[string]any{"raw": base64.RawURLEncoding.EncodeToString(raw), "labelIds": []string{"SENT"}}
	if err := do(ctx, http.MethodPost, base+"/users/me/messages?internalDateSource=dateHeader", body, &inserted); err != nil {
		return "", err
	}
	return strings.TrimSpace(inserted.ID), nil
}

// gmailRequest is a GmailRequest with the given access token, against this
// package's Gmail API address.
func gmailRequest(token string) GmailRequest {
	return func(ctx context.Context, method, endpoint string, body, out any) error {
		return providerJSON(ctx, method, endpoint, token, nil, body, out)
	}
}

// FileJMAPSentCopy files a message sent through another server in the JMAP
// account's Sent mailbox, seen, and returns its email id. A copy with the
// same Message-ID already there is used instead.
func FileJMAPSentCopy(ctx context.Context, cfg *models.AccountConfig, password string, raw []byte, messageID string, date time.Time) (string, error) {
	if !providers.JMAPEnabled() {
		return "", providers.ErrJMAPDisabled
	}
	id := message.NormalizeMessageID(messageID)
	if id == "" {
		return "", errors.New("the message has no Message-ID to file it by")
	}
	client, session, account, err := openJMAPMail(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	mailboxes, err := client.GetMailboxes(ctx, session, account, nil)
	if err != nil {
		return "", err
	}
	sent := ""
	for _, mailbox := range mailboxes.List {
		if strings.EqualFold(mailbox.Role, "sent") {
			sent = mailbox.ID
		}
	}
	if sent == "" {
		return "", errors.New("the JMAP account has no Sent mailbox")
	}
	windows := []jmap.Window{{From: time.Now().Add(-time.Hour), To: time.Now().Add(jmapClockSkew)}}
	if !date.IsZero() {
		windows = append(windows, jmap.Window{From: date.Add(-time.Second), To: date.Add(time.Second)})
	}
	found, err := client.FindEmails(ctx, session, account, sent, id, windows)
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0], nil
	}
	blob, err := client.UploadBlob(ctx, session, account, jmap.ContentTypeMessage, raw)
	if err != nil {
		return "", err
	}
	imported, err := client.ImportEmail(ctx, session, account, blob, sent, []string{jmap.KeywordSeen}, date)
	if err != nil {
		return "", err
	}
	return imported.ID, nil
}

// FileGmailSentCopy files a message sent through another server in this
// Gmail mailbox, signing in again if the token has expired.
func (p *UserProviderMail) FileGmailSentCopy(ctx context.Context, raw []byte, messageID string) (string, error) {
	if p == nil || p.scope == nil || p.scope.tokens == nil || p.scope.config.Provider != providers.ProviderGmail {
		return "", errors.New("Gmail is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.lifetime, cancel)
	defer stop()
	token, err := p.scope.tokens.GetOAuthTokenForAccount(ctx, p.scope.id)
	if err != nil {
		return "", err
	}
	o := NewSyncOrchestrator(nil, nil, nil, p.scope.tokens)
	o.imapScope = p.scope
	var id string
	err = o.outlookRequest(ctx, token, func(access string) error {
		var err error
		id, err = FileGmailSentCopy(ctx, gmailAPIBaseURL, gmailRequest(access), raw, messageID)
		return err
	})
	return id, err
}
