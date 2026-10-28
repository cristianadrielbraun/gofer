package message

import (
	"bufio"
	"bytes"
	"net/textproto"
	"regexp"
	"strings"
)

var msgIDPattern = regexp.MustCompile(`<([^<>]+)>`)

func NormalizeMessageID(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "<") && strings.Contains(value, ">") {
		value = strings.TrimSpace(value[1:strings.Index(value, ">")])
	}
	value = strings.Trim(value, " \t<>")
	if value == "" || strings.ContainsAny(value, " <>") || !strings.Contains(value, "@") {
		return ""
	}

	at := strings.LastIndex(value, "@")
	if at > 1 && strings.HasPrefix(value, "\"") && strings.HasSuffix(value[:at], "\"") {
		local := strings.Trim(value[:at], "\"")
		local = strings.ReplaceAll(local, `\\`, `\`)
		local = strings.ReplaceAll(local, `\"`, `"`)
		value = local + value[at:]
	}
	return value
}

func ParseMessageIDs(header string) []string {
	var ids []string
	seen := make(map[string]bool)
	for _, match := range msgIDPattern.FindAllStringSubmatch(header, -1) {
		id := NormalizeMessageID(match[1])
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(ids) == 0 {
		id := NormalizeMessageID(header)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func ThreadReferences(references, inReplyTo string) []string {
	ids := ParseMessageIDs(references)
	if replyIDs := ParseMessageIDs(inReplyTo); len(replyIDs) > 0 {
		replyID := replyIDs[0]
		if len(ids) == 0 || ids[len(ids)-1] != replyID {
			seen := false
			for _, id := range ids {
				if id == replyID {
					seen = true
					break
				}
			}
			if !seen {
				ids = append(ids, replyID)
			}
		}
	}
	return ids
}

func FormatReferences(parentReferences, parentMessageID string) string {
	ids := ThreadReferences(parentReferences, "")
	parentID := NormalizeMessageID(parentMessageID)
	if parentID != "" {
		seen := false
		for _, id := range ids {
			if id == parentID {
				seen = true
				break
			}
		}
		if !seen {
			ids = append(ids, parentID)
		}
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "<"+id+">")
	}
	return strings.Join(parts, " ")
}

func ParseThreadHeaders(raw []byte) (inReplyTo string, references string) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		return "", ""
	}
	if ids := ParseMessageIDs(header.Get("In-Reply-To")); len(ids) > 0 {
		inReplyTo = ids[0]
	}
	references = header.Get("References")
	return inReplyTo, references
}

// ParseListID reads the List-Id header from raw header lines.
func ParseListID(raw []byte) string {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && len(header) == 0 {
		return ""
	}
	return CleanListID(header.Get("List-Id"))
}

// CleanListID decodes a List-Id header value and puts it on one line, the
// way rules match it.
func CleanListID(value string) string {
	return collapseSpaces(DecodeHeader(strings.TrimSpace(value)))
}

func BaseSubject(subject string) string {
	s := collapseSpaces(subject)
	for {
		prev := s
		s = strings.TrimSpace(s)
		for strings.HasSuffix(strings.ToLower(s), "(fwd)") {
			s = strings.TrimSpace(s[:len(s)-5])
		}
		lower := strings.ToLower(s)
		if strings.HasPrefix(lower, "[fwd:") && strings.HasSuffix(s, "]") {
			s = strings.TrimSpace(s[5 : len(s)-1])
			continue
		}
		for strings.HasPrefix(s, "[") {
			end := strings.Index(s, "]")
			if end < 0 || strings.TrimSpace(s[end+1:]) == "" {
				break
			}
			s = strings.TrimSpace(s[end+1:])
			lower = strings.ToLower(s)
		}
		if strings.HasPrefix(lower, "re:") {
			s = strings.TrimSpace(s[3:])
		} else if strings.HasPrefix(lower, "fw:") {
			s = strings.TrimSpace(s[3:])
		} else if strings.HasPrefix(lower, "fwd:") {
			s = strings.TrimSpace(s[4:])
		} else {
			idx := strings.Index(lower, ":")
			if idx > 2 && (strings.HasPrefix(lower, "re[") || strings.HasPrefix(lower, "fw[")) {
				s = strings.TrimSpace(s[idx+1:])
			}
		}
		if s == prev {
			break
		}
	}
	return strings.ToLower(collapseSpaces(s))
}

func IsReplyOrForwardSubject(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(s, "re:") || strings.HasPrefix(s, "fw:") || strings.HasPrefix(s, "fwd:") || strings.HasSuffix(s, "(fwd)") || strings.HasPrefix(s, "[fwd:") || strings.HasPrefix(s, "re[") || strings.HasPrefix(s, "fw[")
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
