package i18n

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
)

// ScriptPrefix is where each language's browser catalog is served, as
// ScriptPrefix + "es.js".
const ScriptPrefix = "/assets/i18n/"

// Browser scripts read only the "js." messages, with English filled in for
// any the language lacks.
const scriptKeyPrefix = "js."

type script struct {
	body    []byte
	version string
}

var scripts sync.Map // Locale -> script

// Script returns the browser catalog for locale: a script that sets
// window.GOFER_I18N, and its content version.
func Script(locale Locale) ([]byte, string) {
	if cached, ok := scripts.Load(locale); ok {
		s := cached.(script)
		return s.body, s.version
	}
	messages := map[string]string{}
	for key, message := range catalogs[English] {
		if strings.HasPrefix(key, scriptKeyPrefix) {
			messages[key] = message
		}
	}
	for key, message := range catalogs[locale] {
		if strings.HasPrefix(key, scriptKeyPrefix) && message != "" {
			messages[key] = message
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
	scripts.Store(locale, s)
	return s.body, s.version
}

// ScriptURL returns the versioned URL of the context language's browser
// catalog, which browsers may keep until it changes.
func ScriptURL(ctx context.Context) string {
	locale := FromContext(ctx)
	_, version := Script(locale)
	return ScriptPrefix + string(locale) + ".js?v=" + version
}
