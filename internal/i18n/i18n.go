// Package i18n translates Gofer's interface. Each language is a flat JSON
// catalog in locales/, named after its language code. English is complete by
// definition; a key missing from another catalog falls back to English.
//
// Messages use named placeholders such as {account}. Counted messages have a
// ".one" and an ".other" form, picked by N.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/language"
)

// Locale is a language code with a catalog in locales/, such as "en".
type Locale string

const (
	// English is the source language and the fallback for missing messages.
	English Locale = "en"
	// CookieName holds a chosen language for pages shown before sign-in.
	CookieName = "gofer_lang"
	// PickCookieName marks a language picked on a page shown before
	// sign-in, which is then saved as the user's language once they sign in.
	PickCookieName = "gofer_lang_pick"
	// SettingKey is the UI setting that holds a user's language. An empty
	// value or "auto" follows the browser.
	SettingKey = "language"
)

//go:embed locales/*.json
var localeFiles embed.FS

var (
	catalogs = map[Locale]map[string]string{}
	locales  []Locale
	matcher  language.Matcher
)

func init() {
	entries, err := localeFiles.ReadDir("locales")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := localeFiles.ReadFile(path.Join("locales", name))
		if err != nil {
			panic(err)
		}
		messages := map[string]string{}
		if err := json.Unmarshal(data, &messages); err != nil {
			panic(fmt.Sprintf("i18n: locales/%s: %v", name, err))
		}
		locale := Locale(strings.TrimSuffix(name, ".json"))
		catalogs[locale] = messages
		locales = append(locales, locale)
	}
	if catalogs[English] == nil {
		panic("i18n: locales/en.json is missing")
	}
	// English first, the rest in code order.
	sort.Slice(locales, func(i, j int) bool {
		if locales[i] == English || locales[j] == English {
			return locales[i] == English
		}
		return locales[i] < locales[j]
	})
	tags := make([]language.Tag, len(locales))
	for i, locale := range locales {
		tags[i] = language.MustParse(string(locale))
	}
	matcher = language.NewMatcher(tags)
}

// Locales returns every language with a catalog, English first.
func Locales() []Locale {
	return append([]Locale(nil), locales...)
}

// Parse returns the locale for a code such as "es", or false when Gofer has
// no catalog for it.
func Parse(code string) (Locale, bool) {
	locale := Locale(strings.ToLower(strings.TrimSpace(code)))
	_, ok := catalogs[locale]
	return locale, ok
}

// Match picks the best catalog for preferences such as an Accept-Language
// header, falling back to English.
func Match(preferences ...string) Locale {
	tag, index := language.MatchStrings(matcher, preferences...)
	if index > 0 && index < len(locales) && strings.Contains(string(locales[index]), "-") {
		return locales[index]
	}
	base, _ := tag.Base()
	if locale, ok := Parse(base.String()); ok {
		return locale
	}
	return English
}

// FromRequest picks the language for a request that has no signed-in
// choice: the language cookie, then the browser's languages.
func FromRequest(r *http.Request) Locale {
	if cookie, err := r.Cookie(CookieName); err == nil {
		if locale, ok := Parse(cookie.Value); ok {
			return locale
		}
	}
	return Match(r.Header.Get("Accept-Language"))
}

// FromSetting returns the locale a saved language setting asks for, or false
// when it is empty, "auto" or unknown, so the browser's language applies.
func FromSetting(value string) (Locale, bool) {
	if value == "" || value == "auto" {
		return "", false
	}
	return Parse(value)
}

type contextKey struct{}

// WithLocale returns a context that renders in locale.
func WithLocale(ctx context.Context, locale Locale) context.Context {
	return context.WithValue(ctx, contextKey{}, locale)
}

// FromContext returns the context's locale, English when none was set.
func FromContext(ctx context.Context) Locale {
	if ctx != nil {
		if locale, ok := ctx.Value(contextKey{}).(Locale); ok {
			return locale
		}
	}
	return English
}

// Lang returns the context's language code for an HTML lang attribute.
func Lang(ctx context.Context) string {
	return string(FromContext(ctx))
}

// NativeName is a language's name in that language, such as "Español", for
// language pickers.
func NativeName(locale Locale) string {
	return lookup(locale, "language.native_name")
}

// T returns the message for key in the context's language. args are
// placeholder names and values in pairs: T(ctx, "mail.moved", "folder", name).
func T(ctx context.Context, key string, args ...any) string {
	return Translate(FromContext(ctx), key, args...)
}

// N returns the form of a counted message for count, from key+".one" or
// key+".other". count also fills the {count} placeholder, unless args give
// one.
func N(ctx context.Context, key string, count int, args ...any) string {
	return TranslateCount(FromContext(ctx), key, count, args...)
}

// Translate is T for a known locale, for work outside a request.
func Translate(locale Locale, key string, args ...any) string {
	return format(lookup(locale, key), args)
}

// TranslateCount is N for a known locale, for work outside a request.
func TranslateCount(locale Locale, key string, count int, args ...any) string {
	form := key + "." + pluralForm(locale, count)
	// The caller's own {count}, such as a formatted number, comes first and
	// wins; the plain count fills it in otherwise.
	return format(lookup(locale, form), append(append([]any{}, args...), "count", count))
}

func lookup(locale Locale, key string) string {
	if message, ok := catalogs[locale][key]; ok && message != "" {
		return message
	}
	if message, ok := catalogs[English][key]; ok {
		return message
	}
	// Showing the key makes a missing message easy to spot and to find.
	return key
}

// pluralForm returns the CLDR plural category for count. English and Spanish
// only tell one from other for whole numbers; Spanish's "many" applies only
// to exact millions, which read fine with "other".
func pluralForm(_ Locale, count int) string {
	if count == 1 {
		return "one"
	}
	return "other"
}

func format(message string, args []any) string {
	if len(args) < 2 || !strings.Contains(message, "{") {
		return message
	}
	pairs := make([]string, 0, len(args))
	for i := 0; i+1 < len(args); i += 2 {
		name, ok := args[i].(string)
		if !ok {
			continue
		}
		pairs = append(pairs, "{"+name+"}", fmt.Sprint(args[i+1]))
	}
	return strings.NewReplacer(pairs...).Replace(message)
}

// LanguageName names a language, such as "de" or "zh-CN", in the context's
// language: "German", "Alemán". Each name is a "language.name.<code>"
// message; a language without one shows its code.
func LanguageName(ctx context.Context, code string) string {
	key := "language.name." + strings.ToLower(strings.TrimSpace(code))
	if name := T(ctx, key); name != key {
		return name
	}
	if base, _, ok := strings.Cut(key, "-"); ok {
		if name := T(ctx, base); name != base {
			return name
		}
	}
	return code
}
