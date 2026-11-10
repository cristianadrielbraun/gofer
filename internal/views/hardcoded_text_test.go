package views

import (
	"html"
	"os"
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
// straight into the markup or given as a Go string to a text prop such as
// Placeholder, except for the names and technical terms below, which read the
// same in every language, and sample addresses or file names.
var untranslatedTextAllowed = map[string]bool{
	// Names of products, protocols and services.
	"Gofer": true, "Gmail": true, "Outlook": true, "iCloud": true, "iCloud Mail": true,
	"(iCloud Mail)": true, "JMAP": true, "(JMAP)": true, "(IMAP)": true, "(SMTP)": true,
	"IMAP / SMTP": true, "HTML": true, "UTC": true, "INBOX": true, "IDLE": true, "CC BY 3.0": true,
	"CalDAV": true, "CardDAV": true, "TLS": true, "STARTTLS": true, "PLAIN": true, "OAuth2": true,
	"CC": true, "BCC": true, "PDF": true, "Gofer Admin": true, "Gofer -": true,
	"IMAP": true, "SMTP": true, "Gmail API": true, "Microsoft Graph": true, "0 MB": true,
	// Settings, commands and paths an administrator types as shown.
	"GOFER_OUTLOOK_WEBHOOKS=true": true, "GOFER_JMAP_EXPERIMENT=true": true, "GOFER_BASE_URL": true,
	"gofer auth users list": true, "/webhooks/outlook": true, "mailto:": true, "etag": true,
	// Sample addresses and people in examples and previews.
	"jane@work.example": true, "jane@example.com": true, "lab.example.test": true, "mailhog.local": true,
	"Ana": true, "Alex Smith": true, "Cristian Braun": true, "CB": true,
	"cristian@gofer.email": true, "design@gofer.email": true, "AS": true, "alex@work.example": true,
	// Values for scripts, never shown.
	"default": true, "continue": true, "sign-in": true,
}

// Components Gofer has but doesn't use; their English never reaches a page.
var untranslatedTextUnusedComponents = map[string]bool{
	"breadcrumb": true, "carousel": true, "sidebar": true, "slider": true, "tabs": true,
}

var (
	readableText      = regexp.MustCompile(`[A-Za-z]{2}`)
	readableAttribute = map[string]bool{"title": true, "aria-label": true, "aria-description": true, "placeholder": true, "alt": true, "label": true}
	textNodeType      = reflect.TypeOf(parser.Text{})
	constantAttrType  = reflect.TypeOf(parser.ConstantAttribute{})
	expressionType    = reflect.TypeOf(parser.Expression{})
	// textLiteral finds Go strings given to a component's text props, such as
	// Placeholder: "…", or to a readable attribute in an attribute map.
	textLiteral = regexp.MustCompile(`\b(?:Placeholder|SearchPlaceholder|Label|Title|Text|Description|AMLabel|PMLabel)\s*:\s*"([^"\\]*)"|"(?:aria-label|aria-description|title|placeholder|alt|data-tooltip)"\s*:\s*"([^"\\]*)"`)
	// sampleValue is an example address, host, path or file name.
	sampleValue = regexp.MustCompile(`^\S*[@./]\S*$`)
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
		}, func(code string, line uint32) {
			for _, match := range textLiteral.FindAllStringSubmatch(code, -1) {
				text := strings.TrimSpace(match[1] + match[2])
				if readableText.MatchString(text) && !sampleValue.MatchString(text) && !untranslatedTextAllowed[text] {
					found = append(found, filepath.Base(file)+":"+itoa(int(line)+1)+": "+text)
				}
			}
		})
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("untranslated text, use i18n.T: %s", f)
	}
}

// visitTemplateNodes reports a template's text and readable attributes to
// report, and its Go code to code.
func visitTemplateNodes(v reflect.Value, report, code func(text string, line uint32)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			visitTemplateNodes(v.Elem(), report, code)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			visitTemplateNodes(v.Index(i), report, code)
		}
	case reflect.Struct:
		switch v.Type() {
		case expressionType:
			expr := v.Interface().(parser.Expression)
			code(expr.Value, expr.Range.From.Line)
			return
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
				visitTemplateNodes(v.Field(i), report, code)
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

// Text a stylesheet draws with content: can't be translated; it comes from
// an attribute set by translated markup or script instead, such as
// content: attr(data-drop-label).
func TestStylesheetsDrawNoText(t *testing.T) {
	files, _ := filepath.Glob("../../assets/css/*.css")
	themes, _ := filepath.Glob("../../assets/css/themes/*.css")
	files = append(files, themes...)
	drawn := regexp.MustCompile(`content:\s*(["'])([^"']*[A-Za-z]{2}[^"']*)(["'])`)
	checked := 0
	for _, file := range files {
		if filepath.Base(file) == "output.css" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if m := drawn.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d draws text %q; set it from a translated attribute", filepath.Base(file), i+1, m[2])
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no stylesheets")
	}
}
