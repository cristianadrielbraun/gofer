package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestCalendarReplyStatusViews(t *testing.T) {
	for _, tc := range []struct {
		state, send, text string
		polling           bool
	}{
		{"pending", "pending", "Reply queued for sending", true},
		{"pending", "sent", "Reply sent. Updating your calendar", true},
		{"pending", "failed", "Reply not sent", false},
		{"pending", "ambiguous", "The email may have been sent", false},
		{"complete", "sent", "Reply sent and calendar updated", false},
		{"conflict", "sent", "calendar could not be updated safely", false},
	} {
		t.Run(tc.state+tc.send, func(t *testing.T) {
			var out bytes.Buffer
			if err := CalendarReplyStatus(CalendarReplyData{ID: "job", EventID: "event", Scope: "event", State: tc.state, SendStatus: tc.send}).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			if !strings.Contains(html, tc.text) || strings.Contains(html, `hx-trigger="every 3s"`) != tc.polling {
				t.Fatal("wrong delivery feedback", html)
			}
			if tc.send == "ambiguous" && (!strings.Contains(html, "Resend (may send twice)") || !strings.Contains(html, "I confirmed it was sent") || strings.Contains(html, "Cancel reply")) {
				t.Fatal("uncertain delivery lacks explicit safe recovery")
			}
			if tc.state == "conflict" && !strings.Contains(html, "Dismiss notice") {
				t.Fatal("calendar-save conflict cannot be acknowledged")
			}
		})
	}
}
