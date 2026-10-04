package calendar

import (
	"html"
	"net/url"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var descriptionMarkup = regexp.MustCompile(`(?i)</?(?:html|head|body|div|p|br|span|a|ul|ol|li|table|tr|td|th|h[1-6]|b|strong|em|i|u|s|strike|blockquote|pre|code|script|style|img|hr)(?:\s[^>]*|/?)>`)

// DescriptionHTML is the safe, theme-neutral fragment used inside the app.
// Unlike email HTML displayed in an isolated document, calendar HTML shares
// the app's DOM. Never retain CSS, IDs, HTMX attributes, images or active content.
func DescriptionHTML(value string) string {
	if !descriptionMarkup.MatchString(value) {
		return strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(value, "\r\n", "\n")), "\n", "<br>")
	}
	return SanitizeDescriptionHTML(value)
}

func SanitizeDescriptionHTML(value string) string {
	context := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(strings.NewReader(value), context)
	if err != nil {
		return ""
	}
	allowed := map[string]bool{"div": true, "p": true, "br": true, "span": true, "a": true, "ul": true, "ol": true, "li": true, "table": true, "tbody": true, "thead": true, "tr": true, "td": true, "th": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "b": true, "strong": true, "em": true, "i": true, "u": true, "s": true, "strike": true, "blockquote": true, "pre": true, "code": true, "hr": true}
	blocked := map[string]bool{"script": true, "style": true, "head": true, "iframe": true, "object": true, "embed": true, "svg": true, "math": true, "template": true}
	var out strings.Builder
	var render func(*xhtml.Node)
	render = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			out.WriteString(html.EscapeString(n.Data))
			return
		}
		if n.Type != xhtml.ElementNode || blocked[n.Data] || n.Namespace != "" {
			return
		}
		tag := n.Data
		if allowed[tag] {
			out.WriteString("<" + tag)
			for _, attr := range n.Attr {
				if tag == "a" && attr.Key == "href" {
					u, err := url.Parse(strings.TrimSpace(attr.Val))
					if err == nil && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "mailto") {
						out.WriteString(` href="` + html.EscapeString(u.String()) + `" target="_blank" rel="noopener noreferrer"`)
					}
				}
				if attr.Key == "style" {
					var styles []string
					for _, declaration := range strings.Split(strings.ToLower(attr.Val), ";") {
						parts := strings.SplitN(declaration, ":", 2)
						if len(parts) != 2 {
							continue
						}
						key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
						valid := (key == "font-weight" && (val == "bold" || val == "700")) || (key == "font-style" && val == "italic") || (key == "text-decoration" && (val == "underline" || val == "line-through")) || (key == "text-align" && (val == "left" || val == "center" || val == "right" || val == "justify"))
						if valid {
							styles = append(styles, key+":"+val)
						}
					}
					if len(styles) > 0 {
						out.WriteString(` style="` + strings.Join(styles, ";") + `"`)
					}
				}
			}
			out.WriteString(">")
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			render(child)
		}
		if allowed[tag] && tag != "br" && tag != "hr" {
			out.WriteString("</" + tag + ">")
		}
	}
	for _, node := range nodes {
		render(node)
	}
	return strings.TrimSpace(out.String())
}

// DescriptionPlainText supplies a readable fallback for iCalendar clients.
func DescriptionPlainText(value string) string {
	z := xhtml.NewTokenizer(strings.NewReader(SanitizeDescriptionHTML(value)))
	var out strings.Builder
	for kind := z.Next(); kind != xhtml.ErrorToken; kind = z.Next() {
		token := z.Token()
		if kind == xhtml.TextToken {
			out.WriteString(token.Data)
		}
		if token.Data == "br" || (kind == xhtml.EndTagToken && (token.Data == "div" || token.Data == "p" || token.Data == "li" || token.Data == "tr" || token.Data == "blockquote" || strings.HasPrefix(token.Data, "h"))) {
			out.WriteByte('\n')
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(out.String(), "\u00a0", " "))
}

func DraftDescription(draft EventDraft) string {
	if draft.DescriptionHTML != nil {
		return *draft.DescriptionHTML
	}
	return draft.Description
}
