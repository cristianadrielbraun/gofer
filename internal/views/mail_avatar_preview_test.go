package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailViewAvatarUsesProfilePicturePreviewWhenAvailable(t *testing.T) {
	contact := models.Contact{
		Name:       "Jane Sender",
		Email:      "jane@example.com",
		Initials:   "JS",
		AvatarURL:  "https://photos.example/jane.jpg",
		AvatarHash: "avatar-hash",
	}
	var out bytes.Buffer
	if err := MailViewAvatar(contact, "message-1", "size-11 rounded-full", "bg-muted").Render(context.Background(), &out); err != nil {
		t.Fatalf("MailViewAvatar.Render() error = %v", err)
	}
	html := out.String()
	for _, expected := range []string{
		`data-contact-avatar-preview-trigger`,
		`data-contact-avatar-preview-dialog="mail-avatar-preview-message-1"`,
		`id="mail-avatar-preview-message-1"`,
		`data-contact-avatar-preview-image`,
		`aria-label="View profile picture"`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered avatar missing %q: %s", expected, html)
		}
	}
}

func TestMailViewAvatarKeepsFallbackNonInteractive(t *testing.T) {
	contact := models.Contact{Name: "Jane Sender", Email: "jane@example.com", Initials: "JS"}
	var out bytes.Buffer
	if err := MailViewAvatar(contact, "message-1", "size-11 rounded-full", "bg-muted").Render(context.Background(), &out); err != nil {
		t.Fatalf("MailViewAvatar.Render() error = %v", err)
	}
	if strings.Contains(out.String(), "data-contact-avatar-preview-trigger") {
		t.Fatalf("fallback avatar unexpectedly rendered a preview trigger: %s", out.String())
	}
}
