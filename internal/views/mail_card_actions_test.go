package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A mail card, and the card layout editor's preview, have each action button
// the card layout can place; the Read button says which way it goes.
func TestMailCardHasEveryActionButton(t *testing.T) {
	actions := map[string]string{
		"readAction":    "read",
		"archiveAction": "archive",
		"deleteAction":  "delete",
		"labelsAction":  "labels",
		"spamAction":    "spam",
	}
	unread := renderForTest(t, func(out *bytes.Buffer) error {
		return MailListCardItem(nil, models.Email{ID: "m1", AccountID: "a1", FolderID: "inbox"}, 0, nil, "").Render(context.Background(), out)
	})
	preview := renderForTest(t, func(out *bytes.Buffer) error {
		return MailCardLayoutDialog().Render(context.Background(), out)
	})
	for field, action := range actions {
		if !strings.Contains(unread, `data-mail-card-field="`+field+`" data-mail-card-action="`+action+`"`) {
			t.Errorf("card has no %s button", field)
		}
		if !strings.Contains(preview, `data-mail-card-field="`+field+`"`) {
			t.Errorf("editor preview has no %s field", field)
		}
	}
	if !strings.Contains(unread, `aria-label="Mark as read"`) {
		t.Error("an unread card's Read button doesn't offer to mark it read")
	}
	read := renderForTest(t, func(out *bytes.Buffer) error {
		return MailListCardItem(nil, models.Email{ID: "m2", AccountID: "a1", FolderID: "inbox", IsRead: true}, 0, nil, "").Render(context.Background(), out)
	})
	if !strings.Contains(read, `aria-label="Mark as unread"`) {
		t.Error("a read card's Read button doesn't offer to mark it unread")
	}
}
