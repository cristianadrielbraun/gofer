package jmap

import (
	"context"
	"errors"
	"fmt"
)

// ChangesResponse is the standard /changes result (RFC 8620 section 5.2),
// shared by every object type.
type ChangesResponse struct {
	AccountID      string   `json:"accountId"`
	OldState       string   `json:"oldState"`
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// MailboxChangesResponse adds UpdatedProperties: when set, the updated
// mailboxes changed only in these properties (usually just the counters).
// When nil, anything may have changed.
type MailboxChangesResponse struct {
	ChangesResponse
	UpdatedProperties []string `json:"updatedProperties"`
}

// IsCannotCalculateChanges reports whether the server can no longer compute
// changes from the given state, so the caller must resync from scratch.
func IsCannotCalculateChanges(err error) bool {
	var methodErr *MethodError
	return errors.As(err, &methodErr) && methodErr.Type == "cannotCalculateChanges"
}

// IsUnknownMethod reports whether the server doesn't offer a method.
func IsUnknownMethod(err error) bool {
	var methodErr *MethodError
	return errors.As(err, &methodErr) && methodErr.Type == "unknownMethod"
}

type changesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	MaxChanges int    `json:"maxChanges,omitempty"`
}

// GetMailboxChanges runs Mailbox/changes. maxChanges caps the ids returned
// (0 leaves it to the server); keep calling with NewState while
// HasMoreChanges is true.
func (c *Client) GetMailboxChanges(ctx context.Context, session *Session, accountID, sinceState string, maxChanges int) (*MailboxChangesResponse, error) {
	var result MailboxChangesResponse
	if err := c.getChanges(ctx, session, CapabilityMail, "Mailbox/changes", changesArgs{accountID, sinceState, maxChanges}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetEmailChanges runs Email/changes for the whole account. Updated emails
// changed only their keywords or mailboxIds, since nothing else about an
// Email can change.
func (c *Client) GetEmailChanges(ctx context.Context, session *Session, accountID, sinceState string, maxChanges int) (*ChangesResponse, error) {
	var result ChangesResponse
	if err := c.getChanges(ctx, session, CapabilityMail, "Email/changes", changesArgs{accountID, sinceState, maxChanges}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) getChanges(ctx context.Context, session *Session, capability, method string, args changesArgs, out any) error {
	responses, err := c.Call(ctx, session, []string{capability},
		Invocation{Name: method, Args: args, CallID: "0"})
	if err != nil {
		return err
	}
	if err := responses.Get("0", out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}
