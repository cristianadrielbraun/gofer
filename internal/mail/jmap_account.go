package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

const jmapMaxRedirects = 5

// ErrJMAPSyncUnavailable stops the IMAP sync from running a JMAP account,
// which cannot sync until JMAP has its own sync.
var ErrJMAPSyncUnavailable = errors.New("JMAP mail sync isn't available yet")

// newJMAPAccountClient builds the JMAP client for an account. The exceptions
// in cfg approve plain HTTP or a private address only for the session URL's
// exact scheme, host and port; any other server the session, an API URL or a
// redirect points to must be public and use HTTPS.
func newJMAPAccountClient(cfg *models.AccountConfig, password string) (*jmap.Client, error) {
	if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg.Provider), "jmap") {
		return nil, errors.New("not a JMAP account")
	}
	approved, err := jmap.ParseEndpoint(cfg.JMAPSessionURL)
	if err != nil {
		return nil, fmt.Errorf("JMAP session URL: %w", err)
	}
	if approved.Scheme == "http" && !cfg.JMAPAllowPlaintext {
		return nil, fmt.Errorf("Gofer won't connect to %s without encryption; an administrator can add an HTTP plaintext exception for %s:%d under Mail security", approved, approved.Host, approved.Port)
	}
	allowPrivate := func(_ context.Context, protocol, host string, port int) bool {
		return cfg.JMAPAllowPrivate && protocol == approved.Scheme && jmap.NormalizeHost(host) == approved.Host && port == approved.Port
	}
	transport := jmapPlaintextGuard{
		next:           netguard.NewHTTPTransport(allowPrivate),
		approved:       approved,
		allowPlaintext: cfg.JMAPAllowPlaintext,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= jmapMaxRedirects {
				return errors.New("JMAP server redirected too many times")
			}
			return nil
		},
	}
	return jmapPasswordClient(client, cfg.JMAPSessionURL, cfg.Username, password), nil
}

// jmapTokenServers holds the session URLs whose server asked for a bearer
// token instead of a username and password, so later clients send the token
// right away.
var jmapTokenServers sync.Map

// jmapPasswordClient logs in with the username and password, or, on a server
// that accepts only bearer tokens (Fastmail's API tokens), with the password
// as the token. Tokens never contain spaces, so the ones a paste leaves around
// it are dropped.
func jmapPasswordClient(httpClient *http.Client, sessionURL, username, password string) *jmap.Client {
	token := jmap.BearerAuth(strings.TrimSpace(password))
	if _, ok := jmapTokenServers.Load(sessionURL); ok {
		return &jmap.Client{HTTP: httpClient, Auth: token}
	}
	return &jmap.Client{HTTP: httpClient, Auth: jmap.BasicAuth(username, password), TokenAuth: func(req *http.Request) {
		jmapTokenServers.Store(sessionURL, struct{}{})
		token(req)
	}}
}

// jmapLoginError turns a refused session request into ErrJMAPLoginRejected,
// or ErrJMAPTokenRejected when the server takes only API tokens, adding the
// server's reason when it gave a short one. Other errors come back unchanged.
func jmapLoginError(err error) error {
	var httpErr *jmap.HTTPError
	var rejected error
	switch {
	case !errors.As(err, &httpErr):
		return err
	case httpErr.WantsBearerOnly():
		rejected = ErrJMAPTokenRejected
	case httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden:
		rejected = ErrJMAPLoginRejected
	default:
		return err
	}
	// Only a short plain text body reads as a reason; skip HTML pages.
	reason, _, _ := strings.Cut(httpErr.Body, "\n")
	if reason = strings.TrimSpace(reason); reason == "" || len(reason) > 200 || strings.HasPrefix(reason, "<") {
		return rejected
	}
	return fmt.Errorf("%w (server said: %s)", rejected, reason)
}

// jmapPlaintextGuard refuses every request, including each redirect, that
// would go out without TLS anywhere but the approved endpoint.
type jmapPlaintextGuard struct {
	next           http.RoundTripper
	approved       jmap.Endpoint
	allowPlaintext bool
}

func (g jmapPlaintextGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := jmap.ParseEndpoint(req.URL.String())
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}
	if target.Scheme == "http" && (!g.allowPlaintext || target != g.approved) {
		closeRequestBody(req)
		return nil, fmt.Errorf("Gofer won't send JMAP requests to %s without encryption", target)
	}
	return g.next.RoundTrip(req)
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// TestJMAPConnection fetches a JMAP account's session and checks that it
// offers a mail account.
func TestJMAPConnection(ctx context.Context, cfg *models.AccountConfig, password string) (*jmap.Session, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, jmapLoginError(err)
	}
	if session.PrimaryAccount(jmap.CapabilityMail) == "" {
		return nil, errors.New("the JMAP server offers no mail account for this login")
	}
	return session, nil
}

// ErrJMAPCannotSend means the login reads mail but the server does not let it
// send, as with an API token made without the submission permission.
var ErrJMAPCannotSend = errors.New("the JMAP server doesn't let this login send mail; if you signed in with an API token, create one that also has the email submission permission")

// jmapCanSend reports whether the session lets its primary mail account send.
// The account's own capabilities are checked when the server lists them.
func jmapCanSend(session *jmap.Session) bool {
	if _, ok := session.Capabilities[jmap.CapabilitySubmission]; !ok {
		return false
	}
	account := session.Accounts[session.PrimaryAccount(jmap.CapabilityMail)]
	if len(account.AccountCapabilities) == 0 {
		return true
	}
	_, ok := account.AccountCapabilities[jmap.CapabilitySubmission]
	return ok
}

// JMAPSendingWarning is the connection test warning for a session that cannot
// send mail, or "" when it can.
func JMAPSendingWarning(session *jmap.Session) string {
	if jmapCanSend(session) {
		return ""
	}
	return "Sending won't work: the JMAP server doesn't let this login send mail. If you signed in with an API token, create one that also has the Email submission permission."
}

// JMAPConnectionTestResult is TestJMAPConnection as a connection test result.
func JMAPConnectionTestResult(ctx context.Context, cfg *models.AccountConfig, password string) models.ConnectionTestResult {
	result := models.ConnectionTestResult{Service: "jmap", Message: cfg.JMAPSessionURL}
	session, err := TestJMAPConnection(ctx, cfg, password)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Success = true
	result.Message = fmt.Sprintf("Connected to %s (mail account %s)", cfg.JMAPSessionURL, session.PrimaryAccount(jmap.CapabilityMail))
	result.Warning = JMAPSendingWarning(session)
	return result
}
