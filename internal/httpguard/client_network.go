package httpguard

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

const maximumNetworkEntries = 64

func parseCIDRs(name, raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ",")
	if len(entries) > maximumNetworkEntries {
		return nil, fmt.Errorf("%s accepts at most %d CIDRs", name, maximumNetworkEntries)
	}
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("%s must contain comma-separated IP CIDRs; invalid entry %q", name, entry)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("%s contains an ambiguous mapped IPv4 prefix", name)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func containsAddress(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// ClientNetworkMiddleware applies an explicit allowlist in every auth mode.
// Without one, open mode defaults to loopback; authenticated modes accept any
// network. Host/origin validation and authentication remain independent.
func (c *Config) ClientNetworkMiddleware(authEnabled bool, next http.Handler) http.Handler {
	if authEnabled && len(c.allowedCIDRs) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address, ok := c.clientAddress(r)
		allowed := ok && containsAddress(c.allowedCIDRs, address)
		if len(c.allowedCIDRs) == 0 {
			allowed = ok && (address.IsLoopback() || c.AllowUnauthenticatedRemote)
		}
		if !allowed {
			setSecurityHeaders(w)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "client network is not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c *Config) clientAddress(r *http.Request) (netip.Addr, bool) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	address := peer.Addr().Unmap().WithZone("")
	if !containsAddress(c.trustedProxyCIDRs, address) {
		// Neither X-Forwarded-For, Forwarded nor X-Real-IP can grant access when
		// sent by a peer that has not explicitly been configured as a proxy.
		return address, true
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if forwarded == "" || len(forwarded) > 8192 {
		return netip.Addr{}, false
	}
	entries := strings.Split(forwarded, ",")
	if len(entries) > maximumNetworkEntries {
		return netip.Addr{}, false
	}
	hops := make([]netip.Addr, len(entries))
	for i, entry := range entries {
		hop, err := netip.ParseAddr(strings.TrimSpace(entry))
		if err != nil || hop.Zone() != "" || hop.IsUnspecified() || hop.IsMulticast() {
			return netip.Addr{}, false
		}
		hops[i] = hop.Unmap()
	}
	// Walk from the socket peer toward the client. Stop at the first untrusted
	// hop, ignoring any claims it may have prepended to the forwarded chain.
	for i := len(hops) - 1; i >= 0 && containsAddress(c.trustedProxyCIDRs, address); i-- {
		address = hops[i]
	}
	return address, true
}
