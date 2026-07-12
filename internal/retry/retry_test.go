package retry

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	date := now.Add(5 * time.Minute).Format(http.TimeFormat)
	for _, tc := range []struct {
		name  string
		value string
		want  time.Time
		ok    bool
	}{
		{name: "delta seconds", value: "120", want: now.Add(2 * time.Minute), ok: true},
		{name: "http date", value: date, want: now.Add(5 * time.Minute), ok: true},
		{name: "past date", value: now.Add(-time.Minute).Format(http.TimeFormat)},
		{name: "malformed", value: "later"},
		{name: "negative", value: "-1"},
		{name: "empty", value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tc.value, now)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("ParseRetryAfter(%q) = %s, %v; want %s, %v", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseRetryAfterClampsLargeValues(t *testing.T) {
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	for _, value := range []string{"999999999999999999999", now.Add(7 * 24 * time.Hour).Format(http.TimeFormat)} {
		got, ok := ParseRetryAfter(value, now)
		if !ok || !got.Equal(now.Add(MaxRetryAfter)) {
			t.Fatalf("ParseRetryAfter(%q) = %s, %v; want %s, true", value, got, ok, now.Add(MaxRetryAfter))
		}
	}
}
