package i18n

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/branding"
)

// ScriptPrefix is where each language's browser catalog is served, as
// ScriptPrefix + "es.js".
const ScriptPrefix = "/assets/i18n/"

// Browser scripts read only the "js." messages, plus the date and number
// words their formatter needs and language names, with English filled in for any the language
// lacks.
const scriptKeyPrefix = "js."

var scriptKeyPrefixes = []string{scriptKeyPrefix, "date.", "number.", "language.name."}

func forScripts(key string) bool {
	for _, prefix := range scriptKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

type script struct {
	body    []byte
	version string
}

// scriptKey tells catalogs apart by language and by the installation's name,
// which the messages carry.
type scriptKey struct {
	locale   Locale
	branding string
}

var scripts sync.Map // scriptKey -> script

// Script returns the browser catalog for locale: a script that sets
// window.GOFER_I18N, and its content version.
func Script(locale Locale) ([]byte, string) {
	key := scriptKey{locale, branding.Version()}
	if cached, ok := scripts.Load(key); ok {
		s := cached.(script)
		return s.body, s.version
	}
	messages := map[string]string{}
	for key, message := range catalogs[English] {
		if forScripts(key) {
			messages[key] = withAppName(message)
		}
	}
	for key, message := range catalogs[locale] {
		if forScripts(key) && message != "" {
			messages[key] = withAppName(message)
		}
	}
	data, err := json.Marshal(struct {
		Locale   Locale            `json:"locale"`
		Messages map[string]string `json:"messages"`
	}{locale, messages})
	if err != nil {
		panic(err)
	}
	body := append([]byte("window.GOFER_I18N="), data...)
	body = append(body, ";\n"...)
	sum := sha256.Sum256(body)
	s := script{body: body, version: hex.EncodeToString(sum[:])[:12]}
	scripts.Store(key, s)
	return s.body, s.version
}

// ScriptURL returns the versioned URL of the context language's browser
// catalog, which browsers may keep until it changes.
func ScriptURL(ctx context.Context) string {
	locale := FromContext(ctx)
	_, version := Script(locale)
	return ScriptPrefix + string(locale) + ".js?v=" + version
}
