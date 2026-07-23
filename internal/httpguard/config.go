package httpguard

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultListenAddr = "127.0.0.1:8090"
	DefaultBaseURL    = "http://local.localhost:8090"

	automationHeader = "X-Gofer-Request"
)

type requestOrigin struct {
	scheme string
	host   string
	port   string
}

type Config struct {
	ListenAddr                 string
	BaseURL                    string
	AllowUnauthenticatedRemote bool

	baseOrigin     requestOrigin
	listenLoopback bool
	baseLoopback   bool
	trustedOrigins map[requestOrigin]struct{}
}

func LoadConfig() (*Config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("GOFER_ADDR"))
	baseURL := strings.TrimSpace(os.Getenv("GOFER_BASE_URL"))

	allowRemote, err := parseOptionalBool("GOFER_ALLOW_UNAUTHENTICATED_REMOTE")
	if err != nil {
		return nil, err
	}

	return newConfig(listenAddr, baseURL, allowRemote)
}

func newConfig(listenAddr, baseURL string, allowUnauthenticatedRemote bool) (*Config, error) {
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	listenHost, err := validateListenAddr(listenAddr)
	if err != nil {
		return nil, err
	}

	canonicalBaseURL, baseOrigin, err := validateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		ListenAddr:                 listenAddr,
		BaseURL:                    canonicalBaseURL,
		AllowUnauthenticatedRemote: allowUnauthenticatedRemote,
		baseOrigin:                 baseOrigin,
		listenLoopback:             isLoopbackHost(listenHost),
		baseLoopback:               isLoopbackHost(baseOrigin.host),
		trustedOrigins:             map[requestOrigin]struct{}{baseOrigin: {}},
	}
	if cfg.baseLoopback {
		for _, host := range []string{"localhost", "local.localhost", "127.0.0.1", "::1"} {
			cfg.trustedOrigins[requestOrigin{scheme: baseOrigin.scheme, host: host, port: baseOrigin.port}] = struct{}{}
		}
	}

	return cfg, nil
}

func (c *Config) ValidateExposure(authEnabled bool) error {
	if !authEnabled && c.hasRemoteExposure() && !c.AllowUnauthenticatedRemote {
		return fmt.Errorf(
			"refusing unauthenticated remote exposure: GOFER_ADDR=%q and GOFER_BASE_URL=%q are only allowed when authentication is enabled or GOFER_ALLOW_UNAUTHENTICATED_REMOTE=true is explicitly set",
			c.ListenAddr,
			c.BaseURL,
		)
	}
	if authEnabled && !c.baseLoopback && c.baseOrigin.scheme != "https" {
		return fmt.Errorf("GOFER_BASE_URL must use https for authenticated non-loopback access")
	}
	return nil
}

func (c *Config) WarnUnauthenticatedRemote(authEnabled bool) bool {
	return !authEnabled && c.hasRemoteExposure() && c.AllowUnauthenticatedRemote
}

func (c *Config) SecureCookies() bool {
	return c.baseOrigin.scheme == "https"
}

func (c *Config) hasRemoteExposure() bool {
	return !c.listenLoopback || !c.baseLoopback
}

func (c *Config) trustsHost(rawHost string) bool {
	origin, err := originFromHost(rawHost, c.baseOrigin.scheme)
	if err != nil {
		return false
	}
	_, ok := c.trustedOrigins[origin]
	return ok
}

func (c *Config) trustsOrigin(rawOrigin string) bool {
	origin, err := originFromURL(rawOrigin, false)
	if err != nil {
		return false
	}
	_, ok := c.trustedOrigins[origin]
	return ok
}

func (c *Config) trustsReferer(rawReferer string) bool {
	origin, err := originFromURL(rawReferer, true)
	if err != nil {
		return false
	}
	_, ok := c.trustedOrigins[origin]
	return ok
}

func parseOptionalBool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func validateListenAddr(raw string) (string, error) {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("invalid GOFER_ADDR %q: expected host:port", raw)
	}
	if err := validatePort(port); err != nil {
		return "", fmt.Errorf("invalid GOFER_ADDR %q: %w", raw, err)
	}
	return normalizeHost(host), nil
}

func validateBaseURL(raw string) (string, requestOrigin, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: %w", raw, err)
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: an absolute URL without credentials is required", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: paths are not allowed", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: query strings and fragments are not allowed", raw)
	}

	host := normalizeHost(parsed.Hostname())
	port := parsed.Port()
	if port != "" {
		if err := validatePort(port); err != nil {
			return "", requestOrigin{}, fmt.Errorf("invalid GOFER_BASE_URL %q: %w", raw, err)
		}
	}

	parsed.Host = formatHostPort(host, port)
	parsed.Path = ""
	parsed.RawPath = ""
	origin := requestOrigin{
		scheme: parsed.Scheme,
		host:   host,
		port:   effectivePort(parsed.Scheme, port),
	}
	return parsed.String(), origin, nil
}

func originFromHost(rawHost, scheme string) (requestOrigin, error) {
	rawHost = strings.TrimSpace(rawHost)
	if rawHost == "" || strings.ContainsAny(rawHost, `/\`) {
		return requestOrigin{}, fmt.Errorf("invalid host")
	}

	var host, port string
	if strings.HasPrefix(rawHost, "[") && strings.HasSuffix(rawHost, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(rawHost, "["), "]")
	} else if strings.Contains(rawHost, ":") {
		var err error
		host, port, err = net.SplitHostPort(rawHost)
		if err != nil {
			return requestOrigin{}, err
		}
	} else {
		host = rawHost
	}
	host = normalizeHost(host)
	if host == "" {
		return requestOrigin{}, fmt.Errorf("invalid host")
	}
	if port != "" {
		if err := validatePort(port); err != nil {
			return requestOrigin{}, err
		}
	}
	return requestOrigin{scheme: scheme, host: host, port: effectivePort(scheme, port)}, nil
}

func originFromURL(raw string, allowPath bool) (requestOrigin, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || strings.ContainsAny(raw, "\r\n\t ") {
		return requestOrigin{}, fmt.Errorf("invalid origin")
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return requestOrigin{}, fmt.Errorf("invalid origin")
	}
	if !allowPath && parsed.Path != "" {
		return requestOrigin{}, fmt.Errorf("origin must not contain a path")
	}
	if !allowPath && (parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery) {
		return requestOrigin{}, fmt.Errorf("origin must not contain a query or fragment")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return requestOrigin{}, fmt.Errorf("invalid origin scheme")
	}
	port := parsed.Port()
	if port != "" {
		if err := validatePort(port); err != nil {
			return requestOrigin{}, err
		}
	}
	return requestOrigin{
		scheme: scheme,
		host:   normalizeHost(parsed.Hostname()),
		port:   effectivePort(scheme, port),
	}, nil
}

func validatePort(raw string) error {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be a number between 1 and 65535")
	}
	return nil
}

func normalizeHost(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	return strings.TrimSuffix(raw, ".")
}

func formatHostPort(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func effectivePort(scheme, port string) string {
	if port != "" {
		return port
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func isLoopbackHost(host string) bool {
	host = normalizeHost(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
