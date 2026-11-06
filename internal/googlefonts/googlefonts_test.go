package googlefonts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRewriteCSSKeepsFontsOnGofer(t *testing.T) {
	css := []byte(`@import url(https://evil.example/x.css);
/* latin */
@font-face {
  font-family: 'Inter';
  src: url(https://fonts.gstatic.com/s/inter/v20/UcCO3FwrK3iLTeHuS_nVMrMxCp50SjIw2boKoduKmMEVuLyfAZ9hiA.woff2) format('woff2');
  src: url(https://elsewhere.example/font.woff2) format('woff2');
}
`)
	got := string(RewriteCSS(css))
	if !strings.Contains(got, "url("+FilePrefix+"s/inter/v20/UcCO3FwrK3iLTeHuS_nVMrMxCp50SjIw2boKoduKmMEVuLyfAZ9hiA.woff2)") {
		t.Fatalf("font file not pointed at Gofer:\n%s", got)
	}
	if strings.Contains(got, "://") || strings.Contains(got, "@import") {
		t.Fatalf("stylesheet still reaches elsewhere:\n%s", got)
	}
}

func TestFileRefusesPathsOutsideTheLibrary(t *testing.T) {
	library := New(t.TempDir(), &http.Client{Transport: failTransport{}})
	for _, path := range []string{"../etc/passwd", "s/inter/v20/../../x.woff2", "s/inter/v20/a.ttf", "img/x.woff2", "s/Inter/v20/a.woff2"} {
		if _, err := library.File(context.Background(), path); !errors.Is(err, ErrBadPath) {
			t.Errorf("File(%q) error = %v, want ErrBadPath", path, err)
		}
	}
}

func TestStylesheetRefusesUnknownFamilies(t *testing.T) {
	library := New(t.TempDir(), &http.Client{Transport: metadataTransport{}})
	if _, err := library.Stylesheet(context.Background(), "Not A Font"); !errors.Is(err, ErrUnknownFamily) {
		t.Fatalf("Stylesheet error = %v, want ErrUnknownFamily", err)
	}
	families, err := library.Families(context.Background())
	if err != nil || len(families) != 2 || families[0].Name != "Inter" || families[1].Category != "Serif" {
		t.Fatalf("Families() = %+v, %v", families, err)
	}
}

func TestFamilyQueryAsksForInterfaceWeights(t *testing.T) {
	families, err := parseMetadata([]byte(metadataFixture))
	if err != nil {
		t.Fatal(err)
	}
	if got := families["Inter"].query(); got != "Inter:wght@400;500;700" {
		t.Fatalf("Inter query = %q", got)
	}
	if got := families["Lora"].query(); got != "Lora" {
		t.Fatalf("Lora query = %q", got)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{"Inter": true, "Noto Sans JP": true, "": false, " Inter": false, "A  B": false, `Inter"`: false, "Inter;}": false} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}

const metadataFixture = `)]}'
{"familyMetadataList":[
 {"family":"Inter","category":"Sans Serif","fonts":{"300":{},"400":{},"400i":{},"500":{},"700":{}}},
 {"family":"Lora","category":"Serif","fonts":{"300":{}}},
 {"family":"Bad\"Name","category":"Serif","fonts":{"400":{}}}
]}`

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in tests")
}

type metadataTransport struct{}

func (metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != metadataURL {
		return nil, errors.New("unexpected request " + r.URL.String())
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(metadataFixture)), Header: http.Header{}}, nil
}

func TestChoiceCSSOnlyWithTheLibrary(t *testing.T) {
	t.Setenv(EnvVar, "")
	if css := ChoiceCSS("Inter", ""); css != "" {
		t.Fatalf("fonts used without the library: %s", css)
	}
	t.Setenv(EnvVar, "true")
	if css := ChoiceCSS("", ""); css != "" {
		t.Fatalf("CSS with nothing chosen: %s", css)
	}
	css := ChoiceCSS("Inter", "")
	if !strings.HasPrefix(css, `@import url("`+CSSPath+`?family=Inter");`) {
		t.Fatalf("CSS does not start with the font import: %s", css)
	}
	if !strings.Contains(css, `:root:not([data-theme="express"]){`) {
		t.Fatalf("fonts not kept off Express: %s", css)
	}
	if !strings.Contains(css, `--font-sans:"Inter","Nunito Sans",`) || !strings.Contains(css, `--default-font-family:"Inter",`) || strings.Contains(css, "--font-serif") {
		t.Fatalf("font variables wrong: %s", css)
	}
	if css := ChoiceCSS(`Inter";}body{color:red`, "Playfair Display"); strings.Contains(css, "color:red") || !strings.Contains(css, `--font-serif:"Playfair Display",`) {
		t.Fatalf("unsafe name not dropped: %s", css)
	}
}

func TestPreviewDrawsOnlyTheNameUnderItsOwnFamily(t *testing.T) {
	var asked []string
	transport := previewTransport{asked: &asked}
	library := New(t.TempDir(), &http.Client{Transport: transport})
	css, err := library.Preview(context.Background(), "Lora")
	if err != nil {
		t.Fatal(err)
	}
	got := string(css)
	if !strings.Contains(got, "font-family: '"+PreviewPrefix+"Lora'") {
		t.Fatalf("preview not renamed:\n%s", got)
	}
	if !strings.Contains(got, "url("+FilePrefix+"l/abc_-1/f3b6/v40.woff2)") || strings.Contains(got, "://") {
		t.Fatalf("subset file not pointed at Gofer:\n%s", got)
	}
	// Lora has no regular weight, so the preview asks for the one it has.
	if len(asked) != 1 || !strings.Contains(asked[0], "family=Lora%3Awght%40300") || !strings.Contains(asked[0], "text=Lora") {
		t.Fatalf("asked Google for %v", asked)
	}
	if _, err := library.Preview(context.Background(), "Not A Font"); !errors.Is(err, ErrUnknownFamily) {
		t.Fatalf("Preview error = %v, want ErrUnknownFamily", err)
	}
}

func TestFileRefusesSubsetPathsOutsideTheLibrary(t *testing.T) {
	library := New(t.TempDir(), &http.Client{Transport: failTransport{}})
	for _, path := range []string{"l/abc/f3b6/v40.ttf", "l/abc/F3B6/v40.woff2", "l/a&b/f3b6/v40.woff2", "l/../f3b6/v40.woff2", "l/abc/f3b6/40.woff2"} {
		if _, err := library.File(context.Background(), path); !errors.Is(err, ErrBadPath) {
			t.Errorf("File(%q) error = %v, want ErrBadPath", path, err)
		}
	}
}

type previewTransport struct{ asked *[]string }

func (p previewTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := metadataFixture
	if r.URL.String() != metadataURL {
		*p.asked = append(*p.asked, r.URL.String())
		body = "@font-face {\n  font-family: 'Lora';\n  src: url(https://fonts.gstatic.com/l/font?kit=abc_-1&skey=f3b6&v=v40) format('woff2');\n}\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}
