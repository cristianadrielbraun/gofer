package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestContactsDetailShowsSyncQueuedNotice(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{ID: "contact-1", Name: "Jane", Email: "jane@example.com"}
	if err := ContactsDetail(&contact, nil, false, true, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactsDetail.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, "Sync queued") {
		t.Fatalf("rendered detail missing sync queued notice: %s", html)
	}
}

func TestContactAvatarPreviewURLRequestsLargerGooglePhoto(t *testing.T) {
	got := contactAvatarPreviewURL("https://lh3.googleusercontent.com/-abc/s100/photo.jpg?sz=50")
	if !strings.Contains(got, "/s1024/photo.jpg") || !strings.Contains(got, "sz=1024") {
		t.Fatalf("preview URL = %q, want Google size override", got)
	}

	got = contactAvatarPreviewURL("https://lh3.googleusercontent.com/a-/ALV-UjV=s96-c")
	if !strings.Contains(got, "=s1024-c") || !strings.Contains(got, "sz=1024") {
		t.Fatalf("preview URL = %q, want Google path size override", got)
	}

	dataURL := "data:image/png;base64,abc"
	if got := contactAvatarPreviewURL(dataURL); got != dataURL {
		t.Fatalf("data URL = %q, want unchanged", got)
	}

	otherURL := "https://photos.example/jane.jpg?sz=50"
	if got := contactAvatarPreviewURL(otherURL); got != otherURL {
		t.Fatalf("non-Google URL = %q, want unchanged", got)
	}
}

func TestContactAvatarRenderURLProxiesGoogleProviderPhotos(t *testing.T) {
	raw := "https://lh3.googleusercontent.com/a-/ALV-UjV=s100"
	got := contactAvatarRenderURL(raw)
	if !strings.HasPrefix(got, "/api/provider-avatar?url=") || !strings.Contains(got, "lh3.googleusercontent.com") {
		t.Fatalf("render URL = %q, want provider avatar proxy", got)
	}

	for _, raw := range []string{
		"/api/avatars/hash",
		"data:image/png;base64,abc",
		"https://photos.example/jane.jpg",
	} {
		if got := contactAvatarRenderURL(raw); got != raw {
			t.Fatalf("render URL = %q, want %q unchanged", got, raw)
		}
	}
}
