package branding

import (
	"strings"
	"testing"
)

func TestDefaultsWithoutSettings(t *testing.T) {
	Set(Settings{})
	if Name() != DefaultName || Customized() {
		t.Fatalf("Name() = %q, Customized() = %v", Name(), Customized())
	}
	if LogoURL() != "/assets/logo.svg" || FaviconURL() != "/assets/logo.svg" || IconURL() != "/assets/logo.png" {
		t.Fatalf("default URLs = %q %q %q", LogoURL(), FaviconURL(), IconURL())
	}
}

func TestCustomBranding(t *testing.T) {
	t.Cleanup(func() { Set(Settings{}) })
	png := []byte("\x89PNG\r\n\x1a\n0000")
	before := Version()
	Set(Settings{Name: "  Acme Mail ", Logo: Image{Data: png, ContentType: "image/png"}})
	if Name() != "Acme Mail" || !Customized() {
		t.Fatalf("Name() = %q", Name())
	}
	if !strings.HasPrefix(LogoURL(), LogoPath+"?v=") {
		t.Fatalf("LogoURL() = %q", LogoURL())
	}
	// Without a favicon of its own, the favicon is the logo.
	if FaviconURL() != LogoURL() || FaviconType() != "image/png" || IconURL() != LogoURL() {
		t.Fatalf("favicon %q (%s), icon %q", FaviconURL(), FaviconType(), IconURL())
	}
	if Version() == before {
		t.Fatal("version did not change")
	}
}

func TestValidateName(t *testing.T) {
	if name, err := ValidateName("  Acme   Mail "); err != nil || name != "Acme Mail" {
		t.Fatalf("ValidateName = %q, %v", name, err)
	}
	if _, err := ValidateName(strings.Repeat("á", MaxNameLength+1)); err != ErrNameTooLong {
		t.Fatalf("long name error = %v", err)
	}
}

func TestValidateImage(t *testing.T) {
	cases := []struct {
		data []byte
		want string
		err  error
	}{
		{[]byte("\x89PNG\r\n\x1a\n0000"), "image/png", nil},
		{[]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml", nil},
		{[]byte{0, 0, 1, 0, 1, 0}, "image/x-icon", nil},
		{[]byte("<html><body>hi</body></html>"), "", ErrImageType},
		{[]byte("GIF89a"), "", ErrImageType},
		{make([]byte, MaxImageBytes+1), "", ErrImageTooLarge},
	}
	for _, c := range cases {
		got, err := ValidateImage(c.data)
		if got != c.want || err != c.err {
			t.Errorf("ValidateImage(%q...) = %q, %v; want %q, %v", c.data[:min(len(c.data), 12)], got, err, c.want, c.err)
		}
	}
}
