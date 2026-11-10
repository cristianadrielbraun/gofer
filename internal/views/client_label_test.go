package views

import (
	"context"
	"strings"
	"testing"
)

func TestClientLabelRecognizesCommonBrowsersAndBoundsFallback(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{name: "edge windows", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/140.0 Safari/537.36 Edg/140.0", want: "Microsoft Edge on Windows"},
		{name: "firefox mac", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15) Gecko/20100101 Firefox/141.0", want: "Firefox on macOS"},
		{name: "safari iphone", userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) Version/19.0 Mobile/15E148 Safari/604.1", want: "Safari on iPhone"},
		{name: "unknown", userAgent: "", want: "Unknown browser or device"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClientLabel(context.Background(), test.userAgent); got != test.want {
				t.Fatalf("ClientLabel() = %q, want %q", got, test.want)
			}
		})
	}
	if got := ClientLabel(context.Background(), strings.Repeat("界", 120)); len([]rune(got)) != 96 || !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded fallback = %q (%d runes)", got, len([]rune(got)))
	}
}
