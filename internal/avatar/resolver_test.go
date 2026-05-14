package avatar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

func TestParseIconLinks(t *testing.T) {
	got := parseIconLinks("https://brand.example/mail/", []byte(`
		<html><head>
			<link rel="stylesheet" href="/app.css">
			<link rel="icon" href="/favicon.png">
			<link rel="apple-touch-icon" href="https://cdn.brand.example/touch.png">
			<link rel="icon" href="http://brand.example/insecure.png">
		</head></html>`))
	want := []string{"https://brand.example/favicon.png", "https://cdn.brand.example/touch.png"}
	if len(got) != len(want) {
		t.Fatalf("parseIconLinks() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseIconLinks()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveDomainIconDiscoversLinkedIcon(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(`<link rel="icon" href="/icon.png">`)),
			}, nil
		case "/icon.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
			}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		}
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.Source != "domain_icon" || image.SourceURL != "https://brand.example/icon.png" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want linked domain_icon", image, found)
	}
}

func TestResolveDomainIconFallsBackToFavicon(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	requests := map[string]int{}
	var mu sync.Mutex
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests[req.URL.Path]++
		mu.Unlock()
		if req.URL.Path == "/favicon.ico" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/x-icon"}},
				Body:       io.NopCloser(strings.NewReader("ico")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(`<html><head></head></html>`)),
		}, nil
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://brand.example/favicon.ico" || image.ContentType != "image/x-icon" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want favicon fallback", image, found)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/"] != 0 {
		t.Fatalf("requests = %+v, want direct favicon before homepage discovery", requests)
	}
}

func TestResolveDomainIconFollowsSafeRedirect(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "brand.example" && req.URL.Path == "/":
			return &http.Response{
				StatusCode: http.StatusMovedPermanently,
				Header:     http.Header{"Location": []string{"https://www.brand.example/"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		case req.URL.Host == "www.brand.example" && req.URL.Path == "/":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(`<link rel="icon" href="/icon.svg">`)),
				Request:    req,
			}, nil
		case req.URL.Host == "www.brand.example" && req.URL.Path == "/icon.svg":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/svg+xml"}},
				Body:       io.NopCloser(strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)),
				Request:    req,
			}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		}
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://www.brand.example/icon.svg" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want redirected icon", image, found)
	}
}

func TestResolveDomainIconTriesAlternateFavicons(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	requests := map[string]int{}
	var mu sync.Mutex
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests[req.URL.Path]++
		mu.Unlock()
		if req.URL.Path == "/favicon.svg" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/svg+xml"}},
				Body:       io.NopCloser(strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)),
			}, nil
		}
		if req.URL.Path == "/" {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://brand.example/favicon.svg" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want svg favicon fallback", image, found)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/favicon.svg"] != 1 || requests["/"] != 0 {
		t.Fatalf("requests = %+v, want direct svg favicon without homepage discovery", requests)
	}
}

func TestResolveDomainIconUpgradesInsecureHomepageRedirect(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" {
			return nil, fmt.Errorf("unexpected %s request", req.URL.Scheme)
		}
		switch {
		case req.URL.Host == "brand.example":
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://www.brand.example" + req.URL.Path}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		case req.URL.Host == "www.brand.example" && req.URL.Path == "/":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(`<link rel="icon" href="https://cdn.brand.example/favicon-32x32.png">`)),
				Request:    req,
			}, nil
		case req.URL.Host == "www.brand.example":
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		case req.URL.Host == "cdn.brand.example" && req.URL.Path == "/favicon-32x32.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
				Request:    req,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://cdn.brand.example/favicon-32x32.png" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want CDN icon from upgraded homepage redirect", image, found)
	}
}

func TestResolveDomainIconTreatsOversizedHomepageAsMissing(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxHTMLSize+1))),
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	_, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v, want nil for oversized homepage miss", err)
	}
	if found {
		t.Fatal("ResolveDomainIcon() found = true, want false")
	}
}

func TestResolveDomainIconParsesBoundedHomepagePrefix(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "brand.example" && req.URL.Path == "/":
			body := `<link rel="icon" href="https://cdn.brand.example/favicon-32x32.png">` + strings.Repeat("x", maxHTMLSize+1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		case req.URL.Host == "cdn.brand.example" && req.URL.Path == "/favicon-32x32.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
				Request:    req,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://cdn.brand.example/favicon-32x32.png" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want icon from bounded homepage prefix", image, found)
	}
}

func TestResolveDomainIconRetriesRootDomainAfterSubdomainConnectionError(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	requests := map[string]int{}
	var mu sync.Mutex
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests[req.URL.Host+req.URL.Path]++
		mu.Unlock()
		if req.URL.Host == "msg.salesforce.com" {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: &net.DNSError{Err: "no such host", Name: req.URL.Host, IsNotFound: true}}
		}
		if req.URL.Host == "salesforce.com" && req.URL.Path == "/favicon.ico" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/x-icon"}},
				Body:       io.NopCloser(strings.NewReader("ico")),
			}, nil
		}
		if req.URL.Host == "salesforce.com" && req.URL.Path == "/" {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	image, found, err := r.ResolveDomainIcon(context.Background(), "person@msg.salesforce.com")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if !found || image.SourceURL != "https://salesforce.com/favicon.ico" {
		t.Fatalf("ResolveDomainIcon() = (%+v, %v), want root-domain favicon", image, found)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["msg.salesforce.com/"] == 0 || requests["salesforce.com/favicon.ico"] != 1 {
		t.Fatalf("requests = %+v, want failed subdomain then root favicon", requests)
	}
}

func TestResolveDomainIconSkipsPublicMailboxDomains(t *testing.T) {
	r := NewResolver()
	requests := 0
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	})}

	_, found, err := r.ResolveDomainIcon(context.Background(), "person@gmail.com")
	if err != nil {
		t.Fatalf("ResolveDomainIcon() error = %v", err)
	}
	if found || requests != 0 {
		t.Fatalf("ResolveDomainIcon() found=%v requests=%d, want false and no requests", found, requests)
	}
}

func TestResolveDomainIconRejectsPrivateAddress(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}

	_, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
	if err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("ResolveDomainIcon() error = %v, want private address error", err)
	}
	if found {
		t.Fatal("ResolveDomainIcon() found = true, want false")
	}
}

func TestResolveDomainIconDedupesConcurrentLookups(t *testing.T) {
	r := NewResolver()
	r.lookupIPAddr = publicLookupIPAddr
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var mu sync.Mutex
	requests := map[string]int{}
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests[req.URL.Path]++
		mu.Unlock()
		if req.URL.Path == "/favicon.ico" {
			startedOnce.Do(func() { close(started) })
			<-release
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png")),
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			image, found, err := r.ResolveDomainIcon(context.Background(), "person@brand.example")
			if err != nil {
				errCh <- err
				return
			}
			if !found || image.SourceURL != "https://brand.example/favicon.ico" {
				errCh <- errors.New("unexpected domain icon result")
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if requests["/favicon.ico"] != 1 {
		t.Fatalf("requests = %+v, want one favicon request", requests)
	}
}

func publicLookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
