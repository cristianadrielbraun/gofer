// Package retry contains the small pieces of retry policy shared by provider
// clients and durable workers.
package retry

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryAfter prevents a provider response from parking a durable job
// indefinitely because of a malformed or unreasonable value.
const MaxRetryAfter = 24 * time.Hour

// ParseRetryAfter parses both forms allowed by HTTP Retry-After: a number of
// seconds or an HTTP date. Past and malformed values are ignored. Values
// beyond MaxRetryAfter are clamped to that limit.
func ParseRetryAfter(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	now = now.UTC()
	if isDecimal(value) {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return now.Add(MaxRetryAfter), true
		}
		maxSeconds := int64(MaxRetryAfter / time.Second)
		if seconds > uint64(maxSeconds) {
			seconds = uint64(maxSeconds)
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	when = when.UTC()
	if when.Before(now) {
		return time.Time{}, false
	}
	if wait := when.Sub(now); wait > MaxRetryAfter {
		when = now.Add(MaxRetryAfter)
	}
	return when, true
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
