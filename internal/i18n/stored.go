package i18n

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Some texts are saved in English when written and shown later, such as the
// reason a background job paused. Stored writes such a text in ctx's
// language when it is one of the messages under prefix, matched against
// their English: "Stopped after …: {error}" matches with the error kept as
// it came. Any other text, such as one written by a mail server, is
// returned as it is.
func Stored(ctx context.Context, prefix, text string) string {
	if text == "" || FromContext(ctx) == English {
		return text
	}
	for _, pattern := range storedPatterns(prefix) {
		match := pattern.re.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		args := make([]any, 0, 2*len(pattern.names))
		for i, name := range pattern.names {
			args = append(args, name, match[i+1])
		}
		return T(ctx, pattern.key, args...)
	}
	return text
}

type storedPattern struct {
	key   string
	re    *regexp.Regexp
	names []string
}

var (
	storedMu    sync.Mutex
	storedCache = map[string][]storedPattern{}
	placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)
)

func storedPatterns(prefix string) []storedPattern {
	storedMu.Lock()
	defer storedMu.Unlock()
	if patterns, ok := storedCache[prefix]; ok {
		return patterns
	}
	var patterns []storedPattern
	for key, english := range catalogs[English] {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var names []string
		var expr strings.Builder
		expr.WriteString("^")
		last := 0
		for _, loc := range placeholder.FindAllStringSubmatchIndex(english, -1) {
			expr.WriteString(regexp.QuoteMeta(english[last:loc[0]]))
			expr.WriteString("(.*?)")
			names = append(names, english[loc[2]:loc[3]])
			last = loc[1]
		}
		expr.WriteString(regexp.QuoteMeta(english[last:]))
		expr.WriteString("$")
		patterns = append(patterns, storedPattern{key, regexp.MustCompile("(?s)" + expr.String()), names})
	}
	// Messages with more fixed text first, so a bare "{error}" never wins
	// over a fuller match.
	sort.Slice(patterns, func(i, j int) bool {
		li, lj := len(patterns[i].re.String())-4*len(patterns[i].names), len(patterns[j].re.String())-4*len(patterns[j].names)
		if li != lj {
			return li > lj
		}
		return patterns[i].key < patterns[j].key
	})
	storedCache[prefix] = patterns
	return patterns
}
