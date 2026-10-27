package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

// davHTTPTransport carries every CalDAV and CardDAV request. People enter
// those servers themselves, so connections go through netguard and reach a
// private address only when the mail security exceptions approve it. Tests
// replace the transport to reach their local servers.
var davHTTPTransport http.RoundTripper = netguard.NewHTTPTransport(davPrivateTargetAllowed)

var davPrivateTargetPolicy atomic.Pointer[netguard.PrivatePolicy]

// setDAVPrivateTargetPolicy points DAV connections at the instance's mail
// security exceptions. The transport is shared by every request, so the
// policy is set once, when the handler is built.
func setDAVPrivateTargetPolicy(policy netguard.PrivatePolicy) {
	davPrivateTargetPolicy.Store(&policy)
}

func davPrivateTargetAllowed(ctx context.Context, protocol, host string, port int) bool {
	policy := davPrivateTargetPolicy.Load()
	return policy != nil && *policy != nil && (*policy)(ctx, protocol, host, port)
}

// calDAVHTTPTransport and cardDAVHTTPTransport carry every CalDAV and
// CardDAV request. Requests go out over HTTPS, or over plain HTTP only to a
// host and port an administrator approved with an HTTP plaintext exception,
// as JMAP servers do. The check runs on each request, so it covers redirects
// as well.
var (
	calDAVHTTPTransport  http.RoundTripper = davPlaintextGuard{service: "CalDAV"}
	cardDAVHTTPTransport http.RoundTripper = davPlaintextGuard{service: "CardDAV"}
)

var davPlaintextPolicy atomic.Pointer[func(context.Context, string, int) bool]

// setDAVPlaintextPolicy points the DAV plain HTTP check at the instance's
// mail security exceptions, once, when the handler is built.
func setDAVPlaintextPolicy(allowed func(ctx context.Context, host string, port int) bool) {
	davPlaintextPolicy.Store(&allowed)
}

func davPlaintextAllowed(ctx context.Context, host string, port int) bool {
	policy := davPlaintextPolicy.Load()
	return policy != nil && *policy != nil && (*policy)(ctx, host, port)
}

// calDAVURLScheme reports whether a CalDAV URL may use scheme at all; whether
// a plain HTTP server is approved is checked when connecting.
func calDAVURLScheme(scheme string) bool {
	return strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http")
}

var davServerPolicy atomic.Pointer[func(context.Context, string) error]

// setDAVServerPolicy points DAV connections at the allowed mail servers,
// once, when the handler is built.
func setDAVServerPolicy(check func(ctx context.Context, host string) error) {
	davServerPolicy.Store(&check)
}

func davServerCheck(ctx context.Context, host string) error {
	policy := davServerPolicy.Load()
	if policy == nil || *policy == nil {
		return nil
	}
	return (*policy)(ctx, host)
}

// davPlaintextGuard refuses plain HTTP to a DAV server nobody approved, and
// any request to a server the allowed mail servers leave out.
type davPlaintextGuard struct{ service string }

func (g davPlaintextGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := davServerCheck(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	switch strings.ToLower(req.URL.Scheme) {
	case "https":
	case "http":
		host := req.URL.Hostname()
		port, _ := strconv.Atoi(davURLPort(req.URL))
		if !davPlaintextAllowed(req.Context(), host, port) {
			return nil, fmt.Errorf("Gofer won't connect to %s server %s without encryption; an administrator can add an HTTP plaintext exception for %s:%d under Mail security", g.service, req.URL.Host, host, port)
		}
	default:
		return nil, fmt.Errorf("%s does not support %q URLs", g.service, req.URL.Scheme)
	}
	return davHTTPTransport.RoundTrip(req)
}

// jmapServerAddress is a JMAP account's server address, its scheme, host and
// port, from the session URL. DAV discovery tries it when no address was
// entered: many JMAP servers serve CardDAV and CalDAV on the same address.
func jmapServerAddress(sessionURL string) string {
	u, err := url.Parse(strings.TrimSpace(sessionURL))
	if err != nil || u.Host == "" || (!strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http")) {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// validPushEndpoint accepts the HTTPS address of a browser's push service.
// The browser supplies it, so it is delivered to only through netguard as
// well; this refuses anything that isn't a plain HTTPS URL to begin with.
func validPushEndpoint(endpoint string) bool {
	if len(endpoint) > 2048 {
		return false
	}
	u, err := url.Parse(endpoint)
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Host != "" && u.User == nil
}
