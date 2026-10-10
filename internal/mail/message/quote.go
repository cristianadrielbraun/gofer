package message

import (
	"bytes"
	"strings"
	"unicode"

	xhtml "golang.org/x/net/html"
)

// QuoteStartMarker marks where a message's quoted history begins. The body
// document's quote script collapses everything from it to the end of the body.
const QuoteStartMarker = `<span data-gofer-quote-start></span>`

// QuoteSource is an earlier message of the same thread that a reply may quote.
type QuoteSource struct {
	Text  string
	Name  string
	Email string
}

const (
	quoteShingle      = 4   // words per compared run
	quoteStartRun     = 3   // matching runs needed where the quote begins
	quoteMinCoverage  = 0.7 // share of the remaining words that must match
	quoteMaxUnmatched = 8   // longest run of new words allowed inside a quote
	quoteHeaderWords  = 40  // longest line taken for an attribution or header line
	quoteHeaderLines  = 8   // most attribution or header lines above a quote
	quoteSourceWords  = 20000
	quoteOpening      = 5 // words at the top of a message a quote may start within
)

type quoteSegment struct {
	offset int // byte offset in the body where the segment starts
	first  int // index of its first word in the flattened word list
	words  []string
	text   strings.Builder
	rule   bool // an <hr>
}

// MarkThreadQuote finds where body starts quoting one of the earlier messages
// of its thread and inserts QuoteStartMarker there. It works on the words
// themselves, so it needs no knowledge of the client or language that wrote
// the quote. The attribution above the quote ("On … wrote:", or a "From: …
// Sent: …" block) is found by its shape: short lines with a colon, reaching
// up to the one that names the quoted message's sender.
//
// The body is left unchanged when no quote is found, when new text is written
// between quoted parts, or when nothing would remain above the quote.
func MarkThreadQuote(body []byte, sources []QuoteSource) []byte {
	if len(body) == 0 || len(sources) == 0 {
		return body
	}
	segments := quoteSegments(body)
	var words []string
	for _, segment := range segments {
		segment.first = len(words)
		words = append(words, segment.words...)
	}
	if len(words) < quoteShingle+quoteStartRun {
		return body
	}

	// A quote reproduces a message from its top, so it has to begin with one of
	// the earlier messages' opening words. Text that only matches further down,
	// such as a sender's signature and disclaimer repeated in every message they
	// send, never starts a quote.
	known := make(map[string]int)
	openings := make(map[string]int)
	for i, source := range sources {
		sourceWords := quoteWords(source.Text)
		if len(sourceWords) > quoteSourceWords {
			sourceWords = sourceWords[:quoteSourceWords]
		}
		if quoteSameOpening(words, sourceWords) {
			continue // a copy of this message sent again, not one it quotes
		}
		for j := 0; j+quoteShingle <= len(sourceWords); j++ {
			key := strings.Join(sourceWords[j:j+quoteShingle], " ")
			if _, ok := known[key]; !ok {
				known[key] = i
			}
			if _, ok := openings[key]; !ok && j < quoteOpening {
				openings[key] = i
			}
		}
	}
	runs := make([]int, len(words)) // source index of the run starting at each word, or -1
	covered := make([]bool, len(words))
	for i := range words {
		runs[i] = -1
		if i+quoteShingle > len(words) {
			continue
		}
		if source, ok := known[strings.Join(words[i:i+quoteShingle], " ")]; ok {
			runs[i] = source
			for j := i; j < i+quoteShingle; j++ {
				covered[j] = true
			}
		}
	}

	for index, segment := range segments {
		if len(segment.words) == 0 || segment.first == 0 || !quoteStartsAt(runs, segment.first) {
			continue
		}
		quoted, opens := openings[strings.Join(words[segment.first:segment.first+quoteShingle], " ")]
		if !opens || !quoteRunsToEnd(covered, segment.first) {
			continue
		}
		start := quoteAttributionStart(segments, index, sources[quoted])
		if segments[start].first == 0 {
			return body
		}
		offset := segments[start].offset
		marked := make([]byte, 0, len(body)+len(QuoteStartMarker))
		marked = append(marked, body[:offset]...)
		marked = append(marked, QuoteStartMarker...)
		return append(marked, body[offset:]...)
	}
	return body
}

// quoteSameOpening reports whether two messages open with the same words, as a
// message sent twice does. A reply's own text never opens like what it quotes.
func quoteSameOpening(a, b []string) bool {
	n := quoteShingle + quoteStartRun - 1
	if len(a) < n || len(b) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func quoteStartsAt(runs []int, first int) bool {
	for i := first; i < first+quoteStartRun; i++ {
		if i >= len(runs) || runs[i] < 0 {
			return false
		}
	}
	return true
}

// quoteRunsToEnd reports whether the words from first on read as quoted text:
// most of them match, and no long run of new words sits among them.
func quoteRunsToEnd(covered []bool, first int) bool {
	matched, unmatched := 0, 0
	for i := first; i < len(covered); i++ {
		if covered[i] {
			matched++
			unmatched = 0
			continue
		}
		unmatched++
		if unmatched >= quoteMaxUnmatched {
			return false
		}
	}
	return float64(matched) >= quoteMinCoverage*float64(len(covered)-first)
}

// quoteAttributionStart extends the quote at segments[index] up over the lines
// that introduce it. Those lines are short and contain a colon in any language
// ("wrote:", a time, "From:"), and one of them names the quoted message's
// sender. A rule or dashed separator just above them goes with them.
func quoteAttributionStart(segments []*quoteSegment, index int, source QuoteSource) int {
	start := index
	for i, lines := index-1, 0; i >= 0 && lines < quoteHeaderLines; i-- {
		segment := segments[i]
		if len(segment.words) == 0 && !segment.rule {
			continue
		}
		if segment.rule || len(segment.words) > quoteHeaderWords || !strings.ContainsAny(segment.text.String(), ":：") {
			break
		}
		lines++
		if quoteNamesSender(segment, source) {
			start = i
		}
	}
	for i := start - 1; i >= 0 && start != index; i-- {
		segment := segments[i]
		if len(segment.words) == 0 && !segment.rule {
			continue
		}
		if segment.rule || quoteSeparatorLine(segment.text.String()) {
			start = i
		}
		break
	}
	return start
}

func quoteNamesSender(segment *quoteSegment, source QuoteSource) bool {
	text := strings.ToLower(segment.text.String())
	if email := strings.ToLower(strings.TrimSpace(source.Email)); email != "" && strings.Contains(text, email) {
		return true
	}
	for _, name := range quoteWords(source.Name) {
		if len([]rune(name)) < 3 {
			continue
		}
		for _, word := range segment.words {
			if word == name {
				return true
			}
		}
	}
	return false
}

// quoteSeparatorLine reports whether a line is mostly a drawn rule, such as
// "-----Original Message-----" or a row of underscores.
func quoteSeparatorLine(text string) bool {
	marks, letters := 0, 0
	for _, r := range text {
		switch {
		case r == '-' || r == '_' || r == '=' || r == '*':
			marks++
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			letters++
		}
	}
	return marks >= 5 && letters <= 40
}

// quoteSegments splits body into its lines of text: runs between block tags
// and <br>, and the lines of preformatted text. Each starts at the outermost
// tag that opens it, so a marker placed there sits before that whole block.
func quoteSegments(body []byte) []*quoteSegment {
	var segments []*quoteSegment
	var current *quoteSegment
	z := xhtml.NewTokenizer(bytes.NewReader(body))
	offset, anchor, anchored := 0, 0, false
	skip, pre := 0, 0
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return segments
		}
		raw := z.Raw()
		start := offset
		offset += len(raw)
		switch tt {
		case xhtml.StartTagToken, xhtml.EndTagToken, xhtml.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "head", "title":
				if tt == xhtml.StartTagToken {
					skip++
				} else if tt == xhtml.EndTagToken && skip > 0 {
					skip--
				}
				continue
			case "pre":
				if tt == xhtml.StartTagToken {
					pre++
				} else if tt == xhtml.EndTagToken && pre > 0 {
					pre--
				}
			}
			if tag == "hr" {
				segments = append(segments, &quoteSegment{offset: start, rule: true})
				current, anchored = nil, false
				continue
			}
			if !isPreviewBlockElement(tag) {
				continue
			}
			current = nil
			if tt == xhtml.StartTagToken && tag != "br" {
				if !anchored {
					anchor, anchored = start, true
				}
			} else {
				anchor, anchored = offset, false
			}
		case xhtml.TextToken:
			if skip > 0 {
				continue
			}
			lines := []string{string(raw)}
			if pre > 0 {
				lines = strings.Split(string(raw), "\n")
			}
			lineStart := start
			for i, line := range lines {
				if i > 0 {
					current = nil
					anchor = lineStart
				}
				text := xhtml.UnescapeString(line)
				if strings.TrimSpace(text) != "" {
					if current == nil {
						current = &quoteSegment{offset: anchor}
						segments = append(segments, current)
						anchored = false
					}
					current.words = append(current.words, quoteWords(text)...)
					current.text.WriteString(text)
					current.text.WriteByte(' ')
				}
				lineStart += len(line) + 1
			}
		}
	}
}

// quoteWords splits text into lowercase words, dropping punctuation and the
// ">" marks of plain-text quoting.
func quoteWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}
