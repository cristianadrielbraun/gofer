package httpguard

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaultsToLoopback(t *testing.T) {
	t.Setenv("GOFER_ADDR", "")
	t.Setenv("GOFER_BASE_URL", "")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, DefaultBaseURL)
	}
	if err := cfg.ValidateExposure(false); err != nil {
		t.Fatalf("ValidateExposure(false) error = %v", err)
	}
}

func TestValidateExposure(t *testing.T) {
	tests := []struct {
		name        string
		listenAddr  string
		baseURL     string
		allowRemote bool
		authEnabled bool
		wantError   string
		wantWarning bool
	}{
		{
			name:       "local unauthenticated",
			listenAddr: "127.0.0.1:8090",
			baseURL:    "http://localhost:8090",
		},
		{
			name:       "wildcard unauthenticated",
			listenAddr: ":8090",
			baseURL:    "http://localhost:8090",
			wantError:  "refusing unauthenticated remote exposure",
		},
		{
			name:        "wildcard unauthenticated explicit override",
			listenAddr:  "0.0.0.0:8090",
			baseURL:     "http://192.0.2.10:8090",
			allowRemote: true,
			wantWarning: true,
		},
		{
			name:       "remote canonical URL unauthenticated",
			listenAddr: "127.0.0.1:8090",
			baseURL:    "https://mail.example.test",
			wantError:  "refusing unauthenticated remote exposure",
		},
		{
			name:        "authenticated remote URL requires HTTPS",
			listenAddr:  "0.0.0.0:8090",
			baseURL:     "http://mail.example.test",
			authEnabled: true,
			wantError:   "must use https",
		},
		{
			name:        "authenticated remote URL with HTTPS",
			listenAddr:  "0.0.0.0:8090",
			baseURL:     "https://mail.example.test",
			authEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := newConfig(tt.listenAddr, tt.baseURL, tt.allowRemote)
			if err != nil {
				t.Fatalf("newConfig() error = %v", err)
			}
			err = cfg.ValidateExposure(tt.authEnabled)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateExposure() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateExposure() error = %v, want containing %q", err, tt.wantError)
			}
			if got := cfg.WarnUnauthenticatedRemote(tt.authEnabled); got != tt.wantWarning {
				t.Fatalf("WarnUnauthenticatedRemote() = %t, want %t", got, tt.wantWarning)
			}
		})
	}
}

func TestConfigRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		baseURL    string
	}{
		{name: "listen address without port", listenAddr: "127.0.0.1", baseURL: DefaultBaseURL},
		{name: "listen address with invalid port", listenAddr: "127.0.0.1:invalid", baseURL: DefaultBaseURL},
		{name: "base URL without scheme", listenAddr: DefaultListenAddr, baseURL: "localhost:8090"},
		{name: "base URL with credentials", listenAddr: DefaultListenAddr, baseURL: "http://user:pass@localhost:8090"},
		{name: "base URL with path", listenAddr: DefaultListenAddr, baseURL: "http://localhost:8090/gofer"},
		{name: "base URL with query", listenAddr: DefaultListenAddr, baseURL: "http://localhost:8090?mode=test"},
		{name: "base URL with unsupported scheme", listenAddr: DefaultListenAddr, baseURL: "ftp://localhost:8090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newConfig(tt.listenAddr, tt.baseURL, false); err == nil {
				t.Fatal("newConfig() error = nil, want validation error")
			}
		})
	}
}

func TestLoadConfigRejectsInvalidRemoteOverride(t *testing.T) {
	t.Setenv("GOFER_ADDR", DefaultListenAddr)
	t.Setenv("GOFER_BASE_URL", DefaultBaseURL)
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "sometimes")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want invalid boolean error")
	}
}

func TestConfigNormalizesBaseURLAndDerivesCookieSecurity(t *testing.T) {
	cfg, err := newConfig("127.0.0.1:8090", "HTTPS://Gofer.Example:443/", false)
	if err != nil {
		t.Fatalf("newConfig() error = %v", err)
	}
	if cfg.BaseURL != "https://gofer.example:443" {
		t.Fatalf("BaseURL = %q, want canonical URL", cfg.BaseURL)
	}
	if !cfg.SecureCookies() {
		t.Fatal("SecureCookies() = false, want true")
	}
}
