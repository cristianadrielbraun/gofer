package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

const maxResponseBytes = 64 << 20

// Invocation is one method call: ["Name", {args}, "callId"] on the wire.
type Invocation struct {
	Name   string
	Args   any
	CallID string
}

func (i Invocation) MarshalJSON() ([]byte, error) {
	args := i.Args
	if args == nil {
		args = struct{}{}
	}
	return json.Marshal([]any{i.Name, args, i.CallID})
}

// Response is one entry of methodResponses. Args stays raw; the typed
// method wrappers decode it.
type Response struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

func (r *Response) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	if len(parts) != 3 {
		return fmt.Errorf("jmap: method response has %d elements, want 3", len(parts))
	}
	if err := json.Unmarshal(parts[0], &r.Name); err != nil {
		return fmt.Errorf("jmap: method response name: %w", err)
	}
	if err := json.Unmarshal(parts[2], &r.CallID); err != nil {
		return fmt.Errorf("jmap: method response call id: %w", err)
	}
	r.Args = parts[1]
	return nil
}

// Responses is the result of one Call.
type Responses struct {
	List []Response
	// SessionState differs from Session.State when the session changed and
	// should be fetched again.
	SessionState string
}

// MethodError is a method-level error: the server answered one call with
// ["error", {...}, "callId"] while the other calls in the batch still ran.
type MethodError struct {
	CallID      string
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e *MethodError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("jmap: call %s: %s", e.CallID, e.Type)
	}
	return fmt.Sprintf("jmap: call %s: %s: %s", e.CallID, e.Type, e.Description)
}

// Get decodes the response to callID into out (which may be nil). It returns
// a *MethodError when the server answered that call with an error. When a
// call produced several responses (e.g. Foo/copy with onSuccessDestroy), Get
// returns the first.
func (r *Responses) Get(callID string, out any) error {
	for _, resp := range r.List {
		if resp.CallID != callID {
			continue
		}
		if resp.Name == "error" {
			methodErr := &MethodError{CallID: callID}
			if err := json.Unmarshal(resp.Args, methodErr); err != nil {
				return fmt.Errorf("jmap: call %s: decode error: %w", callID, err)
			}
			return methodErr
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Args, out); err != nil {
			return fmt.Errorf("jmap: call %s (%s): decode: %w", callID, resp.Name, err)
		}
		return nil
	}
	return fmt.Errorf("jmap: no response for call %s", callID)
}

// Call sends one JMAP request with the given method calls to the session's
// API URL. The core capability is added to using when missing.
func (c *Client) Call(ctx context.Context, session *Session, using []string, calls ...Invocation) (*Responses, error) {
	if session == nil || session.APIURL == "" {
		return nil, errors.New("jmap: call needs a session with an apiUrl")
	}
	if len(calls) == 0 {
		return nil, errors.New("jmap: call needs at least one method call")
	}
	if limit := session.Core.MaxCallsInRequest; limit > 0 && len(calls) > limit {
		return nil, fmt.Errorf("jmap: %d method calls exceed the server limit of %d", len(calls), limit)
	}
	if !slices.Contains(using, CapabilityCore) {
		using = append([]string{CapabilityCore}, using...)
	}

	body, err := json.Marshal(struct {
		Using       []string     `json:"using"`
		MethodCalls []Invocation `json:"methodCalls"`
	}{using, calls})
	if err != nil {
		return nil, fmt.Errorf("jmap: encode request: %w", err)
	}
	if limit := session.Core.MaxSizeRequest; limit > 0 && int64(len(body)) > limit {
		return nil, fmt.Errorf("jmap: request of %d bytes exceeds the server limit of %d", len(body), limit)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.APIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.Auth != nil {
		c.Auth(req)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newHTTPError(resp)
	}

	var decoded struct {
		MethodResponses []Response `json:"methodResponses"`
		SessionState    string     `json:"sessionState"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("jmap: decode response: %w", err)
	}
	return &Responses{List: decoded.MethodResponses, SessionState: decoded.SessionState}, nil
}

func newHTTPError(resp *http.Response) *HTTPError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	httpErr := &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw)), Challenge: resp.Header.Values("WWW-Authenticate")}
	// Request-level errors are RFC 7807 problem details, e.g.
	// {"type": "urn:ietf:params:jmap:error:limit", "limit": "maxCallsInRequest"}.
	var problem struct {
		Type  string `json:"type"`
		Limit string `json:"limit"`
	}
	if json.Unmarshal(raw, &problem) == nil {
		httpErr.Type = problem.Type
		httpErr.Limit = problem.Limit
	}
	return httpErr
}
