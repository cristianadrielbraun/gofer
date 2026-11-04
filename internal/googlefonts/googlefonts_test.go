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
