package jmap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// ImportError is the server refusing to store a message or to create,
// rename, move or delete a mailbox. Nothing was changed, and sending the same
// request again won't change that.
type ImportError struct {
	Step string
	SetError
}

func (e *ImportError) Error() string {
	return "jmap: " + e.Step + ": " + e.SetError.Error()
}

// Imported is a message stored by ImportEmail. Existed says the server
// already had it and returned that copy instead of storing another.
type Imported struct {
	ID      string
	Existed bool
}

// ImportEmail stores an uploaded message in one mailbox with the keywords
// given, received at receivedAt when it isn't zero, so it sorts by its own
// date rather than the time of the import. A server that keeps one copy of
// each message answers alreadyExists for one it has; that copy is returned.
func (c *Client) ImportEmail(ctx context.Context, session *Session, accountID, blobID, mailboxID string, keywords []string, receivedAt time.Time) (*Imported, error) {
	if blobID == "" || mailboxID == "" {
		return nil, errors.New("jmap: importing a message needs a blob and a mailbox")
	}
	email := map[string]any{
		"blobId":     blobID,
		"mailboxIds": map[string]bool{mailboxID: true},
	}
	set := map[string]bool{}
	for _, keyword := range keywords {
		set[keyword] = true
	}
	email["keywords"] = set
	if !receivedAt.IsZero() {
		email["receivedAt"] = receivedAt.UTC().Format(time.RFC3339)
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/import", Args: map[string]any{
			"accountId": accountID,
			"emails":    map[string]any{"email": email},
		}, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var imported struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	if err := responses.Get("0", &imported); err != nil {
		return nil, fmt.Errorf("Email/import: %w", err)
	}
	if failure, ok := imported.NotCreated["email"]; ok {
		if failure.Type == "alreadyExists" && failure.ExistingID != "" {
			return &Imported{ID: failure.ExistingID, Existed: true}, nil
		}
		return nil, &ImportError{Step: "import", SetError: failure}
	}
	id := imported.Created["email"].ID
	if id == "" {
		return nil, errors.New("jmap: the server did not confirm the import")
	}
	return &Imported{ID: id}, nil
}

// Window is a span of receivedAt times, From included and To not.
type Window struct {
	From, To time.Time
}

// findEmailsLimit is how many emails each FindEmails search reads.
const findEmailsLimit = 100

// FindEmails returns the emails in a mailbox whose Message-ID is messageID,
// given without angle brackets, to tell whether a message whose import got
// no answer was stored. It searches by the Message-ID header and by each
// receivedAt window in one request, as far as the server's call limit
// allows: header search is optional and may lag behind a message just
// stored, and a server may not keep the receivedAt it was given. Results are
// compared by Message-ID exactly. It fails only when every search fails. On
// a server taking one call per request, each search takes two requests.
func (c *Client) FindEmails(ctx context.Context, session *Session, accountID, mailboxID, messageID string, windows []Window) ([]string, error) {
	if mailboxID == "" || messageID == "" {
		return nil, errors.New("jmap: finding an email needs a mailbox and a Message-ID")
	}
	filters := []*EmailFilter{{InMailbox: mailboxID, Header: []string{"Message-ID", messageID}}}
	for _, window := range windows {
		from, to := window.From.UTC().Truncate(time.Second), window.To.UTC().Truncate(time.Second).Add(time.Second)
		filters = append(filters, &EmailFilter{InMailbox: mailboxID, After: &from, Before: &to})
	}
	var found []string
	var failures []error
	answered := 0
	match := func(emails []Email) {
		answered++
		for _, email := range emails {
			for _, id := range email.MessageID {
				if id == messageID && !slices.Contains(found, email.ID) {
					found = append(found, email.ID)
				}
			}
		}
	}
	query := func(filter *EmailFilter) EmailQuery {
		return EmailQuery{AccountID: accountID, Filter: filter, Sort: []Comparator{{Property: "receivedAt"}}, Limit: findEmailsLimit}
	}
	if session.Core.MaxCallsInRequest == 1 {
		// A query and the get of its results can't share a request, so each
		// search takes two.
		for _, filter := range filters {
			page, err := c.QueryEmails(ctx, session, query(filter))
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if len(page.IDs) == 0 {
				match(nil)
				continue
			}
			emails, err := c.GetEmails(ctx, session, accountID, page.IDs, []string{"id", "messageId"})
			if err != nil {
				failures = append(failures, err)
				continue
			}
			match(emails.List)
		}
		if answered == 0 {
			return nil, errors.Join(failures...)
		}
		return found, nil
	}
	perRequest := len(filters)
	if limit := session.Core.MaxCallsInRequest; limit > 0 {
		perRequest = max(1, min(perRequest, limit/2))
	}
	for start := 0; start < len(filters); start += perRequest {
		batch := filters[start:min(start+perRequest, len(filters))]
		var calls []Invocation
		for i, filter := range batch {
			queryID, get := fmt.Sprintf("q%d", i), fmt.Sprintf("g%d", i)
			calls = append(calls,
				Invocation{Name: "Email/query", Args: query(filter), CallID: queryID},
				Invocation{Name: "Email/get", Args: map[string]any{
					"accountId":  accountID,
					"#ids":       ResultReference{ResultOf: queryID, Name: "Email/query", Path: "/ids"},
					"properties": []string{"id", "messageId"},
				}, CallID: get},
			)
		}
		responses, err := c.Call(ctx, session, []string{CapabilityMail}, calls...)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for i := range batch {
			var emails GetResponse[Email]
			if err := responses.Get(fmt.Sprintf("g%d", i), &emails); err != nil {
				failures = append(failures, fmt.Errorf("Email/get: %w", err))
				continue
			}
			match(emails.List)
		}
	}
	if answered == 0 {
		return nil, errors.Join(failures...)
	}
	return found, nil
}

// CreateMailbox creates a mailbox named name inside parentID, or at the top
// when parentID is "", and returns its id.
func (c *Client) CreateMailbox(ctx context.Context, session *Session, accountID, name, parentID string) (string, error) {
	if name == "" {
		return "", errors.New("jmap: a mailbox needs a name")
	}
	mailbox := map[string]any{"name": name, "parentId": nil}
	if parentID != "" {
		mailbox["parentId"] = parentID
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Mailbox/set", Args: map[string]any{
			"accountId": accountID,
			"create":    map[string]any{"mailbox": mailbox},
		}, CallID: "0"})
	if err != nil {
		return "", err
	}
	var created struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	if err := responses.Get("0", &created); err != nil {
		return "", fmt.Errorf("Mailbox/set: %w", err)
	}
	if failure, ok := created.NotCreated["mailbox"]; ok {
		return "", &ImportError{Step: "create mailbox", SetError: failure}
	}
	id := created.Created["mailbox"].ID
	if id == "" {
		return "", errors.New("jmap: the server did not confirm the mailbox")
	}
	return id, nil
}

// RenameMailbox gives a mailbox a new name, keeping its parent.
func (c *Client) RenameMailbox(ctx context.Context, session *Session, accountID, id, name string) error {
	if name == "" {
		return errors.New("jmap: a mailbox needs a name")
	}
	return c.updateMailbox(ctx, session, accountID, id, "rename mailbox", map[string]any{"name": name})
}

// MoveMailbox puts a mailbox inside parentID, or at the top when it's "",
// named name there.
func (c *Client) MoveMailbox(ctx context.Context, session *Session, accountID, id, parentID, name string) error {
	if name == "" {
		return errors.New("jmap: a mailbox needs a name")
	}
	patch := map[string]any{"name": name, "parentId": nil}
	if parentID != "" {
		patch["parentId"] = parentID
	}
	return c.updateMailbox(ctx, session, accountID, id, "move mailbox", patch)
}

func (c *Client) updateMailbox(ctx context.Context, session *Session, accountID, id, step string, patch map[string]any) error {
	if id == "" {
		return errors.New("jmap: a mailbox is required")
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Mailbox/set", Args: map[string]any{
			"accountId": accountID,
			"update":    map[string]any{id: patch},
		}, CallID: "0"})
	if err != nil {
		return err
	}
	var updated struct {
		NotUpdated map[string]SetError `json:"notUpdated"`
	}
	if err := responses.Get("0", &updated); err != nil {
		return fmt.Errorf("Mailbox/set: %w", err)
	}
	if failure, ok := updated.NotUpdated[id]; ok {
		return &ImportError{Step: step, SetError: failure}
	}
	return nil
}

// DestroyMailboxes deletes mailboxes and the emails only they hold, in the
// order given, so children listed before their parents go first. Each is
// its own Mailbox/set in one request: a server refuses a mailbox that still
// has children.
func (c *Client) DestroyMailboxes(ctx context.Context, session *Session, accountID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	calls := make([]Invocation, 0, len(ids))
	for i, id := range ids {
		calls = append(calls, Invocation{Name: "Mailbox/set", Args: map[string]any{
			"accountId":             accountID,
			"destroy":               []string{id},
			"onDestroyRemoveEmails": true,
		}, CallID: strconv.Itoa(i)})
	}
	responses, err := c.Call(ctx, session, []string{CapabilityMail}, calls...)
	if err != nil {
		return err
	}
	for i, id := range ids {
		var destroyed struct {
			NotDestroyed map[string]SetError `json:"notDestroyed"`
		}
		if err := responses.Get(strconv.Itoa(i), &destroyed); err != nil {
			return fmt.Errorf("Mailbox/set: %w", err)
		}
		if failure, ok := destroyed.NotDestroyed[id]; ok {
			return &ImportError{Step: "delete mailbox", SetError: failure}
		}
	}
	return nil
}
