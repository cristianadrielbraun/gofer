package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrUploadTooLarge is data over the server's upload limit.
var ErrUploadTooLarge = errors.New("jmap: too large to upload")

// UploadBlob uploads data to the session's uploadUrl and returns its blob id.
// Data over the server's upload limit is refused before sending.
func (c *Client) UploadBlob(ctx context.Context, session *Session, accountID, contentType string, data []byte) (string, error) {
	if session == nil || session.UploadURL == "" {
		return "", errors.New("jmap: session has no uploadUrl")
	}
	if limit := session.Core.MaxSizeUpload; limit > 0 && int64(len(data)) > limit {
		return "", fmt.Errorf("%w: %d bytes exceed the server's upload limit of %d", ErrUploadTooLarge, len(data), limit)
	}
	endpoint := expandTemplate(session.UploadURL, map[string]string{"accountId": accountID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.Auth != nil {
		c.Auth(req)
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newHTTPError(resp)
	}
	var uploaded struct {
		BlobID string `json:"blobId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBytes)).Decode(&uploaded); err != nil {
		return "", fmt.Errorf("jmap: decode upload: %w", err)
	}
	if uploaded.BlobID == "" {
		return "", errors.New("jmap: upload returned no blob id")
	}
	return uploaded.BlobID, nil
}

// Identity is an address the account may send from (RFC 8621 section 6). An
// email of "*@example.test" allows any address at that domain.
type Identity struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Email   string         `json:"email"`
	ReplyTo []EmailAddress `json:"replyTo"`
}

// GetIdentities runs Identity/get for every identity of the account.
func (c *Client) GetIdentities(ctx context.Context, session *Session, accountID string) (*GetResponse[Identity], error) {
	args := struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{AccountID: accountID}
	responses, err := c.Call(ctx, session, []string{CapabilityMail, CapabilitySubmission},
		Invocation{Name: "Identity/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[Identity]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Identity/get: %w", err)
	}
	return &result, nil
}

// MatchIdentity picks the identity to send from address with: one with that
// exact address, or else one covering its whole domain.
func MatchIdentity(identities []Identity, address string) (Identity, bool) {
	address = strings.ToLower(strings.TrimSpace(address))
	at := strings.LastIndex(address, "@")
	if address == "" || at < 0 {
		return Identity{}, false
	}
	for _, identity := range identities {
		if strings.ToLower(strings.TrimSpace(identity.Email)) == address {
			return identity, true
		}
	}
	for _, identity := range identities {
		if strings.ToLower(strings.TrimSpace(identity.Email)) == "*"+address[at:] {
			return identity, true
		}
	}
	return Identity{}, false
}

// Envelope is the SMTP envelope of a submission: who it is from and who
// receives it, including Bcc recipients. The server strips Bcc headers during
// delivery (RFC 8621 section 7.5), preserving them in the stored email.
type Envelope struct {
	MailFrom string
	RcptTo   []string
}

func (e Envelope) MarshalJSON() ([]byte, error) {
	type address struct {
		Email string `json:"email"`
	}
	rcpt := make([]address, len(e.RcptTo))
	for i, to := range e.RcptTo {
		rcpt[i] = address{Email: to}
	}
	return json.Marshal(struct {
		MailFrom address   `json:"mailFrom"`
		RcptTo   []address `json:"rcptTo"`
	}{address{Email: e.MailFrom}, rcpt})
}

// Outgoing is one message to send: the uploaded original, who sends it, to
// whom, and where it waits (Drafts) and ends up once sent (Sent).
type Outgoing struct {
	BlobID     string
	IdentityID string
	Envelope   Envelope
	DraftsID   string
	SentID     string
}

// Sent is a message the server accepted for delivery.
type Sent struct {
	EmailID      string
	SubmissionID string
}

// SubmissionError is the server refusing a message: importing it as an email
// or submitting it for delivery. Nothing was sent.
type SubmissionError struct {
	Step string
	SetError
}

func (e *SubmissionError) Error() string {
	return "jmap: " + e.Step + ": " + e.SetError.Error()
}

// refusedMethod turns a method-level error answering one call of a send into
// a *SubmissionError: the server made none of that method's changes (RFC 8620
// section 3.6.2). serverPartialFail stays a plain error, since some of them
// may have happened, as does any answer that isn't a method error.
func refusedMethod(step, method string, err error) error {
	var methodErr *MethodError
	if errors.As(err, &methodErr) && methodErr.Type != "serverPartialFail" {
		return &SubmissionError{Step: step, SetError: SetError{Type: methodErr.Type, Description: methodErr.Description}}
	}
	return fmt.Errorf("%s: %w", method, err)
}

// SendEmail sends an uploaded message in one request: Email/import stores it
// in Drafts, and EmailSubmission/set submits it, moving it to Sent, read and
// no longer a draft, once the server accepts it. If the submission is refused,
// the imported email is removed again, so retries don't pile up drafts. A
// *SubmissionError means nothing was sent; any other error may come after the
// server accepted the message.
func (c *Client) SendEmail(ctx context.Context, session *Session, accountID string, out Outgoing) (*Sent, error) {
	if out.BlobID == "" || out.IdentityID == "" || out.DraftsID == "" {
		return nil, errors.New("jmap: sending needs a blob, an identity and a Drafts mailbox")
	}
	importArgs := map[string]any{
		"accountId": accountID,
		"emails": map[string]any{"draft": map[string]any{
			"blobId":     out.BlobID,
			"mailboxIds": map[string]bool{out.DraftsID: true},
			"keywords":   map[string]bool{KeywordDraft: true, KeywordSeen: true},
		}},
	}
	sentPatch := PatchObject{"keywords/" + escapePointer(KeywordDraft): nil}
	if out.SentID != "" && out.SentID != out.DraftsID {
		sentPatch = PatchObject{
			"mailboxIds/" + escapePointer(out.DraftsID): nil,
			"mailboxIds/" + escapePointer(out.SentID):   true,
			"keywords/" + escapePointer(KeywordDraft):   nil,
		}
	}
	submitArgs := map[string]any{
		"accountId": accountID,
		"create": map[string]any{"send": map[string]any{
			"emailId":    "#draft",
			"identityId": out.IdentityID,
			"envelope":   out.Envelope,
		}},
		"onSuccessUpdateEmail": map[string]PatchObject{"#send": sentPatch},
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail, CapabilitySubmission},
		Invocation{Name: "Email/import", Args: importArgs, CallID: "0"},
		Invocation{Name: "EmailSubmission/set", Args: submitArgs, CallID: "1"},
	)
	if err != nil {
		return nil, err
	}
	var imported struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	if err := responses.Get("0", &imported); err != nil {
		return nil, refusedMethod("import", "Email/import", err)
	}
	if failure, ok := imported.NotCreated["draft"]; ok {
		return nil, &SubmissionError{Step: "import", SetError: failure}
	}
	emailID := imported.Created["draft"].ID
	var submitted struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	removeDraft := func() {
		if emailID != "" {
			// Best effort: the refusal is what matters, not the cleanup.
			_, _ = c.SetEmails(ctx, session, EmailSet{AccountID: accountID, Destroy: []string{emailID}})
		}
	}
	if err := responses.Get("1", &submitted); err != nil {
		err = refusedMethod("submission", "EmailSubmission/set", err)
		var refused *SubmissionError
		if errors.As(err, &refused) {
			removeDraft()
		}
		return nil, err
	}
	if failure, ok := submitted.NotCreated["send"]; ok {
		removeDraft()
		return nil, &SubmissionError{Step: "submission", SetError: failure}
	}
	submission := submitted.Created["send"].ID
	if submission == "" {
		return nil, errors.New("jmap: the server did not confirm the submission")
	}
	return &Sent{EmailID: emailID, SubmissionID: submission}, nil
}

// EmailCopy is what tells whether an email is the copy a send stored: where
// it is, whether it is still a draft, its size, and the revision header that
// Gofer's own saved drafts carry and sent messages don't.
type EmailCopy struct {
	ID            string          `json:"id"`
	MailboxIDs    map[string]bool `json:"mailboxIds"`
	Keywords      map[string]bool `json:"keywords"`
	Size          int64           `json:"size"`
	DraftRevision string          `json:"header:X-Gofer-Draft-Revision:asText"`
}

// GetEmailCopies reads ids as EmailCopy values.
func (c *Client) GetEmailCopies(ctx context.Context, session *Session, accountID string, ids []string) ([]EmailCopy, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/get", Args: map[string]any{
			"accountId":  accountID,
			"ids":        ids,
			"properties": []string{"id", "mailboxIds", "keywords", "size", "header:" + DraftRevisionHeader + ":asText"},
		}, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[EmailCopy]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Email/get: %w", err)
	}
	return result.List, nil
}

// HasSubmission reports whether the server has a submission of emailID, that
// is, whether it was handed over for delivery.
func (c *Client) HasSubmission(ctx context.Context, session *Session, accountID, emailID string) (bool, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityMail, CapabilitySubmission},
		Invocation{Name: "EmailSubmission/query", Args: map[string]any{
			"accountId": accountID,
			"filter":    map[string]any{"emailIds": []string{emailID}},
		}, CallID: "0"})
	if err != nil {
		return false, err
	}
	var result QueryResponse
	if err := responses.Get("0", &result); err != nil {
		return false, fmt.Errorf("EmailSubmission/query: %w", err)
	}
	return len(result.IDs) > 0, nil
}
