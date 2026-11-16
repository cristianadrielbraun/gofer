package handler

import (
	"strings"

	"golang.org/x/net/html"
)

// hasLeadingHTMLDoctype reports whether a full document starts with a doctype,
// allowing only comments and whitespace before it. Tokenizing avoids treating
// a doctype-like string inside a comment or style block as a real declaration.
func hasLeadingHTMLDoctype(document string) bool {
	tokenizer := html.NewTokenizer(strings.NewReader(document))
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			return false
		case html.CommentToken:
			continue
		case html.TextToken:
			text := strings.TrimPrefix(string(tokenizer.Text()), "\ufeff")
			if strings.TrimSpace(text) == "" {
				continue
			}
			return false
		case html.DoctypeToken:
			name := strings.Fields(tokenizer.Token().Data)
			return len(name) > 0 && strings.EqualFold(name[0], "html")
		default:
			return false
		}
	}
}
