package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PatchObject changes parts of an object in a /set update (RFC 8620 section
// 5.3): each key is a JSON pointer path, and a nil value removes that path.
type PatchObject map[string]any

// KeywordPatch sets or clears one keyword, leaving the email's others alone.
func KeywordPatch(keyword string, on bool) PatchObject {
	var value any
	if on {
		value = true
	}
	return PatchObject{"keywords/" + escapePointer(keyword): value}
}

// MovePatch moves an email out of one mailbox into another, leaving any other
// mailboxes it is in alone.
func MovePatch(from, to string) PatchObject {
	patch := PatchObject{"mailboxIds/" + escapePointer(to): true}
	if from != "" && from != to {
		patch["mailboxIds/"+escapePointer(from)] = nil
	}
	return patch
}

// escapePointer escapes one JSON pointer path segment (RFC 6901).
func escapePointer(segment string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(segment)
}

// SetError is why the server did not create, update or destroy one object.
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Properties  []string `json:"properties"`
	// ExistingID is the object already there, on an alreadyExists error.
	ExistingID string `json:"existingId"`
}

func (e *SetError) Error() string {
	if e.Description == "" {
		return "jmap: " + e.Type
	}
	return fmt.Sprintf("jmap: %s: %s", e.Type, e.Description)
}

// EmailSet are the Email/set arguments Gofer uses: updates by id and ids to
// destroy for good.
type EmailSet struct {
	AccountID string                 `json:"accountId"`
	Update    map[string]PatchObject `json:"update,omitempty"`
	Destroy   []string               `json:"destroy,omitempty"`
}

// SetResponse is the standard /set result. Each id asked about ends up either
// in Updated or Destroyed, or in NotUpdated or NotDestroyed with the reason.
type SetResponse struct {
	AccountID    string                     `json:"accountId"`
	OldState     string                     `json:"oldState"`
	NewState     string                     `json:"newState"`
	Updated      map[string]json.RawMessage `json:"updated"`
	Destroyed    []string                   `json:"destroyed"`
	NotUpdated   map[string]SetError        `json:"notUpdated"`
	NotDestroyed map[string]SetError        `json:"notDestroyed"`
}

// SetEmails runs Email/set.
func (c *Client) SetEmails(ctx context.Context, session *Session, args EmailSet) (*SetResponse, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityMail},
		Invocation{Name: "Email/set", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result SetResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Email/set: %w", err)
	}
	return &result, nil
}

// createOne runs a /set that creates one object and returns its id. what
// names the object in errors, such as "calendar".
func (c *Client) createOne(ctx context.Context, session *Session, capability, method, accountID, what string, object map[string]any) (string, error) {
	responses, err := c.Call(ctx, session, []string{capability},
		Invocation{Name: method, Args: map[string]any{
			"accountId": accountID,
			"create":    map[string]any{"new": object},
		}, CallID: "0"})
	if err != nil {
		return "", err
	}
	var created struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]SetError            `json:"notCreated"`
	}
	if err := responses.Get("0", &created); err != nil {
		return "", fmt.Errorf("%s: %w", method, err)
	}
	if failure, ok := created.NotCreated["new"]; ok {
		return "", fmt.Errorf("the server did not create the %s: %w", what, &failure)
	}
	id := created.Created["new"].ID
	if id == "" {
		return "", fmt.Errorf("jmap: the server did not confirm the %s", what)
	}
	return id, nil
}
