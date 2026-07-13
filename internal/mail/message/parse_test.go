package message

import (
	"strings"
	"testing"
)

func TestGenerateSnippetPrefersPlainText(t *testing.T) {
	got := GenerateSnippet("Plain text body", []byte(`<html><body><p>HTML body</p></body></html>`))
	if got != "Plain text body" {
		t.Fatalf("GenerateSnippet() = %q, want %q", got, "Plain text body")
	}
}

func TestGenerateSnippetFallsBackToCleanHTML(t *testing.T) {
	html := []byte(`<!doctype html>
<html>
	<head>
		<style>body { color: red; } .preview { display: none; }</style>
		<script>alert("ignore")</script>
	</head>
	<body>
		<div hidden>Hidden text</div>
		<p>Hello&nbsp;<strong>there</strong>.</p>
		<p>Second line</p>
	</body>
</html>`)

	got := GenerateSnippet("", html)
	want := "Hello there. Second line"
	if got != want {
		t.Fatalf("GenerateSnippet() = %q, want %q", got, want)
	}
}

func TestGenerateSnippetCleansHTMLInPlainTextPart(t *testing.T) {
	text := `<html><head><style>body { color: red; }</style></head><body><p>Real message</p></body></html>`

	got := GenerateSnippet(text, nil)
	want := "Real message"
	if got != want {
		t.Fatalf("GenerateSnippet() = %q, want %q", got, want)
	}
}

func TestGenerateSnippetFallsBackWhenPlainTextIsCSS(t *testing.T) {
	text := `body { color: red; } .wrapper { display: block; }`
	html := []byte(`<html><body><p>Readable body</p></body></html>`)

	got := GenerateSnippet(text, html)
	want := "Readable body"
	if got != want {
		t.Fatalf("GenerateSnippet() = %q, want %q", got, want)
	}
}

func TestPreviewFromHTMLUsesImageAltText(t *testing.T) {
	got := PreviewFromHTML([]byte(`<p>Invoice attached <img src="cid:1" alt="PDF icon"></p>`))
	want := "Invoice attached PDF icon"
	if got != want {
		t.Fatalf("PreviewFromHTML() = %q, want %q", got, want)
	}
}

func TestTextFromHTMLReturnsCompleteVisibleText(t *testing.T) {
	longText := "Searchable opening " + strings.Repeat("content ", 40) + "deepbodytoken"
	html := []byte(`<html><head><style>.hidden { display: none; }</style><script>ignore()</script></head><body><p>` + longText + `</p></body></html>`)

	got := TextFromHTML(html)
	if !strings.Contains(got, "deepbodytoken") {
		t.Fatalf("TextFromHTML() truncated searchable content: %q", got)
	}
	if strings.Contains(got, "display: none") || strings.Contains(got, "ignore()") {
		t.Fatalf("TextFromHTML() included non-visible content: %q", got)
	}
}
