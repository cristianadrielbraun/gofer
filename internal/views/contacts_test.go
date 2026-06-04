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
