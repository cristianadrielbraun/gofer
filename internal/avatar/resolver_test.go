package avatar

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGravatarHash(t *testing.T) {
	got := GravatarHash(" MyEmailAddress@example.com ")
	want := "0bc83cb571cd1c50ba6f3e8a78ef1346"
	if got != want {
		t.Fatalf("GravatarHash() = %q, want %q", got, want)
	}
}

func TestGravatarHashInvalidEmail(t *testing.T) {
	if got := GravatarHash("not-an-email"); got != "" {
		t.Fatalf("GravatarHash() = %q, want empty", got)
	}
}

func TestIsGravatarHash(t *testing.T) {
	if !IsGravatarHash("0bc83cb571cd1c50ba6f3e8a78ef1346") {
		t.Fatal("expected valid hash")
	}
	if IsGravatarHash("status") {
		t.Fatal("expected invalid hash")
	}
}

func TestEmailDomain(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  string
	}{
		{name: "normalizes", email: " User@Example.COM. ", want: "example.com"},
		{name: "last at wins", email: "display@local@brand.example", want: "brand.example"},
		{name: "missing at", email: "not-an-email", want: ""},
		{name: "single label domain", email: "user@localhost", want: ""},
		{name: "empty domain", email: "user@", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EmailDomain(tt.email); got != tt.want {
				t.Fatalf("EmailDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsPublicMailboxDomain(t *testing.T) {
	tests := []struct {
		domain string
		want   bool
	}{
		{domain: "gmail.com", want: true},
		{domain: " Outlook.COM. ", want: true},
		{domain: "example.com", want: false},
		{domain: "", want: false},
	}

	for _, tt := range tests {
		if got := IsPublicMailboxDomain(tt.domain); got != tt.want {
			t.Fatalf("IsPublicMailboxDomain(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

func TestParseBIMILogoURL(t *testing.T) {
	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{
			name:    "valid BIMI record",
			records: []string{"v=BIMI1; l=https://brand.example/logo.svg; a=https://brand.example/vmc.pem"},
			want:    "https://brand.example/logo.svg",
		},
		{
			name:    "case insensitive version",
			records: []string{"v=bimi1; l=https://brand.example/logo.svg"},
			want:    "https://brand.example/logo.svg",
		},
		{
			name:    "ignores missing logo",
			records: []string{"v=BIMI1; a=https://brand.example/vmc.pem"},
			want:    "",
		},
		{
			name:    "ignores invalid version",
			records: []string{"v=BIMI2; l=https://brand.example/logo.svg"},
			want:    "",
		},
		{
			name:    "uses first valid record",
			records: []string{"v=spf1 -all", "v=BIMI1; l=https://brand.example/logo.svg"},
			want:    "https://brand.example/logo.svg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseBIMILogoURL(tt.records); got != tt.want {
				t.Fatalf("ParseBIMILogoURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLooksLikeSVG(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "plain svg", data: `<svg xmlns="http://www.w3.org/2000/svg"></svg>`, want: true},
		{name: "xml declaration", data: `<?xml version="1.0" encoding="UTF-8"?><svg></svg>`, want: true},
		{name: "comment before svg", data: `<!-- generated --><svg></svg>`, want: true},
		{name: "doctype before svg", data: `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN"><svg></svg>`, want: true},
		{name: "bom xml doctype svg", data: "\ufeff<?xml version=\"1.0\"?><!DOCTYPE svg><svg></svg>", want: true},
		{name: "html", data: `<html><body>not found</body></html>`, want: false},
		{name: "broken declaration", data: `<?xml version="1.0"<svg></svg>`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeSVG([]byte(tt.data)); got != tt.want {
				t.Fatalf("looksLikeSVG() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSafeSVGRejectsActiveContent(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "simple logo", data: `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1v1z"/></svg>`, want: true},
		{name: "script", data: `<svg><script>alert(1)</script></svg>`, want: false},
		{name: "event handler", data: `<svg onload="alert(1)"></svg>`, want: false},
		{name: "foreign object", data: `<svg><foreignObject><body></body></foreignObject></svg>`, want: false},
		{name: "external style url", data: `<svg><style>rect{fill:url(https://example.com/a)}</style></svg>`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSafeSVG([]byte(tt.data)); got != tt.want {
				t.Fatalf("isSafeSVG() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFetchBIMIClassifiesDNSNotFoundAsMissing(t *testing.T) {
	r := NewResolver()
	r.lookupTXT = func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{IsNotFound: true}
	}

	_, found, err := r.fetchBIMI(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("fetchBIMI() error = %v, want nil", err)
	}
	if found {
		t.Fatal("fetchBIMI() found = true, want false")
	}
}

func TestFetchBIMIClassifiesDNSTimeoutAsRetryableError(t *testing.T) {
	r := NewResolver()
	r.lookupTXT = func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{IsTimeout: true}
	}

	_, found, err := r.fetchBIMI(context.Background(), "example.com")
	if err == nil {
		t.Fatal("fetchBIMI() error = nil, want retryable DNS error")
	}
	if found {
		t.Fatal("fetchBIMI() found = true, want false")
	}
}

func TestFetchGravatarMissingAndInvalidResponses(t *testing.T) {
	tests := []struct {
		name      string
		response  *http.Response
		wantFound bool
		wantErr   bool
	}{
		{
			name: "not found",
			response: &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
		{
			name: "non image",
			response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader("<html></html>")),
			},
			wantErr: true,
		},
		{
			name: "oversized",
			response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxImageSize+1))),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver()
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "www.gravatar.com" {
					return nil, errors.New("unexpected host")
				}
				return tt.response, nil
			})}

			_, found, err := r.fetchGravatar(context.Background(), "0bc83cb571cd1c50ba6f3e8a78ef1346")
			if (err != nil) != tt.wantErr {
				t.Fatalf("fetchGravatar() error = %v, wantErr %v", err, tt.wantErr)
			}
			if found != tt.wantFound {
				t.Fatalf("fetchGravatar() found = %v, want %v", found, tt.wantFound)
			}
		})
	}
}

func TestResolveLibravatarUsesSeparateCacheFromGravatar(t *testing.T) {
	r := NewResolver()
	requests := map[string]int{}
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests[req.URL.Host]++
		switch req.URL.Host {
		case "www.gravatar.com":
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		case "seccdn.libravatar.org":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
			}, nil
		default:
			return nil, errors.New("unexpected host")
		}
	})}
	hash := "0bc83cb571cd1c50ba6f3e8a78ef1346"

	_, found, err := r.ResolveGravatar(context.Background(), hash)
	if err != nil {
		t.Fatalf("ResolveGravatar() error = %v", err)
	}
	if found {
		t.Fatal("ResolveGravatar() found = true, want false")
	}

	image, found, err := r.ResolveLibravatar(context.Background(), hash)
	if err != nil {
		t.Fatalf("ResolveLibravatar() error = %v", err)
	}
	if !found || image.Source != "libravatar" {
		t.Fatalf("ResolveLibravatar() = (%+v, %v), want found libravatar", image, found)
	}
	if !strings.Contains(image.SourceURL, "seccdn.libravatar.org") {
		t.Fatalf("SourceURL = %q, want Libravatar CDN", image.SourceURL)
	}

	_, found, err = r.ResolveLibravatar(context.Background(), hash)
	if err != nil || !found {
		t.Fatalf("cached ResolveLibravatar() = found %v error %v, want found nil", found, err)
	}
	if requests["www.gravatar.com"] != 1 || requests["seccdn.libravatar.org"] != 1 {
		t.Fatalf("requests = %+v, want one request per provider", requests)
	}
}
