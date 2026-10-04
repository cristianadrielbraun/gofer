package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestCalendarEventIndependentJoinButton(t *testing.T) {
	for _, link := range []string{"https://teams.example/123?token=a&x=b", "", "javascript:alert(1)"} {
		var output bytes.Buffer
		details := CalendarEventDetails{JoinURL: link} // No description or edit access, including invited meetings.
		if err := CalendarEventDialog(details, nil).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		if strings.HasPrefix(link, "https:") {
			for _, want := range []string{`data-calendar-join-meeting`, `Join meeting`, `href="https://teams.example/123?token=a&amp;x=b"`, `target="_blank"`, `rel="noopener noreferrer"`} {
				if !strings.Contains(html, want) {
					t.Errorf("missing %s", want)
				}
			}
		} else if strings.Contains(html, "data-calendar-join-meeting") {
			t.Fatal("missing/unsafe meeting link rendered")
		}
	}
}
