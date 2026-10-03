package views

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	htmlnode "golang.org/x/net/html"
)

func TestCalendarEventDetailsDateAndTimeZone(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 24, 23, 30, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	for _, test := range []struct {
		name, date, zone string
		event            CalendarEvent
	}{
		{"single all-day", "Saturday, October 24, 2026", "", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-25"}},
		{"all-day exclusive end across DST", "Sat, Oct 24, 2026 – Mon, Oct 26, 2026", "", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-27"}},
		{"timed across DST", "Sunday, October 25, 2026", "Europe/Prague (CEST → CET)", CalendarEvent{StartAt: &start, EndAt: &end}},
		{"instant", "Sunday, October 25, 2026", "Europe/Prague (CEST)", CalendarEvent{StartAt: &start, EndAt: &start}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := calendarEventDetailsDate(test.event, prague); got != test.date {
				t.Fatalf("date = %q, want %q", got, test.date)
			}
			if got := calendarEventDetailsTimeZone(test.event, prague); got != test.zone {
				t.Fatalf("timezone = %q, want %q", got, test.zone)
			}
		})
	}
	crossDayEnd := start.Add(24 * time.Hour)
	if got := calendarEventDetailsDate(CalendarEvent{StartAt: &start, EndAt: &crossDayEnd}, prague); got != "Sun, Oct 25, 2026 – Mon, Oct 26, 2026" {
		t.Fatalf("cross-day date = %q", got)
	}
}

func TestCalendarEventDialogRendersReadOnlyDetails(t *testing.T) {
	start := time.Date(2026, time.October, 3, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	details := CalendarEventDetails{
		Event:       CalendarEvent{ID: "event-id", Summary: "Planning <script>alert(1)</script>", SourceName: "Work", SourceColor: "#4285f4", Location: "Room 2", StartAt: &start, EndAt: &end},
		Description: "First line\n" + strings.Repeat("Complete description. ", 25) + "Last line",
		Organizer:   CalendarEventParticipant{Name: "Organizer", Email: "organizer@example.com"},
		Attendees:   []CalendarEventParticipant{{Name: "Guest", Email: "guest@example.com", ResponseStatus: "accepted", Optional: true}},
	}
	var output bytes.Buffer
	if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		`id="calendar-event-details-dialog"`, `aria-labelledby="calendar-event-details-title"`,
		`Planning &lt;script&gt;alert(1)&lt;/script&gt;`, `Saturday, October 3, 2026`, `09:00–10:00`,
		`Work`, `Room 2`, `organizer@example.com`, `guest@example.com`, `Accepted`, `Optional`, `Last line`,
		`overflow-y-auto`, `w-[calc(100vw_-_2rem)]`, `Read-only · Cached event`, `data-tui-dialog-close`,
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("dialog missing %q", expected)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "Save") || strings.Contains(html, "border-l-2") {
		t.Fatal("dialog includes unsafe HTML, editing actions, or a colored left border")
	}

	output.Reset()
	if err := CalendarEventDialog(CalendarEventDetails{Event: CalendarEvent{AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"}}, time.UTC).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	for _, unexpected := range []string{"Location", "Organizer", "Description", "Guests (", "UTC"} {
		if strings.Contains(output.String(), unexpected) {
			t.Errorf("empty detail still shows %q", unexpected)
		}
	}
	if !strings.Contains(output.String(), "Untitled event") || !strings.Contains(output.String(), "All day") {
		t.Fatal("missing untitled or all-day fallback")
	}
}

func TestCalendarEventDialogResponsiveColumnsAndScrolling(t *testing.T) {
	for _, test := range []struct {
		name                string
		description, guests bool
	}{
		{"invitation with long notes", true, true},
		{"description only", true, false},
		{"guests without description", false, true},
		{"compact event", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			details := CalendarEventDetails{Event: CalendarEvent{ID: "event"}, Response: &CalendarResponseData{EventID: "event", Ready: true}}
			if test.description {
				details.Description = strings.Repeat("Full meeting notes.\n", 100) + "https://example.com/" + strings.Repeat("long-link", 100)
			}
			if test.guests {
				for range 50 {
					details.Attendees = append(details.Attendees, CalendarEventParticipant{Email: "guest@example.com"})
				}
			}
			var output bytes.Buffer
			if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			markup := output.String()
			doc, err := htmlnode.Parse(strings.NewReader(markup))
			if err != nil {
				t.Fatal(err)
			}
			nodes := map[string]*htmlnode.Node{}
			var inspect func(*htmlnode.Node)
			inspect = func(node *htmlnode.Node) {
				for _, attr := range node.Attr {
					if attr.Key == "id" {
						nodes[attr.Val] = node
					} else if strings.HasPrefix(attr.Key, "data-calendar-event-") {
						nodes[attr.Key] = node
					}
				}
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					inspect(child)
				}
			}
			inspect(doc)
			split := test.description || test.guests
			if strings.Contains(markup, "md:max-w-5xl") != split || strings.Contains(markup, "md:grid-cols-2") != split {
				t.Error("only events with secondary content should use the wide, two-column desktop layout")
			}
			for _, want := range []string{"md:max-h-[min(44rem,calc(100dvh-4rem))]", "md:grid-rows-[minmax(0,1fr)]", "md:overflow-y-auto", "min-h-0 overflow-y-auto md:flex md:flex-col md:overflow-hidden"} {
				if !strings.Contains(markup, want) {
					t.Errorf("missing responsive height or scrolling constraint %q", want)
				}
			}
			body, columns := nodes["data-calendar-event-details-body"], nodes["data-calendar-event-columns"]
			response := nodes["calendar-event-response"]
			if body == nil || columns == nil || response == nil || columns.Parent != body || response.Parent != body {
				t.Fatal("response controls must remain outside the scrolling columns, within the mobile scroll body")
			}
			if test.description {
				scroll := nodes["data-calendar-event-description-scroll"]
				if scroll == nil || scroll.Parent.Parent != columns || !strings.Contains(markup, details.Description) {
					t.Error("full description must occupy its own column, without truncation")
				}
			}
			if test.guests {
				parent := columns
				if test.description {
					parent = nodes["data-calendar-event-overview"]
				}
				heading := nodes["calendar-event-guests-heading"]
				if heading == nil || heading.Parent.Parent != parent || strings.Count(markup, `id="calendar-event-guests-heading"`) != 1 || strings.Count(markup, "guest@example.com") != 50 {
					t.Error("all guests must render once, beside the description or in the otherwise unused column")
				}
			}
		})
	}
}

func TestCalendarEventsHaveSharedAccessibleDetailsTriggers(t *testing.T) {
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	start := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	month := NewCalendarMonthData(start)
	month.Events = []CalendarEvent{{ID: "event-id", Summary: "Planning", StartAt: &start, EndAt: &end}}
	var output bytes.Buffer
	if err := CalendarPage(month, nil).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	for _, attribute := range []string{`hx-get="/api/calendar/events/event-id"`, `hx-target="#app-pane-dialogs"`, `aria-haspopup="dialog"`, `data-calendar-event-trigger`} {
		if got := strings.Count(output.String(), attribute); got != 2 {
			t.Errorf("attribute %q count = %d, want grid and agenda triggers", attribute, got)
		}
	}
}

func TestCalendarEventEditAffordanceFollowsCapability(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		canEdit      bool
	}{
		{"supported", "", true},
		{"recurring read-only", "Recurring events must be edited in your calendar provider.", false},
		{"permissions read-only", "This calendar has read-only access.", false},
		{"escaped reason", "Unsupported <script>reason</script>", false},
		{"unspecified read-only", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			details := CalendarEventDetails{Event: CalendarEvent{ID: "event/with space?#"}, CanEdit: test.canEdit, EditUnavailableReason: test.reason}
			if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			markup := output.String()
			editButton := regexp.MustCompile(`<button[^>]*data-calendar-edit-trigger[^>]*>`).FindString(markup)
			if test.canEdit {
				for _, want := range []string{
					`data-calendar-create-trigger`, `data-calendar-edit-trigger`, `type="button"`,
					`hx-get="/api/calendar/events/event%2Fwith%20space%3F%23/edit"`, `hx-target="#app-pane-dialogs"`,
					`hx-swap="innerHTML"`, `hx-sync="#app-pane-dialogs:replace"`, `hx-disabled-elt="this"`,
					`aria-haspopup="dialog"`, `aria-controls="calendar-create-dialog"`,
				} {
					if !strings.Contains(editButton, want) {
						t.Errorf("edit button missing %q", want)
					}
				}
				if strings.Contains(markup, "Read-only") || strings.Contains(markup, "data-calendar-edit-unavailable") || strings.Contains(editButton, "data-tui-dialog-close") {
					t.Error("supported edit must load through the shared lifecycle without closing details early")
				}
			} else {
				if editButton != "" || strings.Contains(markup, "data-calendar-create-trigger") {
					t.Error("read-only events must not offer editing")
				}
				if !strings.Contains(markup, "data-calendar-edit-unavailable") || !strings.Contains(markup, "Read-only") {
					t.Error("read-only events must explain editing availability")
				}
				if test.reason == "" {
					if !strings.Contains(markup, "Editing is unavailable for this event.") {
						t.Error("missing read-only fallback")
					}
				} else if test.name != "escaped reason" && !strings.Contains(markup, test.reason) {
					t.Error("missing contextual read-only reason")
				}
				if strings.Contains(markup, "<script>") {
					t.Error("read-only reasons must be escaped")
				}
			}
		})
	}
}

func TestCalendarEventDeleteConfirmationUsesTemplUIAndHTMX(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		var output bytes.Buffer
		details := CalendarEventDetails{Event: CalendarEvent{ID: "event/with space?#", Summary: "Unsafe <script>title</script>"}, CanDelete: allowed, DeleteVersion: `"v1"`}
		if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		markup := output.String()
		if strings.Contains(markup, "data-calendar-delete-trigger") != allowed || strings.Contains(markup, "data-calendar-delete-form") != allowed {
			t.Fatal("delete action did not follow capability")
		}
		if !allowed {
			continue
		}
		for _, want := range []string{
			`data-tui-popover-root`, `data-tui-popover-trigger`, `data-tui-popover-content`,
			`role="dialog"`, `aria-labelledby="calendar-event-delete-title"`,
			`hx-delete="/api/calendar/events/event%2Fwith%20space%3F%23"`, `hx-params="version"`, `hx-swap="none"`, `hx-sync="this:drop"`,
			`name="version"`, `data-calendar-delete-cancel`, `data-calendar-delete-submit`, `data-calendar-delete-error`, `data-calendar-delete-spinner`,
			`from your calendar provider, not just Gofer`, `Unsafe &lt;script&gt;title&lt;/script&gt;`,
		} {
			if !strings.Contains(markup, want) {
				t.Errorf("missing %q", want)
			}
		}
		version := regexp.MustCompile(`<input[^>]*name="version"[^>]*value="([^"]*)"`).FindStringSubmatch(markup)
		if len(version) != 2 || html.UnescapeString(version[1]) != details.DeleteVersion {
			t.Fatal("confirmation lost the exact event version")
		}
		if strings.Contains(markup, "confirm(") || strings.Count(markup, "<dialog ") != 1 {
			t.Fatal("confirmation must not open a native browser prompt or replace the modal backdrop")
		}
	}
}

func TestCalendarEventSeriesDeleteConfirmationIsScopedAndInitiallyDisabled(t *testing.T) {
	for _, ready := range []bool{false, true} {
		details := CalendarEventDetails{Event: CalendarEvent{ID: "instance/with space", Summary: "Series <script>unsafe</script>"}, CanDelete: true, DeleteSeries: true}
		if ready {
			details.DeleteVersion = `"master-v1"`
			details.DeleteSeriesID = "remote-master"
		}
		var output bytes.Buffer
		if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		markup := output.String()
		for _, want := range []string{`Delete series`, `Delete the entire series?`, `all occurrences, including past and future events`, `hx-params="version,scope,series_id"`, `name="scope" value="series"`, `data-calendar-delete-load`, `/delete-series-confirmation`, `hx-target="#calendar-event-delete-confirmation-body"`, `data-tui-popover-root`} {
			if !strings.Contains(markup, want) {
				t.Errorf("series confirmation missing %q", want)
			}
		}
		button := regexp.MustCompile(`<button[^>]*data-calendar-delete-submit[^>]*>`).FindString(markup)
		if regexp.MustCompile(`\sdisabled(?:\s|=|>)`).MatchString(button) == ready {
			t.Fatal("delete readiness does not follow master version availability")
		}
		if strings.Count(markup, "<dialog ") != 1 || strings.Contains(markup, "<script>") {
			t.Fatal("series confirmation changed backdrop or rendered unescaped text")
		}
	}
}
