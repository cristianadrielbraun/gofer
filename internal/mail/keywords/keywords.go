// Package keywords holds the rules for message keywords, the free-form tags
// that IMAP and JMAP servers store on messages and that Gofer shows as
// labels. Both protocols share one keyword namespace on the server.
package keywords

import (
	"fmt"
	"strings"
)

// Validate checks that keyword can be stored as a label: not a system flag or
// status keyword, and limited to characters every server accepts.
func Validate(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return "", fmt.Errorf("label is required")
	}
	if strings.HasPrefix(keyword, "\\") {
		return "", fmt.Errorf("label cannot be a system flag")
	}
	if IsStatus(keyword) {
		return "", fmt.Errorf("label %q is a status keyword", keyword)
	}
	for _, r := range keyword {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '$' {
			continue
		}
		return "", fmt.Errorf("label %q is not a portable keyword", keyword)
	}
	return keyword, nil
}

// IsStatus reports whether keyword is a system flag or a keyword that marks
// a message's status rather than labelling it. JMAP spells IMAP's \Seen,
// \Flagged, \Draft and \Answered flags as $seen, $flagged, $draft and
// $answered. Fastmail's own undocumented markers, $IsTrusted and the
// $X-ME-* keywords, count as status too, so they don't show as labels.
func IsStatus(keyword string) bool {
	switch lower := strings.ToLower(strings.TrimSpace(keyword)); lower {
	case "\\seen", "\\answered", "\\flagged", "\\deleted", "\\draft", "\\recent",
		"$seen", "$answered", "$flagged", "$draft",
		"$junk", "$notjunk", "$nonjunk", "junk", "notjunk", "nonjunk", "non-junk",
		"$forwarded", "$mdnsent", "$phishing", "$istrusted":
		return true
	default:
		return strings.HasPrefix(keyword, "\\") || strings.HasPrefix(lower, "$x-me-")
	}
}
