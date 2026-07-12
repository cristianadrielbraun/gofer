package autodiscover

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeIPResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (fn fakeIPResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return fn(ctx, host)
}

func TestResolveTargetRejectsPrivateAddressesUnlessExactException(t *testing.T) {
	resolver := fakeIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.168.10.20")}}, nil
	})
	policy := securityPolicy{resolveIP: resolver.LookupIPAddr}
	if _, err := resolveTarget(context.Background(), "imap", "mail.example.test", 993, policy); err == nil {
		t.Fatal("private target was accepted without an exception")
	}
	policy.allowPrivateTarget = func(protocol, host string, port int) bool {
		return protocol == "imap" && host == "mail.example.test" && port == 993
	}
	if _, err := resolveTarget(context.Background(), "imap", "mail.example.test", 993, policy); err != nil {
		t.Fatalf("approved private target was rejected: %v", err)
	}
	if _, err := resolveTarget(context.Background(), "imap", "mail.example.test", 143, policy); err == nil {
		t.Fatal("private target exception was broader than the approved port")
	}
}

func TestDiscoveryRedirectRejectsPrivateTarget(t *testing.T) {
	resolver := fakeIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "private.example.test" {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})
	client := newDiscoveryHTTPClientWithPolicy(securityPolicy{resolveIP: resolver.LookupIPAddr})
	initial, err := http.NewRequest(http.MethodGet, "https://public.example.test/config.xml", nil)
	if err != nil {
		t.Fatalf("initial request: %v", err)
	}
	redirect, err := http.NewRequest(http.MethodGet, "https://private.example.test/config.xml", nil)
	if err != nil {
		t.Fatalf("redirect request: %v", err)
	}
	if err := client.CheckRedirect(redirect, []*http.Request{initial}); err == nil || !strings.Contains(err.Error(), "private target") {
		t.Fatalf("private redirect error = %v, want private-target rejection", err)
	}
}

func TestValidateDiscoveredCandidatesRejectsPrivateMailHost(t *testing.T) {
	resolver := fakeIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "imap.private.test" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.20")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.20")}}, nil
	})
	policy := securityPolicy{resolveIP: resolver.LookupIPAddr}
	candidates := []Candidate{{
		IMAPHost: "imap.private.test", IMAPPort: 993, IMAPTLSMode: "tls",
		SMTPHost: "smtp.public.test", SMTPPort: 587, SMTPTLSMode: "starttls",
		Username: "me@example.test", AuthMethod: "plain",
	}}
	if got := validateDiscoveredCandidates(context.Background(), candidates, policy); len(got) != 0 {
		t.Fatalf("private candidate survived validation: %#v", got)
	}
	policy.allowPrivateTarget = func(protocol, host string, port int) bool {
		return protocol == "imap" && host == "imap.private.test" && port == 993
	}
	if got := validateDiscoveredCandidates(context.Background(), candidates, policy); len(got) != 1 {
		t.Fatalf("approved private candidate was filtered: %#v", got)
	}
}

func TestDiscoverFiltersPrivateXMLMailTargets(t *testing.T) {
	parts := "me@example.test"
	configURL := "https://autoconfig.example.test/mail/config-v1.1.xml?emailaddress=me%40example.test"
	client := fakeHTTPClient{configURL: testConfigXML("imap.private.test", "smtp.public.test")}
	resolver := fakeResolver{}
	ipResolver := fakeIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "autoconfig.example.test", "smtp.public.test":
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.40")}}, nil
		case "imap.private.test":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.40")}}, nil
		default:
			return nil, fmt.Errorf("unexpected DNS lookup for %s", host)
		}
	})
	candidates, err := Discover(context.Background(), parts, Options{
		HTTPClient: client,
		Resolver:   resolver,
		MXResolver: fakeMXResolver{},
		IPResolver: ipResolver,
	})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("private XML target survived discovery: %#v", candidates)
	}
}

func TestDialResolvedTargetPinsValidatedDNSAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			close(accepted)
			_ = conn.Close()
		}
	}()

	var calls int
	resolver := fakeIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls == 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.2")}}, nil
	})
	policy := securityPolicy{resolveIP: resolver.LookupIPAddr, allowPrivateTarget: func(string, string, int) bool { return true }}
	addresses, err := resolveTarget(context.Background(), "imap", "mail.example.test", 143, policy)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	conn, err := dialResolvedTarget(context.Background(), "mail.example.test", port, time.Second, addresses)
	if err != nil {
		t.Fatalf("dialResolvedTarget: %v", err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("validated address was not dialed")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want one lookup before pinned dial", calls)
	}
}

func TestIMAPStartTLSProbeCompletesHandshakeAndRefreshesCapabilities(t *testing.T) {
	cert, roots := testProbeCertificate(t, "127.0.0.1")
	port, done := serveIMAPStartTLS(t, cert, false, false)
	policy := securityPolicy{
		allowPrivateTarget: func(protocol, host string, port int) bool { return protocol == "imap" && host == "127.0.0.1" },
		rootCAs:            roots,
	}
	probe := probeMailEndpointWithPolicy(context.Background(), "imap", "127.0.0.1", port, "starttls", time.Second, policy)
	if !probe.ok || !strings.Contains(strings.Join(probe.notes, " "), "handshake") {
		t.Fatalf("IMAP STARTTLS probe = %#v, want successful handshake", probe)
	}
	<-done
}

func TestSMTPStartTLSProbeCompletesHandshakeAndRefreshesEHLO(t *testing.T) {
	cert, roots := testProbeCertificate(t, "127.0.0.1")
	port, done := serveSMTPStartTLS(t, cert, false, false)
	policy := securityPolicy{
		allowPrivateTarget: func(protocol, host string, port int) bool { return protocol == "smtp" && host == "127.0.0.1" },
		rootCAs:            roots,
	}
	probe := probeMailEndpointWithPolicy(context.Background(), "smtp", "127.0.0.1", port, "starttls", time.Second, policy)
	if !probe.ok || !strings.Contains(strings.Join(probe.notes, " "), "handshake") {
		t.Fatalf("SMTP STARTTLS probe = %#v, want successful handshake", probe)
	}
	<-done
}

func TestStartTLSProbeRejectsRejectedCommandAndBadCertificate(t *testing.T) {
	tests := []struct {
		name    string
		reject  bool
		badCert bool
	}{
		{name: "command rejected", reject: true},
		{name: "certificate mismatch", badCert: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := "127.0.0.1"
			certHost := host
			if test.badCert {
				certHost = "wrong.example.test"
			}
			cert, roots := testProbeCertificate(t, certHost)
			port, done := serveIMAPStartTLS(t, cert, test.reject, test.badCert)
			policy := securityPolicy{
				allowPrivateTarget: func(protocol, targetHost string, port int) bool { return protocol == "imap" && targetHost == host },
				rootCAs:            roots,
			}
			probe := probeMailEndpointWithPolicy(context.Background(), "imap", host, port, "starttls", time.Second, policy)
			if probe.ok {
				t.Fatalf("probe = %#v, want failure", probe)
			}
			<-done
		})
	}
}

func testProbeCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(cryptorand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		leafTemplate.IPAddresses = []net.IP{ip}
	} else {
		leafTemplate.DNSNames = []string{host}
	}
	leafDER, err := x509.CreateCertificate(cryptorand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: marshalECPrivateKey(t, leafKey)}))
	if err != nil {
		t.Fatalf("load test certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) {
		t.Fatal("append test CA")
	}
	return cert, roots
}

func marshalECPrivateKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal EC key: %v", err)
	}
	return der
}

func serveIMAPStartTLS(t *testing.T, cert tls.Certificate, reject, closeAfterHandshake bool) (int, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen IMAP: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "* OK IMAP ready\r\n")
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "* CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN\r\nA001 OK capability\r\n")
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		if reject {
			_, _ = io.WriteString(conn, "A002 NO STARTTLS unavailable\r\n")
			return
		}
		_, _ = io.WriteString(conn, "A002 OK begin TLS\r\n")
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil || closeAfterHandshake {
			return
		}
		reader = bufio.NewReader(tlsConn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(tlsConn, "* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\nA003 OK capability\r\n")
		_, _ = reader.ReadString('\n')
	}()
	return listener.Addr().(*net.TCPAddr).Port, done
}

func serveSMTPStartTLS(t *testing.T, cert tls.Certificate, reject, closeAfterHandshake bool) (int, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen SMTP: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "220 smtp ready\r\n")
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "250-smtp.example\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		if reject {
			_, _ = io.WriteString(conn, "454 TLS unavailable\r\n")
			return
		}
		_, _ = io.WriteString(conn, "220 ready for TLS\r\n")
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil || closeAfterHandshake {
			return
		}
		reader = bufio.NewReader(tlsConn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(tlsConn, "250-smtp.example\r\n250 AUTH PLAIN\r\n")
	}()
	return listener.Addr().(*net.TCPAddr).Port, done
}
