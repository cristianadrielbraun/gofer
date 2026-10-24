// Package jmap is a small JMAP (RFC 8620/8621) client covering only what
// Gofer's mail sync needs. It does not import any Gofer package; callers pass
// in the *http.Client so network policy stays with them.
package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CapabilityCore       = "urn:ietf:params:jmap:core"
	CapabilityMail       = "urn:ietf:params:jmap:mail"
	CapabilitySubmission = "urn:ietf:params:jmap:submission"

	maxSessionBytes = 1 << 20
	maxErrorBytes   = 4096
)

// Auth adds credentials to an outgoing request.
type Auth func(*http.Request)

func BasicAuth(username, password string) Auth {
	return func(req *http.Request) { req.SetBasicAuth(username, password) }
}

func BearerAuth(token string) Auth {
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+token) }
}

type Client struct {
	HTTP *http.Client
	Auth Auth
	// TokenAuth, when set, is tried once when the session endpoint refuses
	// Auth and asks only for a bearer token, as Fastmail does; if the session
	// then loads, it replaces Auth for this client.
	TokenAuth Auth

	// StallTimeout gives up a request that sends or receives nothing for
	// this long; zero means DefaultStallTimeout.
	StallTimeout time.Duration
}

// Session is the JMAP session object (RFC 8620 section 2).
type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]Account         `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Username        string                     `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`

	// Core holds the server limits from the core capability.
	Core CoreCapability `json:"-"`
}

type Account struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

type CoreCapability struct {
	MaxSizeUpload         int64 `json:"maxSizeUpload"`
	MaxConcurrentUpload   int   `json:"maxConcurrentUpload"`
	MaxSizeRequest        int64 `json:"maxSizeRequest"`
	MaxConcurrentRequests int   `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int   `json:"maxCallsInRequest"`
	MaxObjectsInGet       int   `json:"maxObjectsInGet"`
	MaxObjectsInSet       int   `json:"maxObjectsInSet"`
}

// HTTPError is a non-2xx response from the server. Type and Limit are set
// when the body is a JMAP problem details object.
type HTTPError struct {
	StatusCode int
	Body       string
	Type       string
	Limit      string
	// Challenge holds the response's WWW-Authenticate headers.
	Challenge []string
}

// WantsBearerOnly reports whether the server refused the login and offered
// only bearer token authentication in its challenge.
func (e *HTTPError) WantsBearerOnly() bool {
	if e.StatusCode != http.StatusUnauthorized {
		return false
	}
	var bearer bool
	for _, header := range e.Challenge {
		for _, part := range strings.Split(header, ",") {
			// A challenge starts with its scheme; the other parts are
			// name=value parameters.
			scheme, _, _ := strings.Cut(strings.TrimSpace(part), " ")
			if scheme == "" || strings.Contains(scheme, "=") {
				continue
			}
			switch strings.ToLower(scheme) {
			case "bearer":
				bearer = true
			case "basic":
				return false
			}
		}
	}
	return bearer
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("jmap: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("jmap: HTTP %d: %s", e.StatusCode, e.Body)
}

// PrimaryAccount returns the primary account id for a capability, or "".
func (s *Session) PrimaryAccount(capability string) string {
	return s.PrimaryAccounts[capability]
}

// Session fetches the session object. sessionURL is usually
// https://host/.well-known/jmap; redirects are followed.
func (c *Client) Session(ctx context.Context, sessionURL string) (*Session, error) {
	session, err := c.session(ctx, sessionURL, c.Auth)
	var httpErr *HTTPError
	if c.TokenAuth != nil && errors.As(err, &httpErr) && httpErr.WantsBearerOnly() {
		if session, err = c.session(ctx, sessionURL, c.TokenAuth); err == nil {
			c.Auth, c.TokenAuth = c.TokenAuth, nil
		}
	}
	return session, err
}

func (c *Client) session(ctx context.Context, sessionURL string, auth Auth) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth != nil {
		auth(req)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newHTTPError(resp)
	}

	var session Session
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSessionBytes)).Decode(&session); err != nil {
		return nil, fmt.Errorf("jmap: decode session: %w", err)
	}
	if raw, ok := session.Capabilities[CapabilityCore]; ok {
		if err := json.Unmarshal(raw, &session.Core); err != nil {
			return nil, fmt.Errorf("jmap: decode core capability: %w", err)
		}
	}

	// Servers may send these relative; resolve them against the URL the
	// session actually came from (after redirects).
	base := resp.Request.URL
	for _, field := range []*string{&session.APIURL, &session.DownloadURL, &session.UploadURL, &session.EventSourceURL} {
		*field = resolveTemplateURL(base, *field)
	}
	if session.APIURL == "" {
		return nil, fmt.Errorf("jmap: session has no apiUrl")
	}
	return &session, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// resolveTemplateURL makes a possibly relative URL absolute. It works on the
// string because downloadUrl and friends are URI templates, and url.URL would
// escape their {placeholders}.
func resolveTemplateURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "://") {
		return ref
	}
	origin := base.Scheme + "://" + base.Host
	if strings.HasPrefix(ref, "/") {
		return origin + ref
	}
	dir := base.Path[:strings.LastIndex(base.Path, "/")+1]
	if dir == "" {
		dir = "/"
	}
	return origin + dir + ref
}
