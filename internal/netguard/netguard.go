// Package netguard keeps Gofer's outgoing connections to servers named by
// users or by message senders off private networks. A user could otherwise
// point a mail account at an internal service, and a sender could point a
// remote image at one, and have the Gofer server connect on their behalf. An
// administrator can approve exact private targets, such as a mail server on
// the local network.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// BlockedError reports a target refused because it is on a private network.
type BlockedError struct {
	Protocol string
	Host     string
	Port     int
}

func (e *BlockedError) Error() string {
	if e.Protocol == "proxy" {
		return fmt.Sprintf("proxy %s is on a private network; a proxy added for your own accounts needs a public address, or an administrator's Proxy private target exception under Mail security",
			net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
	}
	return fmt.Sprintf("%s server %s is on a private network; it needs a private target exception under Mail security",
		strings.ToUpper(e.Protocol), net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
}

// IsBlocked reports whether err comes from a refused private target.
func IsBlocked(err error) bool {
	var blocked *BlockedError
	return errors.As(err, &blocked)
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/32"),       // Teredo, which hides its IPv4 address
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
}

var (
	nat64Prefixes = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}
	sixToFour     = netip.MustParsePrefix("2002::/16")
)

// IsPrivate reports whether addr is anything but a public internet address,
// including IPv4 addresses carried inside IPv6 forms.
func IsPrivate(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	if addr.Is6() {
		b := addr.As16()
		for _, prefix := range nat64Prefixes {
			if prefix.Contains(addr) {
				return IsPrivate(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
			}
		}
		if sixToFour.Contains(addr) {
			return IsPrivate(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}

// Resolver looks up the addresses of a host.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer connects to servers only at checked addresses.
type Dialer struct {
	// Dialer sets timeouts and keep-alives. It is never given a host name.
	Dialer   *net.Dialer
	Resolver Resolver
}

// Dial connects to host:port for protocol. Private addresses are refused
// unless allowPrivate is set. It connects to the address it checked, so a DNS
// answer can't change between the check and the connection. When ctx carries
// a proxy (WithProxy), the connection goes through it, still to that address.
func (d *Dialer) Dial(ctx context.Context, protocol, host string, port int, allowPrivate bool) (net.Conn, error) {
	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if !allowPrivate {
		// One private answer refuses the host, so a name that resolves to
		// both a public and a private address can't be used to reach either.
		for _, addr := range addrs {
			if IsPrivate(addr) {
				return nil, &BlockedError{Protocol: protocol, Host: host, Port: port}
			}
		}
	}
	dialer := d.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	proxy := ProxyFrom(ctx)
	if proxy != nil && (port < 1 || port > 65535) {
		return nil, fmt.Errorf("server port %d is out of range", port)
	}
	var lastErr error
	for _, addr := range addrs {
		var (
			conn net.Conn
			err  error
		)
		if proxy != nil {
			conn, err = dialThroughProxy(ctx, dialer, proxy, netip.AddrPortFrom(addr.Unmap(), uint16(port)))
		} else {
			conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.Unmap().String(), strconv.Itoa(port)))
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *Dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	if host == "" {
		return nil, errors.New("server host is required")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return nil, fmt.Errorf("server address %q must not name a network interface", host)
		}
		return []netip.Addr{addr}, nil
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("server %q has no addresses", host)
	}
	return addrs, nil
}

// IsPrivateHost reports whether host, or any address it resolves to, is
// private, so Dial would refuse it without an exception. It only looks up the
// name and never connects.
func (d *Dialer) IsPrivateHost(ctx context.Context, host string) (bool, error) {
	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if IsPrivate(addr) {
			return true, nil
		}
	}
	return false, nil
}

// IsPrivateHost checks host with the default resolver.
func IsPrivateHost(ctx context.Context, host string) (bool, error) {
	return (&Dialer{}).IsPrivateHost(ctx, host)
}

// Dial connects with a default Dialer.
func Dial(ctx context.Context, dialer *net.Dialer, protocol, host string, port int, allowPrivate bool) (net.Conn, error) {
	return (&Dialer{Dialer: dialer}).Dial(ctx, protocol, host, port, allowPrivate)
}
