package views

import (
	"html"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/a-h/templ/parser/v2"
)

// Text people read goes through i18n.T, so every page can be shown in any
// language. This test reads the templates and fails on text or labels written
// straight into the markup, except for the names and technical terms below,
// which read the same in every language.
var untranslatedTextAllowed = map[string]bool{
	// Names of products, protocols and services.
	"Gofer": true, "Gmail": true, "Outlook": true, "iCloud": true, "iCloud Mail": true,
	"(iCloud Mail)": true, "JMAP": true, "(JMAP)": true, "(IMAP)": true, "(SMTP)": true,
	"IMAP / SMTP": true, "HTML": true, "UTC": true, "INBOX": true, "IDLE": true, "CC BY 3.0": true,
	"CalDAV": true, "CardDAV": true, "TLS": true, "STARTTLS": true, "PLAIN": true, "OAuth2": true,
	"CC": true, "BCC": true, "PDF": true, "Gofer Admin": true, "Gofer -": true,
	// Settings, commands and paths an administrator types as shown.
	"GOFER_OUTLOOK_WEBHOOKS=true": true, "GOFER_JMAP_EXPERIMENT=true": true, "GOFER_BASE_URL": true,
	"gofer auth users list": true, "/webhooks/outlook": true, "mailto:": true, "etag": true,
	// Sample addresses and people in examples and previews.
	"jane@work.example": true, "jane@example.com": true, "lab.example.test": true, "mailhog.local": true,
	"Ana": true, "Alex Smith": true, "Cristian Braun": true, "CB": true,
	"cristian@gofer.email": true, "design@gofer.email": true,
	// Values for scripts, never shown.
	"default": true, "continue": true, "sign-in": true,
}

// Components Gofer has but doesn't use; their English never reaches a page.
var untranslatedTextUnusedComponents = map[string]bool{
	"breadcrumb": true, "carousel": true, "pagination": true, "sidebar": true, "slider": true, "tabs": true,
}

var (
	readableText      = regexp.MustCompile(`[A-Za-z]{2}`)
	readableAttribute = map[string]bool{"title": true, "aria-label": true, "aria-description": true, "placeholder": true, "alt": true, "label": true}
	textNodeType      = reflect.TypeOf(parser.Text{})
	constantAttrType  = reflect.TypeOf(parser.ConstantAttribute{})
)

func TestTemplatesHaveNoUntranslatedText(t *testing.T) {
	files, _ := filepath.Glob("*.templ")
	components, _ := filepath.Glob("../../components/*/*.templ")
	for _, file := range components {
		if !untranslatedTextUnusedComponents[filepath.Base(filepath.Dir(file))] {
			files = append(files, file)
		}
	}
	if len(files) < 50 {
		t.Fatalf("found only %d templates", len(files))
	}
	var found []string
	for _, file := range files {
		tf, err := parser.Parse(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		visitTemplateNodes(reflect.ValueOf(tf), func(text string, line uint32) {
			text = strings.TrimSpace(html.UnescapeString(text))
			if readableText.MatchString(text) && !untranslatedTextAllowed[text] {
				found = append(found, filepath.Base(file)+":"+itoa(int(line)+1)+": "+text)
			}
		})
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("untranslated text, use i18n.T: %s", f)
	}
}

func visitTemplateNodes(v reflect.Value, report func(text string, line uint32)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			visitTemplateNodes(v.Elem(), report)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			visitTemplateNodes(v.Index(i), report)
		}
	case reflect.Struct:
		switch v.Type() {
		case textNodeType:
			text := v.Interface().(parser.Text)
			report(text.Value, text.Range.From.Line)
			return
		case constantAttrType:
			attr := v.Interface().(parser.ConstantAttribute)
			if readableAttribute[attr.Key.String()] {
				report(attr.Value, attr.Range.From.Line)
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				visitTemplateNodes(v.Field(i), report)
			}
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
