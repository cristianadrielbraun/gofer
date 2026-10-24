package views

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestCalendarAccountProviderLabel(t *testing.T) {
	for provider, want := range map[string]string{"gmail": "Google", "outlook": "Microsoft", "imap": "IMAP", "jmap": "JMAP", "": "Mailbox"} {
		if got := calendarAccountProviderLabel(models.Account{Provider: provider}); got != want {
			t.Errorf("%q: got %q, want %q", provider, got, want)
		}
	}
}
