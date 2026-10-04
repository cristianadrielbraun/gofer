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

func TestCalendarEventTeamsLabelAndResponsiveJoinButton(t *testing.T) {
	for _, link := range []string{"https://teams.live.com/meet/123", "https://teams.microsoft.com/l/meetup-join/123", "https://meet.google.com/abc-defg-hij"} {
		var output bytes.Buffer
		if err := CalendarEventDialog(CalendarEventDetails{JoinURL: link}, nil).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		for _, want := range []string{"data-calendar-meeting-section", "flex-col items-start gap-2 sm:flex-row sm:items-center sm:gap-3", "shrink-0", "data-calendar-join-meeting"} {
			if !strings.Contains(html, want) {
				t.Errorf("meeting section missing %q", want)
			}
		}
		teams := strings.Contains(link, "teams.")
		if strings.Contains(html, "This is a Microsoft Teams meeting") != teams {
			t.Errorf("Teams label incorrectly rendered for %s", link)
		}
	}
}
