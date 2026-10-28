package jmap

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Email is the subset of RFC 8621 section 4 that sync reads. Bodies are not
// here: the raw message is downloaded through BlobID instead.
type Email struct {
	ID            string          `json:"id"`
	BlobID        string          `json:"blobId"`
	ThreadID      string          `json:"threadId"`
	MailboxIDs    map[string]bool `json:"mailboxIds"`
	Keywords      map[string]bool `json:"keywords"`
	Size          int64           `json:"size"`
	ReceivedAt    time.Time       `json:"receivedAt"`
	MessageID     []string        `json:"messageId"`
	InReplyTo     []string        `json:"inReplyTo"`
	References    []string        `json:"references"`
	From          []EmailAddress  `json:"from"`
	To            []EmailAddress  `json:"to"`
	CC            []EmailAddress  `json:"cc"`
	BCC           []EmailAddress  `json:"bcc"`
	ReplyTo       []EmailAddress  `json:"replyTo"`
	Subject       string          `json:"subject"`
	SentAt        time.Time       `json:"sentAt"`
	HasAttachment bool            `json:"hasAttachment"`
	Preview       string          `json:"preview"`
	// ListID is the List-Id header, decoded; empty when there is none.
	ListID string `json:"header:List-Id:asText"`
}

type EmailAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Standard keywords (RFC 8621 section 4.1.1).
const (
	KeywordSeen     = "$seen"
	KeywordFlagged  = "$flagged"
	KeywordDraft    = "$draft"
	KeywordAnswered = "$answered"
)

// EmailSummaryProperties are the Email properties sync needs for a message
// list, without any body content.
var EmailSummaryProperties = []string{
	"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt",
	"messageId", "inReplyTo", "references", "from", "to", "cc", "bcc", "replyTo",
	"subject", "sentAt", "hasAttachment", "preview", "header:List-Id:asText",
}

// EmailFilter is a FilterCondition for Email/query. Only the conditions Gofer
// uses are here.
type EmailFilter struct {
	InMailbox string     `json:"inMailbox,omitempty"`
	After     *time.Time `json:"after,omitempty"`
	Before    *time.Time `json:"before,omitempty"`
	// Header is a header name and a value it must contain. Servers need
	// not support it.
	Header []string `json:"header,omitempty"`
}

type Comparator struct {
	Property    string `json:"property"`
	IsAscending bool   `json:"isAscending"`
}

type EmailQuery struct {
	AccountID string       `json:"accountId"`
	Filter    *EmailFilter `json:"filter,omitempty"`
	Sort      []Comparator `json:"sort,omitempty"`
	Position  int          `json:"position,omitempty"`
	// Anchor starts the page at that email (plus AnchorOffset) instead of at
	// Position, so emails added or removed earlier in the results while paging
	// cannot shift a page past one.
	Anchor         string `json:"anchor,omitempty"`
	AnchorOffset   int    `json:"anchorOffset,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	CalculateTotal bool   `json:"calculateTotal,omitempty"`
}

// IsAnchorNotFound reports whether a query's anchor email no longer exists.
func IsAnchorNotFound(err error) bool {
	var methodErr *MethodError
	return errors.As(err, &methodErr) && methodErr.Type == "anchorNotFound"
}

// QueryResponse is the standard /query result (RFC 8620 section 5.5).
type QueryResponse struct {
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int      `json:"position"`
	IDs                 []string `json:"ids"`
	Total               int      `json:"total"`
	Limit               int      `json:"limit"`
}

// ResultReference points an argument at the result of an earlier call in
// the same request (RFC 8620 section 3.7).
type ResultReference struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// QueryEmails runs Email/query and returns the matching ids.
func (c *Client) QueryEmails(ctx context.Context, session *Session, query EmailQuery) (*QueryResponse, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/query", Args: query, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result QueryResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Email/query: %w", err)
	}
	return &result, nil
}

// GetEmails runs Email/get for ids. A nil properties asks for the server's
// default set, which includes bodies; sync passes EmailSummaryProperties.
func (c *Client) GetEmails(ctx context.Context, session *Session, accountID string, ids, properties []string) (*GetResponse[Email], error) {
	args := struct {
		AccountID  string   `json:"accountId"`
		IDs        []string `json:"ids"`
		Properties []string `json:"properties,omitempty"`
	}{accountID, ids, properties}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[Email]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Email/get: %w", err)
	}
	return &result, nil
}

// QueryAndGetEmails runs Email/query and Email/get in one request: the get
// takes its ids from the query's result through a result reference, so a
// page of messages costs a single round trip.
func (c *Client) QueryAndGetEmails(ctx context.Context, session *Session, query EmailQuery, properties []string) (*QueryResponse, *GetResponse[Email], error) {
	getArgs := struct {
		AccountID  string          `json:"accountId"`
		IDsRef     ResultReference `json:"#ids"`
		Properties []string        `json:"properties,omitempty"`
	}{
		AccountID:  query.AccountID,
		IDsRef:     ResultReference{ResultOf: "0", Name: "Email/query", Path: "/ids"},
		Properties: properties,
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/query", Args: query, CallID: "0"},
		Invocation{Name: "Email/get", Args: getArgs, CallID: "1"},
	)
	if err != nil {
		return nil, nil, err
	}
	var queried QueryResponse
	if err := responses.Get("0", &queried); err != nil {
		return nil, nil, fmt.Errorf("Email/query: %w", err)
	}
	var got GetResponse[Email]
	if err := responses.Get("1", &got); err != nil {
		return nil, nil, fmt.Errorf("Email/get: %w", err)
	}
	return &queried, &got, nil
}
