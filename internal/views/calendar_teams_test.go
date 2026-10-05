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
				if n.Data == "input" && calendarTestAttr(n, "role") == "switch" {
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
			if strings.Contains(out.String(), "calendar-teams-help") || attrs["aria-describedby"] != "" {
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
	for _, want := range []string{`data-calendar-source-provider="outlook"`, `hx-get="/api/calendar/meeting-options"`, `hx-params="source_id,event_id"`, `hx-sync="this:replace"`, `data-calendar-teams-state="existing"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestCalendarGoogleMeetOption(t *testing.T) {
	for _, state := range []string{"available", "existing", "unsupported", "checking", "error", "recurring"} {
		var out bytes.Buffer
		if err := CalendarTeamsOption(CalendarTeamsData{State: state, Provider: "gmail"}).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		body := out.String()
		if !strings.Contains(body, "Google Meet meeting") || !strings.Contains(body, `id="calendar-create-google-meet"`) || strings.Contains(body, "teams_meeting") {
			t.Fatalf("incorrect Meet control: %s", body)
		}
		if state == "available" && !strings.Contains(body, `name="google_meet_meeting"`) {
			t.Fatal("missing opt-in flag")
		}
		if state == "existing" && (!strings.Contains(body, "checked") || strings.Contains(body, `name="google_meet_meeting"`)) {
			t.Fatal("existing Meet must be checked without submitting removal")
		}
	}
}

func TestCalendarGoogleMeetReadyLinkPreviewControls(t *testing.T) {
	for _, state := range []string{"available", "existing"} {
		var out bytes.Buffer
		if err := CalendarTeamsOption(CalendarTeamsData{Provider: "gmail", State: state, JoinURL: "https://meet.google.com/abc-defg-hij"}).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		if state == "available" {
			for _, want := range []string{"data-calendar-meet-preview", `name="google_meet_draft_id"`, `aria-live="polite"`, "Copy link", "retryCalendarMeetPreview"} {
				if !strings.Contains(html, want) {
					t.Errorf("preview missing %s", want)
				}
			}
		} else if !strings.Contains(html, `href="https://meet.google.com/abc-defg-hij"`) || strings.Contains(html, "data-calendar-meet-preview") {
			t.Fatal("existing Meet must display its link without allocating a preview")
		}
	}
}

func calendarTestAttr(n *htmlnode.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func TestCalendarTeamsPreviewOnlyReservesNewEvents(t *testing.T) {
	for _, eventID := range []string{"", "existing"} {
		var out bytes.Buffer
		if err := CalendarTeamsOption(CalendarTeamsData{Provider: "outlook", State: "available", EventID: eventID}).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		has := strings.Contains(html, `name="teams_draft_id"`)
		if has != (eventID == "") {
			t.Fatal("Teams preview eligibility does not match event scope")
		}
		if has {
			for _, want := range []string{`data-calendar-meeting-provider="teams_meeting"`, `data-calendar-meet-status`, `data-calendar-meet-url`, `Copy link`, `retryCalendarTeamsPreview`} {
				if !strings.Contains(html, want) {
					t.Fatalf("missing preview control %s", want)
				}
			}
		}
	}
}
