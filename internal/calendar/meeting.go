package calendar

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
)

// MeetingJoinURL reads conferencing metadata, never the editable description.
// Older caches may not have isOnlineMeeting; an explicit false from Graph is
// authoritative even if it still returns a legacy URL.
func MeetingJoinURL(raw string) string {
	var meeting struct {
		JoinURL string `json:"joinUrl"`
		URL     string `json:"url"`
		Online  *bool  `json:"isOnlineMeeting"`
	}
	if json.Unmarshal([]byte(raw), &meeting) != nil || (meeting.Online != nil && !*meeting.Online) {
		return ""
	}
	if value := SafeMeetingURL(meeting.JoinURL); value != "" {
		return value
	}
	return SafeMeetingURL(meeting.URL)
}

func SafeMeetingURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.ContainsAny(raw, "\r\n\t\x00") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return u.String()
}

// TeamsJoinURL accepts actual join routes, not Teams help, downloads, options,
// or lookalike domains. It is used only to recognize a Teams-specific fallback.
func TeamsJoinURL(raw string) string {
	value := SafeMeetingURL(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" {
		return ""
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "teams.live.com", "teams.microsoft.com", "teams.cloud.microsoft":
	default:
		return ""
	}
	for _, prefix := range []string{"/meet/", "/l/meetup-join/"} {
		if strings.HasPrefix(parsed.Path, prefix) && strings.Trim(parsed.Path[len(prefix):], "/") != "" {
			return value
		}
	}
	return ""
}

var teamsDescriptionURL = regexp.MustCompile(`https://[^\s<>"']+`)

// TeamsJoinURLFromDescription handles personal Outlook events whose generated
// Teams link reaches the body before Graph exposes conferencing metadata.
func TeamsJoinURLFromDescription(value string) string {
	if !strings.Contains(strings.ToLower(value), "teams.") {
		return ""
	}
	doc, err := xhtml.Parse(strings.NewReader(value))
	if err == nil {
		var find func(*xhtml.Node) string
		find = func(node *xhtml.Node) string {
			if node.Type == xhtml.ElementNode && (node.Namespace != "" || node.Data == "head" || node.Data == "script" || node.Data == "style" || node.Data == "template") {
				return ""
			}
			if node.Type == xhtml.ElementNode && node.Data == "a" {
				for _, attr := range node.Attr {
					if attr.Key == "href" {
						if link := TeamsJoinURL(attr.Val); link != "" {
							return link
						}
					}
				}
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if link := find(child); link != "" {
					return link
				}
			}
			return ""
		}
		if link := find(doc); link != "" {
			return link
		}
	}
	if !descriptionMarkup.MatchString(value) {
		for _, candidate := range teamsDescriptionURL.FindAllString(html.UnescapeString(value), -1) {
			if link := TeamsJoinURL(strings.TrimRight(candidate, ").,;")); link != "" {
				return link
			}
		}
	}
	return ""
}
