package views

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestCalendarResponseControls(t *testing.T) {
	for _, scope := range []string{"event", "occurrence", "series"} {
		for _, ready := range []bool{false, true} {
			data := CalendarResponseData{EventID: `event/<script>`, Version: `"<version>"`, Status: "accepted", Scope: scope, HasOccurrence: scope != "event", Ready: ready}
			var out bytes.Buffer
			if err := CalendarResponseForm(data).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			for _, required := range []string{`data-calendar-response-form`, `hx-post=`, `hx-swap="none"`, `hx-params="version,scope,response"`, `name="scope" value="` + scope + `"`, `data-calendar-response-progress role="status" aria-live="polite"`, `role="alert"`, `aria-current="true"`, `aria-current="false"`, "organizer"} {
				if !strings.Contains(html, required) {
					t.Errorf("missing %q in %s", required, scope)
				}
			}
			for _, forbidden := range []string{"<script>", "<select", "<option", "onclick=", "grid-cols-3"} {
				if strings.Contains(html, forbidden) {
					t.Errorf("unsafe/native markup: %s", forbidden)
				}
			}
			if ready == strings.Contains(html, `hx-trigger="load, click"`) {
				t.Fatal("legacy responses alone should require an initial read")
			}
			if ready && !strings.Contains(html, "Accepted") {
				t.Fatal("missing cached current answer")
			}
			if (scope != "event") != strings.Contains(html, "Entire series") {
				t.Fatal("wrong response scope choices")
			}
			for _, required := range []string{`data-tui-popover-trigger`, `data-tui-popover-content`, `role="menu"`, `aria-label="Respond to invitation"`, `data-calendar-response-trigger`, `aria-haspopup="menu"`} {
				if !strings.Contains(html, required) {
					t.Errorf("missing templUI response menu markup %q", required)
				}
			}
			trigger := regexp.MustCompile(`<button[^>]*data-calendar-response-trigger[^>]*>`).FindString(html)
			if !strings.Contains(trigger, `type="button"`) || regexp.MustCompile(`\sdisabled(?:\s|=|>)`).MatchString(trigger) == ready {
				t.Fatal("response trigger must not submit and must follow invitation readiness")
			}
			items := regexp.MustCompile(`<button[^>]*data-calendar-response-choice[^>]*>`).FindAllString(html, -1)
			if len(items) != 3 {
				t.Fatal("expected three response menu actions")
			}
			for index, item := range items {
				status := []string{"accepted", "tentative", "declined"}[index]
				if !strings.Contains(item, `name="response"`) || !strings.Contains(item, `value="`+status+`"`) {
					t.Errorf("response action lost its submit value %q", status)
				}
				if !strings.Contains(item, `type="submit"`) || !strings.Contains(item, `data-tui-dropdown-item`) || regexp.MustCompile(`\sdisabled(?:\s|=|>)`).MatchString(item) == ready {
					t.Error("response choices must remain templUI submit actions with readiness protection")
				}
			}
		}
	}
}
