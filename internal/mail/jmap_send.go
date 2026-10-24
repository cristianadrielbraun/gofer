package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	netmail "net/mail"
	"slices"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// JMAPNotSentError is a JMAP send that failed before the server could accept
// the message, so nothing went out. Retryable says whether trying again may
// work, as when the server was unreachable, rather than being refused.
type JMAPNotSentError struct {
	Err       error
	Retryable bool
}

func (e *JMAPNotSentError) Error() string { return e.Err.Error() }
func (e *JMAPNotSentError) Unwrap() error { return e.Err }

// Submission refusals worth trying again later; any other refusal stands.
var retryableJMAPSubmissionErrors = map[string]bool{"rateLimit": true, "serverUnavailable": true, "serverFail": true}

// SendJMAPMessage sends one message Gofer built, from the account's identity
// for from, to every address in to, including Bcc recipients. It returns the
// sent email's id. A *JMAPNotSentError means nothing was sent; any other error
// may follow the server accepting the message, so it must not be retried
// blindly.
//
// When an earlier attempt may have run, since is when it could have started:
// the server is asked first whether that attempt went out, so a retry never
// sends the message twice. since is zero on the first attempt.
func SendJMAPMessage(ctx context.Context, cfg *models.AccountConfig, password, from string, to []string, raw []byte, since time.Time) (string, error) {
	notSent := func(err error, retryable bool) error { return &JMAPNotSentError{Err: err, Retryable: retryable} }
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return "", notSent(err, false)
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		var httpErr *jmap.HTTPError
		rejected := errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
		return "", notSent(fmt.Errorf("JMAP session: %w", err), !rejected)
	}
	account := session.PrimaryAccount(jmap.CapabilityMail)
	if account == "" {
		return "", notSent(errors.New("the JMAP server offers no mail account for this login"), false)
	}
	if !jmapCanSend(session) {
		return "", notSent(ErrJMAPCannotSend, false)
	}
	identities, err := client.GetIdentities(ctx, session, account)
	if err != nil {
		return "", notSent(err, true)
	}
	identity, ok := jmap.MatchIdentity(identities.List, from)
	if !ok {
		return "", notSent(fmt.Errorf("the JMAP server does not let this account send as %s", from), false)
	}
	mailboxes, err := client.GetMailboxes(ctx, session, account, nil)
	if err != nil {
		return "", notSent(err, true)
	}
	var drafts, sent string
	for _, mailbox := range mailboxes.List {
		switch strings.ToLower(mailbox.Role) {
		case "drafts":
			drafts = mailbox.ID
		case "sent":
			sent = mailbox.ID
		}
	}
	if drafts == "" {
		return "", notSent(errors.New("the JMAP account has no Drafts mailbox to send from"), false)
	}
	if !since.IsZero() {
		state, emailID, err := jmapSendOutcome(ctx, client, session, account, drafts, sent, raw, since)
		switch {
		case err != nil:
			return "", notSent(fmt.Errorf("check whether an earlier attempt was sent: %w", err), true)
		case state == jmapSent:
			return emailID, nil
		}
	}
	blob, err := client.UploadBlob(ctx, session, account, jmap.ContentTypeMessage, raw)
	if err != nil {
		var httpErr *jmap.HTTPError
		tooLarge := errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusRequestEntityTooLarge
		tooLarge = tooLarge || strings.Contains(err.Error(), "upload limit")
		return "", notSent(fmt.Errorf("upload message: %w", err), !tooLarge)
	}
	sending := time.Now()
	result, err := client.SendEmail(ctx, session, account, jmap.Outgoing{
		BlobID: blob, IdentityID: identity.ID, DraftsID: drafts, SentID: sent,
		Envelope: jmap.Envelope{MailFrom: from, RcptTo: to},
	})
	var refused *jmap.SubmissionError
	var httpErr *jmap.HTTPError
	switch {
	case err == nil:
		return result.EmailID, nil
	case errors.As(err, &refused):
		return "", notSent(err, retryableJMAPSubmissionErrors[refused.Type])
	case errors.As(err, &httpErr) && httpErr.StatusCode < 500:
		// The server refused the request as a whole, so it ran no part of it.
		return "", notSent(err, httpErr.StatusCode == http.StatusTooManyRequests)
	}
	// A 5xx may come from a gateway that gave up while the server went on,
	// and a dropped connection may follow the server accepting the message,
	// so ask the server what it did.
	state, emailID, lookupErr := jmapSendOutcome(ctx, client, session, account, drafts, sent, raw, sending)
	switch {
	case lookupErr != nil:
		log.Printf("jmap send: could not tell whether the message was sent: %v", lookupErr)
		return "", err
	case state == jmapSent:
		return emailID, nil
	}
	return "", notSent(err, true)
}

type jmapSendState int

const (
	jmapNotSent jmapSendState = iota
	jmapSent
)

// jmapSendOutcome finds out whether a send whose request got no clear answer,
// made since sending, went out. The request stores the message in Drafts and submits it, and a
// submission moves it to Sent, no longer a draft. So the message in Sent, or
// a draft of it with a submission, means it was sent; otherwise nothing was,
// and a stored copy left in Drafts is removed so a retry doesn't add another.
// Gofer's own saved drafts share the Message-ID, so they are told apart by
// their revision header and never removed.
func jmapSendOutcome(ctx context.Context, client *jmap.Client, session *jmap.Session, account, drafts, sent string, raw []byte, sending time.Time) (jmapSendState, string, error) {
	header, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	messageID := message.NormalizeMessageID(header.Header.Get("Message-ID"))
	if messageID == "" {
		return 0, "", errors.New("the message has no Message-ID to look for")
	}
	// The server dates the stored copy itself.
	windows := []jmap.Window{{From: sending.Add(-jmapClockSkew), To: time.Now().Add(jmapClockSkew)}}
	var ids []string
	for _, mailbox := range []string{drafts, sent} {
		if mailbox == "" || (mailbox == sent && sent == drafts) {
			continue
		}
		found, err := client.FindEmails(ctx, session, account, mailbox, messageID, windows)
		if err != nil {
			return 0, "", err
		}
		for _, id := range found {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return jmapNotSent, "", nil
	}
	copies, err := client.GetEmailCopies(ctx, session, account, ids)
	if err != nil {
		return 0, "", err
	}
	var leftovers []string
	for _, email := range copies {
		if strings.TrimSpace(email.DraftRevision) != "" {
			continue
		}
		if !email.Keywords[jmap.KeywordDraft] && (sent == "" || email.MailboxIDs[sent]) {
			return jmapSent, email.ID, nil
		}
		submitted, err := client.HasSubmission(ctx, session, account, email.ID)
		if err != nil {
			return 0, "", err
		}
		if submitted {
			return jmapSent, email.ID, nil
		}
		if email.MailboxIDs[drafts] && email.Size == int64(len(raw)) {
			leftovers = append(leftovers, email.ID)
		}
	}
	if len(leftovers) > 0 {
		result, err := client.SetEmails(ctx, session, jmap.EmailSet{AccountID: account, Destroy: leftovers})
		if err != nil {
			return 0, "", fmt.Errorf("remove the unsent copy: %w", err)
		}
		for id, failure := range result.NotDestroyed {
			if failure.Type != "notFound" {
				return 0, "", fmt.Errorf("remove the unsent copy %s: %w", id, &failure)
			}
		}
	}
	return jmapNotSent, "", nil
}
