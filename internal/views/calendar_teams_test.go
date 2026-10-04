package views

import (
	"bytes"
	"strings"
	"testing"

	htmlnode "golang.org/x/net/html"
)

func TestCalendarTeamsSwitchStates(t *testing.T) {
	for _, state := range []string{"available", "available-default", "existing", "other-online", "unsupported", "checking", "recurring", "error"} {
		t.Run(state, func(t *testing.T) {
			var out bytes.Buffer
			if err := CalendarTeamsOption(CalendarTeamsData{State: state}).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			doc, err := htmlnode.Parse(strings.NewReader(out.String()))
			if err != nil {
				t.Fatal(err)
			}
			var attrs map[string]string
			var walk func(*htmlnode.Node)
			walk = func(n *htmlnode.Node) {
				if n.Data == "input" {
					attrs = map[string]string{}
					for _, a := range n.Attr {
						attrs[a.Key] = a.Val
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(doc)
			if attrs["role"] != "switch" || attrs["type"] != "checkbox" || attrs["id"] != "calendar-create-teams" {
				t.Fatal("not an accessible templUI switch")
			}
			_, disabled := attrs["disabled"]
			_, checked := attrs["checked"]
			available := state == "available" || state == "available-default"
			if disabled == available || checked != (state == "existing") {
				t.Fatalf("invalid switch state: %v", attrs)
			}
			if (state == "existing" || state == "other-online") && attrs["name"] != "" {
				t.Fatal("existing online meeting submits a toggle/off instruction")
			}
			if available && (attrs["name"] != "teams_meeting" || attrs["value"] != "true") {
				t.Fatal("missing opt-in field")
			}
			if strings.Contains(out.String(), "<p ") || strings.Contains(out.String(), "<p>") || strings.Contains(out.String(), "calendar-teams-help") || attrs["aria-describedby"] != "" {
				t.Fatal("Teams switch must not show conditional explanatory text or reference removed help")
			}
		})
	}
}

func TestCalendarTeamsUsesScopedHTMXAndCalendarProvider(t *testing.T) {
	data := CalendarCreateData{SourceID: "source", EventID: "event", TeamsState: "existing", Sources: []CalendarCreateSource{{ID: "source", Provider: "outlook", Writable: true, Authorized: true}}}
	var out bytes.Buffer
	if err := CalendarCreateDialog(data).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-calendar-source-provider="outlook"`, `hx-get="/api/calendar/teams-options"`, `hx-params="source_id,event_id"`, `hx-sync="this:replace"`, `data-calendar-teams-state="existing"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}
