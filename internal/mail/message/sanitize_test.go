package message

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var sanitizerAttacks = map[string]string{
	"slash-separated handler":   `<img/src=x/onerror=alert(1)>`,
	"svg animation handler":     `<svg><animate onbegin=alert(1) attributeName=x dur=1s></svg>`,
	"entity-encoded scheme":     `<a href="java&#115;cript:alert(1)">x</a>`,
	"unquoted script link":      `<a href=javascript:alert(1)>x</a>`,
	"whitespace in scheme":      "<a href=\"java\nscript:alert(1)\">x</a>",
	"vbscript link":             `<a href="vbscript:msgbox(1)">x</a>`,
	"data html link":            `<a href="data:text/html,<script>alert(1)</script>">x</a>`,
	"nested script":             `<scr<script>ipt>alert(1)</script>`,
	"script":                    `<script>alert(1)</script>`,
	"svg script":                `<svg><script>alert(1)</script></svg>`,
	"svg style breakout":        `<svg><style><img src=x onerror=alert(1)></style></svg>`,
	"math style breakout":       `<math><mtext><table><mglyph><style><img src=x onerror=alert(1)></style></mglyph></table></mtext></math>`,
	"noscript breakout":         `<noscript><p title="</noscript><img src=x onerror=alert(1)>"></noscript>`,
	"iframe srcdoc":             `<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
	"object":                    `<object data="https://track.example/x.swf"></object>`,
	"embed":                     `<embed src="https://track.example/x.swf">`,
	"form":                      `<form action="https://evil.example/"><input name=p><button formaction="https://evil.example/">Go</button></form>`,
	"base":                      `<base href="https://evil.example/">`,
	"meta refresh":              `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
	"stylesheet link":           `<link rel=stylesheet href="https://track.example/a.css">`,
	"comment":                   `<!--<img src=x onerror=alert(1)>-->`,
	"relative link to Gofer":    `<a href="/settings">x</a>`,
	"svg remote image":          `<svg><image href="https://track.example/p.gif"/></svg>`,
	"svg javascript link":       `<svg><a xlink:href="javascript:alert(1)"><text>x</text></a></svg>`,
	"mathml href":               `<math href="javascript:alert(1)"><mi>x</mi></math>`,
	"traversal in Gofer path":   `<img src="/api/inline-content/../../settings">`,
	"encoded traversal":         `<img src="/api/inline-content/%2e%2e/%2e%2e/settings">`,
	"spoofed remote marker":     `<img src="" data-remote-src="javascript:alert(1)">`,
	"spoofed restorable marker": `<div data-gofer-remote-onclick="alert(1)" data-gofer-remote-href="https://evil.example/">x</div>`,
	"named image clobbering":    `<img name="querySelectorAll" src="cid:a">`,
	"link target":               `<a href="https://example.com/" target="_top">x</a>`,
}

// Tracking and remote loads that must be marked, never left loadable.
var sanitizerRemoteLoads = map[string]string{
	"img src":         `<img src="https://track.example/p.gif">`,
	"unquoted src":    `<img src=https://track.example/p.gif>`,
	"srcset":          `<img srcset="https://track.example/p.gif 1x">`,
	"background":      `<table background="https://track.example/bg.gif"><tr><td>x</td></tr></table>`,
	"import":          `<style>@import "https://track.example/a.css";</style>`,
	"protocol url":    `<div style="background:url(//track.example/p.gif)">x</div>`,
	"style url":       `<style>body{background:url('https://track.example/p.gif')}</style>`,
	"poster":          `<video poster="https://track.example/p.gif"></video>`,
	"picture source":  `<picture><source srcset="https://track.example/p.gif"><img src="cid:a"></picture>`,
	"image input":     `<input type=image src="https://track.example/p.gif">`,
	"font face":       `<style>@font-face{font-family:x;src:url(https://track.example/f.woff)}</style>`,
	"uppercase http":  `<img src="HTTPS://track.example/p.gif">`,
	"whitespace host": "<img src=\" \thttps://track.example/p.gif\">",
}

// assertInert parses the output the way a browser would and fails on anything
// that runs code or loads from the web.
func assertInert(t *testing.T, name, output string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(output))
	if err != nil {
		t.Fatalf("%s: parse output: %v", name, err)
	}
	for n := range doc.Descendants() {
		if n.Type == html.TextNode && n.Parent != nil && n.Parent.Data == "style" && strings.Contains(strings.ToLower(n.Data), "url(\"http") {
			t.Errorf("%s: style loads from the web: %q", name, output)
		}
		if n.Type != html.ElementNode {
			continue
		}
		switch strings.ToLower(n.Data) {
		case "script", "iframe", "object", "embed", "form", "base", "meta", "link", "animate", "set", "noscript":
			t.Errorf("%s: kept <%s>: %q", name, n.Data, output)
		}
		for _, a := range n.Attr {
			key, value := strings.ToLower(a.Key), strings.ToLower(cleanURL(a.Val))
			switch {
			case strings.HasPrefix(key, "on"):
				t.Errorf("%s: kept handler %s: %q", name, a.Key, output)
			case strings.Contains(value, "javascript:"), strings.Contains(value, "vbscript:"), strings.HasPrefix(value, "data:text"):
				t.Errorf("%s: kept script URL in %s: %q", name, a.Key, output)
			case (key == "src" || key == "srcset" || key == "background" || key == "poster" || key == "href" && n.Data != "a") && (strings.Contains(value, "track.example") || strings.Contains(value, "..") || strings.Contains(value, "%2e")):
				t.Errorf("%s: %s still loads %q: %q", name, a.Key, a.Val, output)
			case key == "style" && strings.Contains(value, "url(\"http"):
				t.Errorf("%s: style attribute loads from the web: %q", name, output)
			case key == "href" && n.Data == "a" && strings.HasPrefix(value, "/"):
				t.Errorf("%s: kept a link to Gofer's own address: %q", name, output)
			case key == "name" && n.Data == "img", key == "target", key == "formaction", key == "action":
				t.Errorf("%s: kept %s: %q", name, a.Key, output)
			case strings.HasPrefix(key, remoteAttrPrefix) && !restorableRemoteAttributes[strings.TrimPrefix(key, remoteAttrPrefix)]:
				t.Errorf("%s: kept a marker that restores %s: %q", name, a.Key, output)
			}
		}
	}
}

func TestSanitizeHTMLRemovesCode(t *testing.T) {
	for name, input := range sanitizerAttacks {
		out := string(SanitizeHTML([]byte(input)))
		assertInert(t, name, out)
		// Bodies are sanitized again after translation, and a browser may
		// parse the output differently from the input, so a second pass
		// must find nothing to undo either.
		assertInert(t, name+" (second pass)", string(SanitizeHTML([]byte(out))))
	}
}

func TestSanitizeHTMLMarksRemoteContent(t *testing.T) {
	for name, input := range sanitizerRemoteLoads {
		out := string(SanitizeHTML([]byte(input)))
		assertInert(t, name, out)
		if !IsRemoteImagesBlocked(out) {
			t.Errorf("%s: remote content not reported as blocked: %q", name, out)
		}
		restored := string(RestoreRemoteContent([]byte(out), func(u string) string { return u }))
		if !strings.Contains(strings.ToLower(restored), "https://track.example/") || IsRemoteImagesBlocked(strings.ReplaceAll(restored, remoteMarkerAttr+`=""`, "")) {
			t.Errorf("%s: restoring did not bring the remote content back: %q", name, restored)
		}
		var loaded []string
		relayed := string(RestoreRemoteContent([]byte(out), func(u string) string {
			loaded = append(loaded, u)
			return "/relay?n=1&amp=2"
		}))
		if len(loaded) == 0 || strings.Contains(strings.ToLower(relayed), "track.example") || !strings.Contains(relayed, "/relay?n=1&amp") {
			t.Errorf("%s: restoring did not send the remote content through the relay: %q", name, relayed)
		}
		for _, u := range loaded {
			if !strings.HasPrefix(strings.ToLower(u), "https://track.example/") {
				t.Errorf("%s: relay was asked for %q", name, u)
			}
		}
		if again := string(SanitizeHTML([]byte(out))); again != out {
			t.Errorf("%s: second pass changed %q to %q", name, out, again)
		}
	}
}

func TestSanitizeHTMLKeepsOrdinaryMail(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"formatting", `<p class="intro" style="color:#333;font-size:14px">Hello <b>there</b>, <i>friend</i></p>`, `<p class="intro" style="color:#333;font-size:14px">Hello <b>there</b>, <i>friend</i></p>`},
		{"layout table", `<table width="600" cellpadding="0" bgcolor="#ffffff" align="center"><tr><td valign="top">x</td></tr></table>`, `<table width="600" cellpadding="0" bgcolor="#ffffff" align="center"><tbody><tr><td valign="top">x</td></tr></tbody></table>`},
		{"web link", `<a href="https://example.com/a?b=1&amp;c=2">x</a>`, `<a href="https://example.com/a?b=1&amp;c=2">x</a>`},
		{"mail and phone links", `<a href="mailto:ana@example.com">m</a><a href="tel:+100">t</a><a href="#top">j</a>`, `<a href="mailto:ana@example.com">m</a><a href="tel:+100">t</a><a href="#top">j</a>`},
		{"inline part", `<img src="cid:logo@example" alt="Logo" width="120">`, `<img src="cid:logo@example" alt="Logo" width="120"/>`},
		{"embedded image", `<img src="data:image/png;base64,iVBORw0KGgo=">`, `<img src="data:image/png;base64,iVBORw0KGgo="/>`},
		{"gofer inline path", `<img src="/api/inline-content/7/logo%20one">`, `<img src="/api/inline-content/7/logo%20one"/>`},
		{"style element", `<style>.a > .b{color:red}</style><p class="a">x</p>`, `<style>.a > .b{color:red}</style><p class="a">x</p>`},
		{"svg case", `<svg viewBox="0 0 10 10"><linearGradient id="g"></linearGradient></svg>`, `<svg viewBox="0 0 10 10"><linearGradient id="g"></linearGradient></svg>`},
		{"form content kept", `<form><p>Reply by mail</p></form>`, `<p>Reply by mail</p>`},
	} {
		if got := string(SanitizeHTML([]byte(tc.input))); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestSanitizeHTMLKeepsDocumentShape(t *testing.T) {
	fragment := string(SanitizeHTML([]byte(`<p>Hi</p>`)))
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, "<body") {
		t.Fatalf("fragment became a document: %q", fragment)
	}
	doc := string(SanitizeHTML([]byte(`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head><title>Note</title><style>p{margin:0}</style></head><body bgcolor="#eeeeee"><p>Hi</p></body></html>`)))
	for _, want := range []string{`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN"`, `<style>p{margin:0}</style>`, `<body bgcolor="#eeeeee">`} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document lost %q: %q", want, doc)
		}
	}
}

func TestRemoteImageDownloadsUseDecodedAddresses(t *testing.T) {
	out := SanitizeHTML([]byte(`<img src="https://images.example/p.png?a=1&amp;b=2">`))
	urls := ExtractRemoteURLs(string(out))
	if len(urls) != 1 || urls[0] != "https://images.example/p.png?a=1&b=2" {
		t.Fatalf("ExtractRemoteURLs = %q", urls)
	}
	local := RewriteToLocalAssets(out, map[string]string{urls[0]: "/api/remote-assets/1/0123456789abcdef.png"})
	if !bytes.Contains(local, []byte(`src="/api/remote-assets/1/0123456789abcdef.png"`)) || bytes.Contains(local, []byte(remoteImageSrcAttr)) {
		t.Fatalf("RewriteToLocalAssets = %q", local)
	}
}

func TestSanitizeOutgoingHTMLKeepsRemoteImages(t *testing.T) {
	got := string(SanitizeOutgoingHTML([]byte(`<p>Hi</p><img src="https://example.com/logo.png" alt="Logo"><img src="cid:part1@example.com"><img src="x" onerror="alert(1)"><script>alert(1)</script>`)))
	if !strings.Contains(got, `src="https://example.com/logo.png"`) {
		t.Fatalf("remote image src not kept: %s", got)
	}
	if strings.Contains(got, "data-remote-src") {
		t.Fatalf("remote image still blocked: %s", got)
	}
	if !strings.Contains(got, `src="cid:part1@example.com"`) {
		t.Fatalf("inline part reference not kept: %s", got)
	}
	if strings.Contains(got, "onerror") || strings.Contains(got, "<script") {
		t.Fatalf("unsafe markup kept: %s", got)
	}
}
