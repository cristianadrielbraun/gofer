package avatar

import "testing"

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
