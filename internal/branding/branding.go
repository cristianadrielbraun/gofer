// Package branding holds the name, logo and favicon
package branding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	DefaultName = "Gofer"
	MaxNameLength = 40
	MaxImageBytes = 512 << 10

	defaultLogoURL    = "/assets/logo.svg"
	defaultFaviconURL = "/assets/logo.svg"
	defaultIconURL    = "/assets/logo.png"

	LogoPath    = "/branding/logo"
	FaviconPath = "/branding/favicon"
)

var (
	ErrNameTooLong = errors.New("branding: name too long")
	ErrImageTooLarge = errors.New("branding: image too large")
	ErrImageType = errors.New("branding: unsupported image type")
)

type Image struct {
	Data        []byte
	ContentType string
}

func (i Image) Empty() bool { return len(i.Data) == 0 }

type Settings struct {
	Name    string
	Logo    Image
	Favicon Image
}

type state struct {
	settings    Settings
	logoHash    string
	faviconHash string
	version     string
}

var current atomic.Pointer[state]

func init() {
	Set(Settings{})
}

func Set(settings Settings) {
	settings.Name = strings.TrimSpace(settings.Name)
	next := &state{settings: settings}
	if !settings.Logo.Empty() {
		next.logoHash = hash(settings.Logo.Data)
	}
	if !settings.Favicon.Empty() {
		next.faviconHash = hash(settings.Favicon.Data)
	}
	next.version = hash([]byte(settings.Name + "\x00" + next.logoHash + "\x00" + next.faviconHash))
	current.Store(next)
}

func Current() Settings { return current.Load().settings }

func Customized() bool { return current.Load().settings.Name != "" }

func Name() string {
	if name := current.Load().settings.Name; name != "" {
		return name
	}
	return DefaultName
}

func Version() string { return current.Load().version }

func HasLogo() bool    { return !current.Load().settings.Logo.Empty() }
func HasFavicon() bool { return !current.Load().settings.Favicon.Empty() }

func LogoURL() string {
	s := current.Load()
	if s.logoHash == "" {
		return defaultLogoURL
	}
	return LogoPath + "?v=" + s.logoHash
}

func FaviconURL() string {
	s := current.Load()
	if s.faviconHash != "" {
		return FaviconPath + "?v=" + s.faviconHash
	}
	if s.logoHash != "" {
		return LogoURL()
	}
	return defaultFaviconURL
}

func IconURL() string {
	s := current.Load()
	switch {
	case s.faviconHash != "" && s.settings.Favicon.ContentType != "image/svg+xml":
		return FaviconURL()
	case s.logoHash != "" && s.settings.Logo.ContentType != "image/svg+xml":
		return LogoURL()
	}
	return defaultIconURL
}

func FaviconType() string {
	s := current.Load()
	if s.faviconHash != "" {
		return s.settings.Favicon.ContentType
	}
	if s.logoHash != "" {
		return s.settings.Logo.ContentType
	}
	return "image/svg+xml"
}

func ValidateName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > MaxNameLength {
		return "", ErrNameTooLong
	}
	return name, nil
}

func ValidateImage(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrImageType
	}
	if len(data) > MaxImageBytes {
		return "", ErrImageTooLarge
	}
	if isSVG(data) {
		return "image/svg+xml", nil
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0, 0, 1, 0}) {
		return "image/x-icon", nil
	}
	switch kind := http.DetectContentType(data); kind {
	case "image/png", "image/webp", "image/jpeg", "image/x-icon", "image/vnd.microsoft.icon":
		if kind == "image/vnd.microsoft.icon" {
			kind = "image/x-icon"
		}
		return kind, nil
	}
	return "", ErrImageType
}

func isSVG(data []byte) bool {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	text := strings.ToLower(string(bytes.TrimSpace(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")))))
	if !strings.HasPrefix(text, "<") {
		return false
	}
	return strings.Contains(text, "<svg")
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
