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
	"script", "iframe", "object", "embed", "form", "meta", "link",
}

func removeDangerousTags(html []byte) []byte {
	s := string(html)

	for _, tag := range dangerousElements {
		re := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>.*?</` + tag + `\s*>`)
		s = re.ReplaceAllString(s, "")
		reOpen := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*/?\s*>`)
		s = reOpen.ReplaceAllString(s, "")
	}

	reEventAttr := regexp.MustCompile(`(?i)\s+on\w+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]*)`)
	s = reEventAttr.ReplaceAllString(s, "")

	reJavascript := regexp.MustCompile(`(?i)href\s*=\s*["']javascript:[^"']*["']`)
	s = reJavascript.ReplaceAllString(s, `href="#"`)

	return []byte(s)
}

func blockRemoteImages(html []byte) []byte {
	s := string(html)

	reImgDouble := regexp.MustCompile(`(?i)(<img\b[^>]*?\s)src\s*=\s*"((https?://)[^"]*)"`)
	s = reImgDouble.ReplaceAllString(s, `${1}src="" data-remote-src="$2"`)

	reImgSingle := regexp.MustCompile(`(?i)(<img\b[^>]*?\s)src\s*=\s*'((https?://)[^']*)'`)
	s = reImgSingle.ReplaceAllString(s, `${1}src="" data-remote-src='$2'`)

	reCSSUrl := regexp.MustCompile(`(?i)url\s*\(\s*['"]?(https?://[^)'"]*?)['"]?\s*\)`)
	s = reCSSUrl.ReplaceAllString(s, `url("")`)

	return []byte(s)
}

func IsRemoteImagesBlocked(html string) bool {
	return strings.Contains(html, "data-remote-src")
}
