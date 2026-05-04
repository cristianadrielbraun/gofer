package message

import (
	"regexp"
	"strings"
)

func SanitizeHTML(input []byte) []byte {
	if len(input) == 0 {
		return nil
	}

	sanitized := removeDangerousTags(input)
	sanitized = blockRemoteImages(sanitized)

	return sanitized
}

var dangerousElements = []string{
	"script", "style", "iframe", "object", "embed", "form", "meta", "link",
}

func removeDangerousTags(html []byte) []byte {
	s := string(html)

	for _, tag := range dangerousElements {
		reOpen := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>`)
		reClose := regexp.MustCompile(`(?is)</` + tag + `\s*>`)
		s = reOpen.ReplaceAllString(s, "")
		s = reClose.ReplaceAllString(s, "")
	}

	reEventAttr := regexp.MustCompile(`(?i)\s+on\w+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]*)`)
	s = reEventAttr.ReplaceAllString(s, "")

	reJavascript := regexp.MustCompile(`(?i)href\s*=\s*["']javascript:[^"']*["']`)
	s = reJavascript.ReplaceAllString(s, `href="#"`)

	return []byte(s)
}

func blockRemoteImages(html []byte) []byte {
	s := string(html)

	reImg := regexp.MustCompile(`(?i)(<img\b[^>]*?\s)src\s*=\s*(["'])(https?://[^"']*?)\2`)
	s = reImg.ReplaceAllString(s, `${1}src="" data-remote-src=$2$3$2`)

	reCSSUrl := regexp.MustCompile(`(?i)url\s*\(\s*(['"]?)(https?://[^)]*?)\1\s*\)`)
	s = reCSSUrl.ReplaceAllString(s, `url($1$1)`)

	return []byte(s)
}

func IsRemoteImagesBlocked(html string) bool {
	return strings.Contains(html, "data-remote-src")
}
