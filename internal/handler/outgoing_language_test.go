package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// Mail Gofer writes for the user, such as a forwarded invitation, is written
// in the user's language; "Fwd:" stays as every mail program reads it.
func TestCalendarForwardMessageIsWrittenInTheSendersLanguage(t *testing.T) {
	english := calendarForwardMessage(i18n.WithLocale(context.Background(), i18n.English), "Ana", "ana@example.test", "", "", "", nil)
	if english.Subject != "Fwd: Invitation: Meeting" || english.TextBody != "Ana forwarded you an invitation: Meeting" {
		t.Fatalf("English forward = %q / %q", english.Subject, english.TextBody)
	}
	spanish := calendarForwardMessage(i18n.WithLocale(context.Background(), i18n.Locale("es")), "Ana", "ana@example.test", "Planificación", "Nos vemos ahí.", "", nil)
	if spanish.Subject != "Fwd: Invitación: Planificación" {
		t.Fatalf("Spanish subject = %q", spanish.Subject)
	}
	if !strings.HasPrefix(spanish.TextBody, "Nos vemos ahí.\n\n") || !strings.HasSuffix(spanish.TextBody, "Ana te reenvió una invitación: Planificación") {
		t.Fatalf("Spanish body = %q", spanish.TextBody)
	}
}

func TestQuoteToggleLabelsFollowTheLanguage(t *testing.T) {
	script := string(emailQuoteScript(i18n.WithLocale(context.Background(), i18n.Locale("es"))))
	if !strings.Contains(script, "Mostrar texto citado") || !strings.Contains(script, "Ocultar texto citado") || strings.Contains(script, "Show quoted text") {
		t.Fatal("the quote toggle labels are not in Spanish")
	}
}
