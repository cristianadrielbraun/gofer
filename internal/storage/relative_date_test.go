package storage

import (
	"context"
	"testing"
	"time"
)

func TestFormatRelativeDateWithTime(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, loc)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"today keeps only the time", time.Date(2026, 10, 21, 9, 5, 0, 0, loc), "9:05 AM"},
		{"yesterday", time.Date(2026, 10, 20, 18, 30, 0, 0, loc), "Yesterday, 6:30 PM"},
		{"this year", time.Date(2026, 3, 2, 7, 45, 0, 0, loc), "Mar 2, 7:45 AM"},
		{"earlier year", time.Date(2024, 12, 31, 23, 59, 0, 0, loc), "Dec 31, 2024, 11:59 PM"},
	}
	for _, tc := range cases {
		if got := formatRelativeDateWithTime(context.Background(), tc.at, now, loc); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
