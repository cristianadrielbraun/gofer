package message

import (
	"bytes"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SanitizeHTML makes a message's HTML safe to show in the reader. The HTML is
// parsed the way a browser parses it, and rebuilt without scripts, embedded
// pages, forms, event handlers or dangerous links. Everything it would load
// from the web is marked instead of loaded; RestoreRemoteContent turns it back
// on when the reader allows remote content. The reader's Content-Security-
// Policy enforces the same limits in the browser, so this is the first of two
// layers rather than the only one.
func SanitizeHTML(input []byte) []byte {
	if len(input) == 0 {
		return nil
	}
	return sanitizeHTML(input)
}

func SanitizeOriginalHTML(input []byte) []byte {
	return SanitizeHTML(input)
}

// SanitizeOutgoingHTML cleans HTML the user wrote to send or keep as a
// draft. It drops what SanitizeHTML drops, but remote images and other
// remote content stay as plain web addresses: recipients load them as with
// any message, and blocking them would send the message without them.
func SanitizeOutgoingHTML(input []byte) []byte {
	return RestoreRemoteContent(SanitizeHTML(input), func(remoteURL string) string { return remoteURL })
}

func RewriteCIDReferences(html []byte, cidToURL map[string]string) []byte {
	if len(cidToURL) == 0 {
		return html
	}
	s := string(html)
	for cid, url := range cidToURL {
		s = strings.ReplaceAll(s, `cid:`+cid, url)
	}
	return []byte(s)
}

// Marks left by the sanitizer. An <img> keeps the original format,
// src="" data-remote-src="URL", which saved drafts, signatures and remote
// asset downloads rely on. Other attributes are renamed with a prefix, and a
// CSS url() is given a scheme the browser can't load.
const (
	remoteMarkerAttr    = "data-gofer-remote"
	remoteAttrPrefix    = "data-gofer-remote-"
	remoteCSSURLPrefix  = "gofer-remote:"
	remoteImageSrcAttr  = "data-remote-src"
	removedImageSrcAttr = "data-removed-src"
)

// Elements removed together with everything inside them.
var droppedElements = map[string]bool{
	"script": true, "iframe": true, "frame": true, "frameset": true, "object": true,
	"embed": true, "applet": true, "param": true, "noscript": true, "noembed": true,
	"noframes": true, "template": true, "plaintext": true, "xmp": true, "portal": true,
	"fencedframe": true, "meta": true, "link": true, "base": true,
	// SVG elements that run code or change attributes over time.
	"handler": true, "listener": true, "set": true, "animate": true,
	"animatemotion": true, "animatetransform": true,
}

// Elements removed while their content stays.
var unwrappedElements = map[string]bool{"form": true}

// Attributes removed wherever they appear.
var droppedAttributes = map[string]bool{
	"action": true, "formaction": true, "srcdoc": true, "http-equiv": true, "ping": true,
	"target": true, "codebase": true, "archive": true, "manifest": true, "longdesc": true,
	"profile": true,
}

// Attributes the browser fetches.
var resourceAttributes = map[string]bool{
	"src": true, "background": true, "poster": true, "lowsrc": true, "dynsrc": true, "data": true,
}

// Remote attributes that can be marked and restored. Others are removed.
var restorableRemoteAttributes = map[string]bool{"src": true, "srcset": true, "background": true, "poster": true}

func sanitizeHTML(input []byte) []byte {
	var out bytes.Buffer
	if isFullDocument(input) {
		doc, err := html.Parse(bytes.NewReader(input))
		if err != nil {
			return nil
		}
		sanitizeChildren(doc)
		if err := html.Render(&out, doc); err != nil {
			return nil
		}
		return out.Bytes()
	}
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(bytes.NewReader(input), body)
	if err != nil {
		return nil
	}
	for _, n := range nodes {
		body.AppendChild(n)
	}
	sanitizeChildren(body)
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&out, c); err != nil {
			return nil
		}
	}
	return out.Bytes()
}

// isFullDocument keeps a fragment a fragment, since the reader styles bare
// fragments differently from complete documents.
func isFullDocument(input []byte) bool {
	return bytes.Contains(bytes.ToLower(input), []byte("<html"))
}

func sanitizeChildren(parent *html.Node) {
	for n := parent.FirstChild; n != nil; {
		next := n.NextSibling
		switch n.Type {
		case html.CommentNode:
			parent.RemoveChild(n)
		case html.ElementNode:
			name := strings.ToLower(n.Data)
			switch {
			case droppedElements[name]:
				parent.RemoveChild(n)
			case unwrappedElements[name] && n.Namespace == "":
				sanitizeChildren(n)
				for c := n.FirstChild; c != nil; {
					after := c.NextSibling
					n.RemoveChild(c)
					parent.InsertBefore(c, n)
					c = after
				}
				parent.RemoveChild(n)
			default:
				sanitizeElement(n)
				sanitizeChildren(n)
			}
		case html.TextNode:
			if parent.Type == html.ElementNode && parent.Data == "style" {
				css, remote := sanitizeCSS(n.Data)
				n.Data = css
				if remote {
					markRemote(parent)
				}
			}
		}
		n = next
	}
}

func sanitizeElement(n *html.Node) {
	name := strings.ToLower(n.Data)
	isHTMLImage := n.Namespace == "" && name == "img"
	attrs := make([]html.Attribute, 0, len(n.Attr))
	remote := false
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		switch {
		case strings.HasPrefix(key, "on"), droppedAttributes[key]:
			continue
		case key == remoteMarkerAttr || key == removedImageSrcAttr:
			// Left by an earlier pass; restoring them loads nothing.
		case key == remoteImageSrcAttr || strings.HasPrefix(key, remoteAttrPrefix):
			// Left by an earlier pass. Kept only when they would restore a
			// web address the reader can choose to load.
			target := strings.TrimPrefix(key, remoteAttrPrefix)
			if key == remoteImageSrcAttr {
				target = "src"
			}
			value, kind := classifyResource(target, a.Val)
			if kind != resourceRemote || !restorableRemoteAttributes[target] || (key == remoteImageSrcAttr && !isHTMLImage) {
				continue
			}
			a.Val = value
			remote = true
		case key == "name" && isHTMLImage:
			// A named <img> shadows document properties the reader's scripts use.
			continue
		case key == "style":
			css, found := sanitizeCSS(a.Val)
			a.Val = css
			remote = remote || found
		case key == "href" && isLinkElement(n):
			value, ok := safeLinkURL(a.Val)
			if !ok {
				continue
			}
			a.Val = value
		case key == "href", key == "srcset", resourceAttributes[key]:
			value, kind := classifyResource(key, a.Val)
			switch kind {
			case resourceDropped:
				continue
			case resourceRemote:
				if a.Namespace != "" || !restorableRemoteAttributes[key] {
					continue
				}
				remote = true
				if isHTMLImage && key == "src" {
					attrs = append(attrs, html.Attribute{Key: "src"}, html.Attribute{Key: remoteImageSrcAttr, Val: value})
					continue
				}
				a = html.Attribute{Key: remoteAttrPrefix + key, Val: value}
			default:
				a.Val = value
			}
		case key == "cite":
			value, ok := safeLinkURL(a.Val)
			if !ok || !isWebURL(value) {
				continue
			}
			a.Val = value
		}
		attrs = append(attrs, a)
	}
	n.Attr = attrs
	if remote {
		markRemote(n)
	}
}

func markRemote(n *html.Node) {
	for _, a := range n.Attr {
		if a.Key == remoteMarkerAttr {
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: remoteMarkerAttr})
}

func isLinkElement(n *html.Node) bool {
	name := strings.ToLower(n.Data)
	return name == "a" || (n.Namespace == "" && name == "area")
}

// cleanURL strips what browsers ignore before reading a URL's scheme, so
// "java\nscript:" can't slip past a prefix check.
func cleanURL(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, value)
	return strings.TrimFunc(value, func(r rune) bool { return r <= ' ' })
}

func isWebURL(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// safeLinkURL keeps links that leave the message for the web, mail, a phone
// number, an inline part, or a place in the message. Anything else, including
// script URLs and paths on Gofer's own address, is dropped.
func safeLinkURL(value string) (string, bool) {
	value = cleanURL(value)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(value, "#"):
		return value, true
	case strings.HasPrefix(value, "//"):
		return "https:" + value, true
	case isWebURL(value), strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "tel:"), strings.HasPrefix(lower, "cid:"):
		return value, true
	}
	return "", false
}

// Gofer rewrites inline parts and saved remote images to these paths, and
// some bodies are sanitized again after that, as when they are translated.
var goferContentPaths = []string{"/api/inline-content/", "/api/remote-assets/", "/api/attachments/", "/compose/attachments/"}

func isGoferContentPath(value string) bool {
	if strings.Contains(value, "..") || strings.Contains(strings.ToLower(value), "%2e") || strings.ContainsAny(value, `\?#`) {
		return false
	}
	for _, prefix := range goferContentPaths {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

type resourceKind int

const (
	resourceDropped resourceKind = iota
	resourceLocal
	resourceRemote
)

var safeDataImage = regexp.MustCompile(`(?i)^data:image/(?:png|jpe?g|gif|webp|bmp|svg\+xml)[;,]`)

func classifyResource(key, value string) (string, resourceKind) {
	if key == "srcset" {
		return classifySrcset(value)
	}
	value = cleanURL(value)
	lower := strings.ToLower(value)
	switch {
	case value == "":
		return value, resourceLocal
	case strings.HasPrefix(lower, "cid:"), safeDataImage.MatchString(value), isGoferContentPath(value):
		return value, resourceLocal
	case key == "href" && strings.HasPrefix(value, "#"):
		return value, resourceLocal
	case strings.HasPrefix(value, "//"):
		return "https:" + value, resourceRemote
	case isWebURL(value):
		return value, resourceRemote
	}
	return "", resourceDropped
}

// A srcset is remote if any of its candidates is; candidates that are neither
// remote nor inline parts drop the whole attribute.
func classifySrcset(value string) (string, resourceKind) {
	kind := resourceLocal
	for _, candidate := range strings.Split(value, ",") {
		fields := strings.Fields(candidate)
		if len(fields) == 0 {
			continue
		}
		_, k := classifyResource("src", fields[0])
		switch k {
		case resourceDropped:
			return "", resourceDropped
		case resourceRemote:
			kind = resourceRemote
		}
	}
	return strings.TrimSpace(value), kind
}

var (
	cssURL    = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"\s]*))\s*\)`)
	cssImport = regexp.MustCompile(`(?i)@import\s+(?:"([^"]*)"|'([^']*)')`)
)

// sanitizeCSS marks remote url()s and @imports, drops ones pointing anywhere
// but an inline part or embedded image, and removes "<" so a stylesheet can't
// close its own element in SVG content.
func sanitizeCSS(css string) (string, bool) {
	remote := false
	rewrite := func(raw string) string {
		value, kind := classifyResource("src", raw)
		switch {
		case kind == resourceRemote && !strings.ContainsAny(value, `"\`):
			remote = true
			return `url("` + remoteCSSURLPrefix + value + `")`
		case kind == resourceLocal && value != "" && !strings.ContainsAny(value, `"\`):
			return `url("` + value + `")`
		}
		return `url("")`
	}
	css = strings.ReplaceAll(css, "<", "")
	css = cssImport.ReplaceAllStringFunc(css, func(match string) string {
		m := cssImport.FindStringSubmatch(match)
		return "@import " + rewrite(m[1]+m[2])
	})
	css = cssURL.ReplaceAllStringFunc(css, func(match string) string {
		m := cssURL.FindStringSubmatch(match)
		value := m[1] + m[2] + m[3]
		if strings.HasPrefix(value, remoteCSSURLPrefix) || value == "" {
			return match
		}
		return rewrite(value)
	})
	return css, remote
}

var restoreCSSURL = regexp.MustCompile(`url\("` + remoteCSSURLPrefix + `([^"]*)"\)`)

// RestoreRemoteContent turns marked remote content back on, with every web
// address passed through load, which returns the address the reader's browser
// should fetch instead. Gofer uses it to send remote content through its own
// server, so the sender never sees the reader's address.
func RestoreRemoteContent(input []byte, load func(remoteURL string) string) []byte {
	if len(input) == 0 {
		return input
	}
	restoreCSS := func(css string) string {
		css = restoreCSSURL.ReplaceAllStringFunc(css, func(match string) string {
			return `url("` + load(restoreCSSURL.FindStringSubmatch(match)[1]) + `")`
		})
		// Bodies sanitized before remote CSS was marked hold empty url()s.
		return strings.ReplaceAll(css, `url("")`, ``)
	}
	restore := func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			if n.Parent != nil && n.Parent.Type == html.ElementNode && n.Parent.Data == "style" {
				n.Data = restoreCSS(n.Data)
			}
			return
		case html.ElementNode:
		default:
			return
		}
		attrs := n.Attr[:0]
		for _, a := range n.Attr {
			switch {
			case a.Key == remoteImageSrcAttr:
				attrs = slices.DeleteFunc(attrs, func(b html.Attribute) bool { return b.Key == "src" })
				a = html.Attribute{Key: "src", Val: load(a.Val)}
			case a.Key == "src" && a.Val == "" && hasAttr(n, remoteImageSrcAttr):
				continue
			case strings.HasPrefix(a.Key, remoteAttrPrefix):
				key := strings.TrimPrefix(a.Key, remoteAttrPrefix)
				if !restorableRemoteAttributes[key] {
					break
				}
				if key == "srcset" {
					a = html.Attribute{Key: key, Val: restoreSrcset(a.Val, load)}
				} else {
					a = html.Attribute{Key: key, Val: load(a.Val)}
				}
			case a.Key == "style":
				a.Val = restoreCSS(a.Val)
			}
			attrs = append(attrs, a)
		}
		n.Attr = attrs
	}
	var out bytes.Buffer
	if isFullDocument(input) {
		doc, err := html.Parse(bytes.NewReader(input))
		if err != nil {
			return input
		}
		for n := range doc.Descendants() {
			restore(n)
		}
		if err := html.Render(&out, doc); err != nil {
			return input
		}
		return out.Bytes()
	}
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(bytes.NewReader(input), body)
	if err != nil {
		return input
	}
	for _, n := range nodes {
		body.AppendChild(n)
	}
	for n := range body.Descendants() {
		restore(n)
	}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&out, c); err != nil {
			return input
		}
	}
	return out.Bytes()
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func restoreSrcset(value string, load func(string) string) string {
	candidates := strings.Split(value, ",")
	for i, candidate := range candidates {
		fields := strings.Fields(candidate)
		if len(fields) == 0 {
			continue
		}
		if _, kind := classifyResource("src", fields[0]); kind == resourceRemote {
			fields[0] = load(fields[0])
		}
		candidates[i] = strings.Join(fields, " ")
	}
	return strings.Join(candidates, ", ")
}

func IsRemoteImagesBlocked(html string) bool {
	return strings.Contains(html, remoteImageSrcAttr) || strings.Contains(html, remoteMarkerAttr) || strings.Contains(html, remoteCSSURLPrefix)
}

var reExtractRemoteSrcs = regexp.MustCompile(`(?i)data-remote-src=["']([^"']+)["']`)

// ExtractRemoteURLs lists the marked remote images to download, decoded from
// their HTML attribute form.
func ExtractRemoteURLs(body string) []string {
	matches := reExtractRemoteSrcs.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool)
	var urls []string
	for _, m := range matches {
		url := html.UnescapeString(m[1])
		if len(m) > 1 && !seen[url] {
			seen[url] = true
			urls = append(urls, url)
		}
	}
	return urls
}

func RewriteToLocalAssets(body []byte, urlToLocal map[string]string) []byte {
	s := string(body)
	for remoteURL, localPath := range urlToLocal {
		for _, form := range []string{html.EscapeString(remoteURL), remoteURL} {
			s = strings.ReplaceAll(s, `src="" data-remote-src="`+form+`"`, `src="`+localPath+`"`)
			s = strings.ReplaceAll(s, `src='' data-remote-src='`+form+`'`, `src='`+localPath+`'`)
			s = strings.ReplaceAll(s, `data-remote-src="`+form+`"`, `src="`+localPath+`"`)
			s = strings.ReplaceAll(s, `data-remote-src='`+form+`'`, `src='`+localPath+`'`)
		}
	}
	s = strings.ReplaceAll(s, `data-remote-src="`, `data-removed-src="`)
	s = strings.ReplaceAll(s, `data-remote-src='`, `data-removed-src='`)
	return []byte(s)
}
