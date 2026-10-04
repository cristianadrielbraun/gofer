package views

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func TestCalendarInvitationStatesAcrossGridAndAgenda(t *testing.T) {
	week := NewCalendarWeekData(time.Now().UTC().AddDate(0, 0, 7))
	start := week.Weeks[0].Days[0].Date.Add(9 * time.Hour)
	end := start.AddDate(0, 0, 2)
	for _, state := range []struct{ raw, state, label string }{
		{"needsAction", "pending", "Awaiting response"},
		{"NEEDS-ACTION", "pending", "Awaiting response"},
		{"notResponded", "pending", "Awaiting response"},
		{"tentative", "tentative", "Maybe"},
		{"tentativelyAccepted", "tentative", "Maybe"},
		{"DECLINED", "declined", "Declined"},
		{"accepted", "accepted", "Accepted"},
		{"organizer", "", ""},
		{"", "", ""},
		{"unknown", "", ""},
	} {
		for _, allDay := range []bool{false, true} {
			for _, view := range []string{"month", "week"} {
				name := state.raw + "/" + view
				if allDay {
					name += "/all-day"
				}
				t.Run(name, func(t *testing.T) {
					event := CalendarEvent{ID: "invitation", Summary: "Planning", SourceColor: "#a3dcef", ResponseStatus: state.raw, Status: "tentative", AllDay: allDay, StartAt: &start, EndAt: &end, StartDate: start.Format("2006-01-02"), EndDate: end.Format("2006-01-02")}
					data := NewCalendarMonthData(start)
					if view == "week" {
						data = NewCalendarWeekData(start)
					}
					data.Events = []CalendarEvent{event}
					var out bytes.Buffer
					if err := CalendarPage(data, nil).Render(t.Context(), &out); err != nil {
						t.Fatal(err)
					}
					markup := out.String()
					if !strings.Contains(markup, "background-color: #a3dcef;") {
						t.Fatal("RSVP must not replace the source's calendar color")
					}
					if strings.Contains(markup, "calendar-event-response-indicator") != (state.state != "") {
						t.Fatal("known invitations need a status icon; ordinary events must not be marked pending")
					}
					doc, err := html.Parse(strings.NewReader(markup))
					if err != nil {
						t.Fatal(err)
					}
					grid, agenda := 0, 0
					var inspect func(*html.Node)
					inspect = func(node *html.Node) {
						attrs := map[string]string{}
						for _, attr := range node.Attr {
							attrs[attr.Key] = attr.Val
						}
						if _, trigger := attrs["data-calendar-event-trigger"]; trigger {
							if attrs["data-calendar-response-state"] != state.state || !strings.Contains(attrs["aria-label"], state.label) {
								t.Error("grid/agenda response styling and accessible label must agree")
							}
							if _, isAgenda := attrs["data-calendar-agenda-event"]; isAgenda {
								agenda++
							} else {
								grid++
							}
							if attrs["hx-get"] != "/api/calendar/events/invitation" {
								t.Error("every response state must keep the event clickable")
							}
						}
						for child := node.FirstChild; child != nil; child = child.NextSibling {
							inspect(child)
						}
					}
					inspect(doc)
					if grid < 2 || agenda != 1 {
						t.Fatalf("expected every multi-day segment and Upcoming card: grid=%d agenda=%d", grid, agenda)
					}
				})
			}
		}
	}
}

func TestCalendarInvitationIndicatorsShowReadableStatus(t *testing.T) {
	for _, test := range []struct{ response, compact, full string }{
		{"needsAction", "Reply", "Awaiting response"},
		{"tentative", "", "Maybe"},
		{"declined", "", "Declined"},
		{"accepted", "", "Accepted"},
	} {
		for _, compact := range []bool{false, true} {
			t.Run(test.response+"/"+map[bool]string{true: "grid", false: "agenda"}[compact], func(t *testing.T) {
				var out bytes.Buffer
				if err := CalendarEventResponseIndicator(CalendarEvent{ResponseStatus: test.response}, compact).Render(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				markup := out.String()
				if compact && (test.response == "tentative" || test.response == "declined") {
					if markup != "" {
						t.Fatal("Maybe and Declined grid pills use drawings and the footer legend, not badges")
					}
					return
				}
				label := test.full
				if compact {
					label = test.compact
				}
				if label != "" && !strings.Contains(markup, ">"+label+"</span>") {
					t.Fatalf("response must have a visible label, not only an icon or tooltip: %s", markup)
				}
				if compact && test.response != "accepted" && strings.Contains(markup, "<svg") {
					t.Fatal("compact text badges should not take extra title space with a redundant icon")
				}
				if !strings.Contains(markup, `aria-hidden="true"`) || strings.Contains(markup, "sr-only") {
					t.Fatal("badge text must be visible; the parent event already announces the full response")
				}
			})
		}
	}
}
