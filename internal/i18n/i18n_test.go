package i18n

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestLocalesStartWithEnglish(t *testing.T) {
	got := Locales()
	if len(got) < 4 || got[0] != English || !slices.Contains(got, Locale("es")) || !slices.Contains(got, Locale("pt-br")) || !slices.Contains(got, Locale("zh-cn")) {
		t.Fatalf("Locales() = %v, want English first and Spanish, Brazilian Portuguese and Simplified Chinese present", got)
	}
	if locale, ok := Parse("pt-BR"); !ok || locale != "pt-br" {
		t.Fatalf("Parse(pt-BR) = %q, %v", locale, ok)
	}
	if locale, ok := Parse("zh-CN"); !ok || locale != "zh-cn" {
		t.Fatalf("Parse(zh-CN) = %q, %v", locale, ok)
	}
}

func TestMatchPicksTheBestCatalog(t *testing.T) {
	cases := map[string]Locale{
		"":                         English,
		"es-AR,es;q=0.9,en;q=0.8":  "es",
		"es-419":                   "es",
		"fr-FR,fr;q=0.9":           English,
		"fr-FR,es;q=0.5,en;q=0.4":  "es",
		"en-GB,en;q=0.9,es;q=0.8":  English,
		"de-CH, de;q=0.9, *;q=0.5": English,
		"pt-BR,pt;q=0.9,en;q=0.8":  "pt-br",
		"pt":                       "pt-br",
		"pt-PT":                    "pt-br",
		"fr-FR,pt;q=0.5,en;q=0.4":  "pt-br",
		"zh-CN,zh;q=0.9,en;q=0.8":  "zh-cn",
		"zh":                       "zh-cn",
		"fr-FR,zh;q=0.5,en;q=0.4":  "zh-cn",
	}
	for header, want := range cases {
		if got := Match(header); got != want {
			t.Errorf("Match(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestFromRequestPrefersTheCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/login", nil)
	r.Header.Set("Accept-Language", "en-US")
	r.Header.Set("Cookie", CookieName+"=es")
	if got := FromRequest(r); got != "es" {
		t.Fatalf("cookie es: got %q", got)
	}
	r.Header.Set("Cookie", CookieName+"=xx")
	if got := FromRequest(r); got != English {
		t.Fatalf("unknown cookie: got %q, want the browser's English", got)
	}
}

func TestFromSettingLeavesAutoToTheBrowser(t *testing.T) {
	for _, value := range []string{"", "auto", "xx"} {
		if _, ok := FromSetting(value); ok {
			t.Errorf("FromSetting(%q) chose a language", value)
		}
	}
	if got, ok := FromSetting("es"); !ok || got != "es" {
		t.Errorf("FromSetting(es) = %q, %v", got, ok)
	}
}

func TestTranslateFallsBackAndFillsPlaceholders(t *testing.T) {
	saved := catalogs
	defer func() { catalogs = saved }()
	catalogs = map[Locale]map[string]string{
		English: {"greet": "Hello, {name}", "only.en": "English only", "msg.one": "{count} message", "msg.other": "{count} messages"},
		"es":    {"greet": "Hola, {name}", "msg.one": "{count} mensaje", "msg.other": "{count} mensajes"},
	}
	ctx := WithLocale(context.Background(), "es")
	if got := T(ctx, "greet", "name", "Ana"); got != "Hola, Ana" {
		t.Errorf("T greet = %q", got)
	}
	if got := T(ctx, "only.en"); got != "English only" {
		t.Errorf("missing Spanish message = %q, want the English one", got)
	}
	if got := T(ctx, "no.such.key"); got != "no.such.key" {
		t.Errorf("missing everywhere = %q, want the key", got)
	}
	if got := N(ctx, "msg", 1); got != "1 mensaje" {
		t.Errorf("N 1 = %q", got)
	}
	if got := N(ctx, "msg", 0); got != "0 mensajes" {
		t.Errorf("N 0 = %q", got)
	}
	if got := N(ctx, "msg", 1234, "count", "1.234"); got != "1.234 mensajes" {
		t.Errorf("N with a formatted count = %q", got)
	}
	if got := T(context.Background(), "greet", "name", "Ana"); got != "Hello, Ana" {
		t.Errorf("no locale in context = %q, want English", got)
	}
}

var placeholderPattern = regexp.MustCompile(`\{[a-zA-Z_]+\}`)

func placeholders(message string) []string {
	found := placeholderPattern.FindAllString(message, -1)
	sort.Strings(found)
	return slices.Compact(found)
}

// Every catalog has exactly English's messages, using only its placeholders,
// so a language can't silently fall behind.
func TestCatalogsMatchEnglish(t *testing.T) {
	english := catalogs[English]
	for _, locale := range Locales() {
		messages := catalogs[locale]
		if messages["language.native_name"] == "" {
			t.Errorf("%s: language.native_name is empty", locale)
		}
		for key, message := range messages {
			if strings.TrimSpace(message) == "" {
				t.Errorf("%s: %s is empty", locale, key)
			}
			if _, ok := english[key]; !ok {
				t.Errorf("%s: %s is not an English message", locale, key)
				continue
			}
			// A translation may leave a value out, as "el único mensaje" does
			// with {count}, but can't use one English doesn't fill in.
			if locale != English {
				for _, name := range placeholders(message) {
					if !slices.Contains(placeholders(english[key]), name) {
						t.Errorf("%s: %s uses %s, which English doesn't have", locale, key, name)
					}
				}
			}
			if base, ok := strings.CutSuffix(key, ".one"); ok && messages[base+".other"] == "" {
				t.Errorf("%s: %s has no .other form", locale, key)
			}
		}
		if locale == English {
			continue
		}
		for key := range english {
			if _, ok := messages[key]; !ok {
				t.Errorf("%s: %s is missing", locale, key)
			}
		}
	}
}

var (
	// Only whole literal keys: a key built at run time, such as
	// "settings.section."+name, can't be checked here.
	goKeyPattern = regexp.MustCompile(`i18n\.(T|N|Translate|TranslateCount)\(\s*[^,()]+,\s*"([^"]+)"\s*[,)]`)
	jsKeyPattern = regexp.MustCompile(`goferT(n)?\(\s*["']([^"']+)["']\s*[,)]`)
)

// Every literal key the code asks for exists in English.
func TestUsedKeysExist(t *testing.T) {
	root := filepath.Join("..", "..")
	english := catalogs[English]
	check := func(file, key string, counted bool) {
		if counted {
			for _, form := range []string{".one", ".other"} {
				if _, ok := english[key+form]; !ok {
					t.Errorf("%s: %s%s is not in en.json", file, key, form)
				}
			}
			return
		}
		if _, ok := english[key]; !ok {
			t.Errorf("%s: %s is not in en.json", file, key)
		}
	}
	for _, dir := range []string{"internal", "components", filepath.Join("assets", "js")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			name := entry.Name()
			var pattern *regexp.Regexp
			switch {
			// i18n.js defines the helpers; its examples aren't real keys.
			case strings.HasSuffix(name, "_templ.go"), strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, ".min.js"), name == "i18n.js":
				return nil
			case strings.HasSuffix(name, ".go"), strings.HasSuffix(name, ".templ"):
				pattern = goKeyPattern
			case strings.HasSuffix(name, ".js"):
				pattern = jsKeyPattern
			default:
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
				counted := match[1] == "N" || match[1] == "TranslateCount" || match[1] == "n"
				key := match[2]
				if pattern == jsKeyPattern && !forScripts(key) {
					t.Errorf("%s: browser key %s isn't sent to scripts (see scriptKeyPrefixes)", path, key)
				}
				check(path, key, counted)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestScriptHasOnlyBrowserMessagesWithEnglishFilledIn(t *testing.T) {
	saved := catalogs
	defer func() { catalogs = saved; scripts.Clear() }()
	scripts.Clear()
	catalogs = map[Locale]map[string]string{
		English: {"js.a": "A", "js.b": "B", "server": "S"},
		"es":    {"js.a": "Á"},
	}
	body, version := Script("es")
	got := string(body)
	if !strings.HasPrefix(got, "window.GOFER_I18N=") || !strings.Contains(got, `"js.a":"Á"`) || !strings.Contains(got, `"js.b":"B"`) || strings.Contains(got, "server") || !strings.Contains(got, `"locale":"es"`) {
		t.Fatalf("script = %s", got)
	}
	if len(version) != 12 {
		t.Fatalf("version = %q", version)
	}
}

func TestLanguageNames(t *testing.T) {
	en, es, zh := WithLocale(context.Background(), English), WithLocale(context.Background(), "es"), WithLocale(context.Background(), "zh-cn")
	cases := []struct{ got, want string }{
		{LanguageName(en, "de"), "German"},
		{LanguageName(en, "zh-CN"), "Chinese"},
		{LanguageName(es, "de"), "Alemán"},
		{LanguageName(es, "en"), "Inglés"},
		{LanguageName(es, "zh-CN"), "Chino"},
		{LanguageName(zh, "de"), "德语"},
		{LanguageName(zh, "zh-CN"), "中文"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestForUserPrefersTheLastPageThenTheSavedChoice(t *testing.T) {
	ctx := context.Background()
	SetUserPrefsLoader(func(_ context.Context, userID string) (string, string, string, bool) {
		if userID == "saved-es" {
			return "es", "dd/mm/yyyy", "24h", true
		}
		return "auto", "", "", true
	})
	t.Cleanup(func() { SetUserPrefsLoader(nil) })

	if got := FromContext(ForUser(ctx, "nobody")); got != English {
		t.Fatalf("automatic with no page seen: %q", got)
	}
	saved := ForUser(ctx, "saved-es")
	if got := FromContext(saved); got != "es" {
		t.Fatalf("saved Spanish: %q", got)
	}
	if df, tf := DatePrefs(saved); df != "dd/mm/yyyy" || tf != "24h" {
		t.Fatalf("saved date settings: %q %q", df, tf)
	}
	RememberUser("seen-user", "es", "", "")
	t.Cleanup(func() { seenUsers.Delete("seen-user") })
	if got := FromContext(ForUser(ctx, "seen-user")); got != "es" {
		t.Fatalf("last page's language: %q", got)
	}
}
