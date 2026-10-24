package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

const jmapProbeTimeout = 10 * time.Second

// JMAPEndpointPolicy reports what an administrator approved for one endpoint:
// plain HTTP and a private address.
type JMAPEndpointPolicy func(ctx context.Context, endpoint jmap.Endpoint) (allowPlaintext, allowPrivate bool, err error)

// JMAPAttempt is one candidate that did not work, and why.
type JMAPAttempt struct {
	URL    string
	Reason string
}

// JMAPDiscoveryError lists every candidate tried when none worked.
type JMAPDiscoveryError struct {
	Attempts []JMAPAttempt
}

func (e *JMAPDiscoveryError) Error() string {
	if len(e.Attempts) == 0 {
		return "no JMAP server to try"
	}
	parts := make([]string, len(e.Attempts))
	for i, attempt := range e.Attempts {
		parts[i] = attempt.URL + ": " + attempt.Reason
	}
	return "no JMAP mail server found (" + strings.Join(parts, "; ") + ")"
}

// ErrJMAPLoginRejected means a JMAP server answered but refused the username
// or password.
var ErrJMAPLoginRejected = errors.New("the JMAP server rejected the username or password")

// ErrJMAPTokenRejected means a JMAP server accepts only API tokens and
// refused the one entered as the password, or the password was not a token.
var ErrJMAPTokenRejected = errors.New("the JMAP server needs an API token instead of the password: create one in your mail provider's settings and enter it as the password")

// DiscoverJMAPSession tries the candidate session URLs in order with the
// account's credentials and returns the first that is a JMAP server with a
// mail account. A plain HTTP candidate is contacted only where policy approves
// it, so the password never goes out unencrypted unless an administrator
// allowed that endpoint. A rejected login ends the search: the server was
// found, and the password should not go on to the other candidates.
func DiscoverJMAPSession(ctx context.Context, candidates []string, username, password string, policy JMAPEndpointPolicy) (string, *jmap.Session, error) {
	failed := &JMAPDiscoveryError{}
	for _, candidate := range candidates {
		endpoint, err := jmap.ParseEndpoint(candidate)
		if err != nil {
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, err.Error()})
			continue
		}
		var allowPlaintext, allowPrivate bool
		if policy != nil {
			if allowPlaintext, allowPrivate, err = policy(ctx, endpoint); err != nil {
				return "", nil, err
			}
		}
		if endpoint.Scheme == "http" && !allowPlaintext {
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, fmt.Sprintf("not tried: plain HTTP needs an HTTP plaintext exception for %s:%d", endpoint.Host, endpoint.Port)})
			continue
		}
		client, err := newJMAPAccountClient(&models.AccountConfig{
			Provider:           "jmap",
			JMAPSessionURL:     candidate,
			Username:           username,
			JMAPAllowPlaintext: allowPlaintext,
			JMAPAllowPrivate:   allowPrivate,
		}, password)
		if err != nil {
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, err.Error()})
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, jmapProbeTimeout)
		session, err := client.Session(probeCtx, candidate)
		cancel()
		switch loginErr := jmapLoginError(err); {
		case errors.Is(loginErr, ErrJMAPLoginRejected) || errors.Is(loginErr, ErrJMAPTokenRejected):
			return "", nil, fmt.Errorf("%s: %w", candidate, loginErr)
		case netguard.IsBlocked(err):
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, fmt.Sprintf("on a private network: needs a %s private target exception for %s:%d", strings.ToUpper(endpoint.Scheme), endpoint.Host, endpoint.Port)})
			continue
		case err != nil:
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, err.Error()})
			continue
		}
		if _, ok := session.Capabilities[jmap.CapabilityCore]; !ok {
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, "not a JMAP session"})
			continue
		}
		if session.PrimaryAccount(jmap.CapabilityMail) == "" {
			failed.Attempts = append(failed.Attempts, JMAPAttempt{candidate, "the server offers no mail account for this login"})
			continue
		}
		return candidate, session, nil
	}
	return "", nil, failed
}
