package handler

import (
	"context"
	"strings"
	"testing"
)

func TestBuildBodyDocumentUsesStandardsModeForFullDocuments(t *testing.T) {
	const missingDoctype = `<html><head><style>div{height:100%!important;width:100%!important}.copy{line-height:100%}img{width:200px;height:100px}</style></head><body><div class="copy">Message</div><img src="cid:logo" width="200" height="100"></body></html>`
	const existingDoctype = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head></head><body>Message</body></html>`
	const commentBeforeDoctype = `<!-- lead --><!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head></head><body>Message</body></html>`
	const fakeDoctypeComment = `<!-- <!doctype html> --><html><head></head><body>Message</body></html>`
	resize := []byte(`<script>resize()</script>`)

	for _, tc := range []struct {
		name     string
		body     string
		theme    string
		original bool
		wantDoc  string
		countDoc bool
		preserve bool
	}{
		{name: "normal full document", body: missingDoctype, wantDoc: "<!DOCTYPE html>", countDoc: true, preserve: true},
		{name: "dark full document", body: missingDoctype, theme: "dark", wantDoc: "<!DOCTYPE html>", countDoc: true, preserve: true},
		{name: "original full document", body: missingDoctype, original: true, wantDoc: "<!DOCTYPE html>", countDoc: true, preserve: true},
		{name: "preserves existing doctype", body: existingDoctype, wantDoc: "<!DOCTYPE html PUBLIC", countDoc: true},
		{name: "preserves doctype after comment", body: commentBeforeDoctype, wantDoc: "<!-- lead --><!DOCTYPE html PUBLIC", countDoc: true},
		{name: "does not trust comment text", body: fakeDoctypeComment, wantDoc: "<!DOCTYPE html><!-- <!doctype html> -->"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theme := tc.theme
			if theme == "" {
				theme = "light"
			}
			got := string(buildBodyDocument(context.Background(), []byte(tc.body), resize, theme, "", "", "", tc.original, 14, ""))
			if !strings.HasPrefix(got, tc.wantDoc) {
				t.Fatalf("document starts with %q, want %q", got[:min(len(got), 48)], tc.wantDoc)
			}
			if tc.countDoc && strings.Count(strings.ToLower(got), "<!doctype") != 1 {
				t.Fatalf("document has %d doctypes, want exactly one", strings.Count(strings.ToLower(got), "<!doctype"))
			}
			if tc.preserve {
				for _, css := range []string{`height:100%!important;width:100%!important`, `.copy{line-height:100%}`, `img{width:200px;height:100px}`} {
					if !strings.Contains(got, css) {
						t.Fatalf("message CSS was changed: %s", got)
					}
				}
				if !strings.Contains(got, `width="200" height="100"`) {
					t.Fatalf("image dimensions were changed: %s", got)
				}
			}
		})
	}
}

func TestHasLeadingHTMLDoctype(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want bool
	}{
		{name: "doctype", doc: `<!DOCTYPE html><html></html>`, want: true},
		{name: "whitespace and BOM", doc: "\ufeff \r\n<!doctype html><html></html>", want: true},
		{name: "comment before doctype", doc: `<!-- lead --><!doctype html><html></html>`, want: true},
		{name: "fake doctype in comment", doc: `<!-- <!doctype html> --><html></html>`},
		{name: "wrong doctype name", doc: `<!doctypex><html></html>`},
		{name: "content before doctype", doc: `<html><!doctype html></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasLeadingHTMLDoctype(tc.doc); got != tc.want {
				t.Fatalf("hasLeadingHTMLDoctype(%q) = %v, want %v", tc.doc, got, tc.want)
			}
		})
	}
}

func TestBuildBodyDocumentFragmentKeepsStandardsWrapper(t *testing.T) {
	got := string(buildBodyDocument(context.Background(), []byte(`<div>Message</div>`), nil, "light", "", "", "", false, 14, ""))
	if !strings.HasPrefix(got, "<!DOCTYPE html><html>") {
		t.Fatalf("fragment wrapper = %q, want standards document", got[:min(len(got), 48)])
	}
}
