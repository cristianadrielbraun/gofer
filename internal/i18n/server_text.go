package i18n

import (
	"context"
	"sync"
)

// Messages from deep in Gofer's handlers and packages often travel as English
// text, in errors and stored reasons. The handlers keep a table that writes
// such a text in the page's language; ServerText lets pages built outside the
// handlers, such as the views, use it too.

var (
	serverTextMu sync.RWMutex
	serverTextFn func(ctx context.Context, text string) string
)

// SetServerTextTranslator installs the handlers' translation of English
// server texts.
func SetServerTextTranslator(translate func(ctx context.Context, text string) string) {
	serverTextMu.Lock()
	serverTextFn = translate
	serverTextMu.Unlock()
}

// ServerText writes an English server text in ctx's language when the
// handlers know it, and returns it unchanged otherwise, such as a mail
// server's own words.
func ServerText(ctx context.Context, text string) string {
	serverTextMu.RLock()
	translate := serverTextFn
	serverTextMu.RUnlock()
	if translate == nil || text == "" {
		return text
	}
	return translate(ctx, text)
}
