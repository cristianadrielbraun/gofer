package handler

import (
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
)

func TestServerTextTranslatesIMAPLoginLimit(t *testing.T) {
	es, ok := i18n.Parse("es")
	if !ok {
		t.Fatal("Spanish locale is unavailable")
	}
	ctx := i18n.WithLocale(context.Background(), es)
	const want = "el servidor está limitando los inicios de sesión de esta cuenta; Gofer espera unos minutos antes de volver a intentarlo"
	tests := map[string]string{
		imapclient.LoginCooldownMessage: want,
		"authenticate: " + imapclient.LoginCooldownMessage + " (imap: NO [LIMIT] AUTHENTICATE Rate limit hit.)": "autenticación: " + want + " (imap: NO [LIMIT] AUTHENTICATE Rate limit hit.)",
	}
	for text, translated := range tests {
		if got := serverText(ctx, text); got != translated {
			t.Errorf("serverText(%q) = %q, want %q", text, got, translated)
		}
	}
}
