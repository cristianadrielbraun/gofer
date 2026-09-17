package httpguard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientNetworkAllowlistAndProxyBoundary(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "authenticated"}[authenticated], func(t *testing.T) {
			cfg, err := newConfig("0.0.0.0:8090", "http://localhost:8090", false)
			if err != nil {
				t.Fatal(err)
			}
			cfg.allowedCIDRs, err = parseCIDRs("test", "192.168.0.0/24,2001:db8:1::/48")
			if err != nil {
				t.Fatal(err)
			}
			cfg.trustedProxyCIDRs, err = parseCIDRs("test", "10.0.0.0/24,127.0.0.1/32")
			if err != nil {
				t.Fatal(err)
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
			stack := cfg.ClientNetworkMiddleware(authenticated, cfg.Middleware(next))
			for _, tt := range []struct {
				name, peer, forwarded string
				allow                 bool
			}{
				{"allowed IPv4", "192.168.0.25:4567", "", true},
				{"outside subnet", "192.168.1.25:4567", "", false},
				{"mapped IPv4", "[::ffff:192.168.0.25]:4567", "", true},
				{"allowed IPv6", "[2001:db8:1::25]:4567", "", true},
				{"outside IPv6", "[2001:db8:2::25]:4567", "", false},
				{"malformed peer", "not-an-ip", "", false},
				{"untrusted forwarding spoof", "203.0.113.25:4567", "192.168.0.25", false},
				{"trusted proxy allowed", "10.0.0.1:4567", "192.168.0.25", true},
				{"trusted proxy denied", "10.0.0.1:4567", "203.0.113.25", false},
				{"missing proxy header", "10.0.0.1:4567", "", false},
				{"malformed proxy header", "10.0.0.1:4567", "unknown", false},
				{"ignore spoofed left entry", "10.0.0.1:4567", "192.168.0.25, 203.0.113.25", false},
				{"trusted proxy chain", "127.0.0.1:4567", "192.168.0.25, 10.0.0.2", true},
				{"trusted chain with denied client", "127.0.0.1:4567", "203.0.113.25, 10.0.0.2", false},
				{"loopback not implicitly allowed", "[::1]:4567", "", false},
				{"oversized header", "10.0.0.1:4567", strings.Repeat("1", 8193), false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					r := httptest.NewRequest(http.MethodGet, "http://localhost:8090/", nil)
					r.RemoteAddr = tt.peer
					if tt.forwarded != "" {
						r.Header.Set("X-Forwarded-For", tt.forwarded)
					}
					r.Header.Set("X-Real-IP", "192.168.0.25")
					r.Header.Set("Forwarded", "for=192.168.0.25")
					w := httptest.NewRecorder()
					stack.ServeHTTP(w, r)
					want := http.StatusForbidden
					if tt.allow {
						want = http.StatusNoContent
					}
					if w.Code != want {
						t.Fatalf("status %d want %d", w.Code, want)
					}
				})
			}
			// Network admission does not bypass existing Host/origin restrictions.
			for _, tt := range []struct {
				host, origin string
				want         int
			}{{"evil.example", "", http.StatusMisdirectedRequest}, {"localhost:8090", "https://evil.example", http.StatusForbidden}} {
				r := httptest.NewRequest(http.MethodPost, "http://localhost:8090/", nil)
				r.RemoteAddr = "192.168.0.25:4567"
				r.Host = tt.host
				r.Header.Set("Origin", tt.origin)
				w := httptest.NewRecorder()
				stack.ServeHTTP(w, r)
				if w.Code != tt.want {
					t.Fatalf("Host/origin boundary %d want %d", w.Code, tt.want)
				}
			}
		})
	}
}

func TestNetworkDefaultsAndExplicitAllNetworks(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		cfg, err := newConfig("", "", false)
		if err != nil {
			t.Fatal(err)
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
		for _, peer := range []string{"127.0.0.1:1", "[::1]:1", "203.0.113.1:1"} {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = peer
			w := httptest.NewRecorder()
			cfg.ClientNetworkMiddleware(authenticated, next).ServeHTTP(w, r)
			want := http.StatusNoContent
			if !authenticated && peer == "203.0.113.1:1" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Fatalf("default auth=%t peer=%s: %d", authenticated, peer, w.Code)
			}
		}
		cfg.allowedCIDRs, _ = parseCIDRs("test", "0.0.0.0/0,::/0")
		for _, peer := range []string{"203.0.113.1:1", "[2001:db8::1]:1"} {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = peer
			w := httptest.NewRecorder()
			cfg.ClientNetworkMiddleware(authenticated, next).ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Fatalf("explicit unrestricted: %d", w.Code)
			}
		}
	}
}

func TestNetworkConfigurationRejectsMalformedAndOverridesLegacyRemote(t *testing.T) {
	t.Setenv("GOFER_ADDR", "0.0.0.0:8090")
	t.Setenv("GOFER_BASE_URL", "http://192.168.0.2:8090")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "true")
	t.Setenv("GOFER_TRUSTED_PROXY_CIDRS", "")
	for _, value := range []string{"192.168.0.0/24,", "hostname/24", "192.168.0.1", "::/129", strings.Repeat("127.0.0.1/32,", 65)} {
		t.Setenv("GOFER_ALLOWED_CIDRS", value)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("accepted invalid CIDRs %q", value)
		}
	}
	t.Setenv("GOFER_ALLOWED_CIDRS", "192.168.0.22/24,::ffff:10.0.0.0/120")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowUnauthenticatedRemote || cfg.allowedCIDRs[0].String() != "192.168.0.0/24" || cfg.allowedCIDRs[1].String() != "10.0.0.0/24" {
		t.Fatalf("incorrect CIDR precedence/normalization %#v", cfg)
	}
	if err := cfg.ValidateExposure(false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("invalid trusted proxy configuration accepted")
	}
}
