package jmap

import (
	"context"
	"fmt"
)

// CopyEmail copies one email from one account into a mailbox of another
// (Email/copy, RFC 8621 section 4.7), with the keywords given, as the first
// half of moving it between accounts, such as from the login's own mailbox
// into one shared with it. It returns the new email's id. The original is
// left for the caller to remove, so that it can check it was.
func (c *Client) CopyEmail(ctx context.Context, session *Session, fromAccount, toAccount, emailID, toMailbox string, keywords map[string]bool) (string, error) {
	if keywords == nil {
		keywords = map[string]bool{}
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/copy", Args: map[string]any{
			"fromAccountId": fromAccount,
			"accountId":     toAccount,
			"create": map[string]any{"copy": map[string]any{
				"id":         emailID,
				"mailboxIds": map[string]bool{toMailbox: true},
				"keywords":   keywords,
			}},
		}, CallID: "0"})
	if err != nil {
		return "", err
	}
	var copied struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	if err := responses.Get("0", &copied); err != nil {
		return "", fmt.Errorf("Email/copy: %w", err)
	}
	if failure, ok := copied.NotCreated["copy"]; ok {
		// An alreadyExists error names the email the account already has;
		// callers find it with errors.As.
		return "", fmt.Errorf("the server did not move the email: %w", &failure)
	}
	id := copied.Created["copy"].ID
	if id == "" {
		return "", fmt.Errorf("jmap: the server did not confirm the moved email")
	}
	return id, nil
}
