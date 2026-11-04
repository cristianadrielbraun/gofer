package views

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// pageLang is the HTML lang attribute for the request's language.
func pageLang(ctx context.Context) string {
	return i18n.Lang(ctx)
}
