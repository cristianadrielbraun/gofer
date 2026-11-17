package views

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// Settings › Regional offers every date format and clock, each shown as a
// sample, with the saved choice selected.
func TestRegionalTabOffersDateAndTimeFormats(t *testing.T) {
	render := func(ctx context.Context, settings map[string]string) string {
		var out strings.Builder
		if err := SettingsRegionalTab(settings).Render(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	year := strconv.Itoa(time.Now().Year())

	html := render(i18n.WithLocale(context.Background(), i18n.English), map[string]string{"date_format": "dd/mm/yyyy", "time_format": "24h"})
	for _, want := range []string{
		`data-date-format-input`, `data-time-format-input`,
		"Automatic (Dec 31, " + year + ")", "31/12/" + year, year + "-12-31",
		"Automatic (9:30 PM)", "12-hour (9:30 PM)", "24-hour (21:30)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("English Regional tab is missing %q", want)
		}
	}
	for _, id := range []string{"dd/mm/yyyy", "24h"} {
		if !selectedItem(html, id) {
			t.Errorf("saved choice %s is not selected", id)
		}
	}

	spanish := render(i18n.WithLocale(context.Background(), i18n.Locale("es")), map[string]string{"date_format": "unknown"})
	for _, want := range []string{"Formato de fecha", "Formato de hora", "Automático (31 dic " + year + ")", "24 horas (21:30)"} {
		if !strings.Contains(spanish, want) {
			t.Errorf("Spanish Regional tab is missing %q", want)
		}
	}
	if selectedItem(html, "mm/dd/yyyy") || selectedItem(html, "12h") {
		t.Error("choices that weren't saved are selected")
	}
	if !selectedItem(spanish, "auto") {
		t.Error("an unknown saved date format should leave Automatic selected")
	}
}

// selectedItem reports whether the menu item with value id is marked selected.
func selectedItem(html, id string) bool {
	for _, part := range strings.Split(html, "data-tui-selectbox-value=\""+id+"\"")[1:] {
		end := strings.Index(part, ">")
		if end > 0 && strings.Contains(part[:end], `data-tui-selectbox-selected="true"`) {
			return true
		}
	}
	return false
}

// Settings › Regional offers the days a week can start on, Monday unless
// another was saved.
func TestRegionalTabOffersTheFirstDayOfTheWeek(t *testing.T) {
	render := func(ctx context.Context, settings map[string]string) string {
		var out strings.Builder
		if err := SettingsRegionalTab(settings).Render(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	english := i18n.WithLocale(context.Background(), i18n.English)
	html := render(english, nil)
	for _, want := range []string{"First day of the week", `data-week-start-input`, `name="week_start"`, ">Saturday<", ">Sunday<", ">Monday<"} {
		if !strings.Contains(strings.Join(strings.Fields(html), ""), strings.Join(strings.Fields(want), "")) {
			t.Errorf("Regional tab is missing %q", want)
		}
	}
	if !selectedItem(html, "monday") || selectedItem(html, "sunday") || selectedItem(html, "saturday") {
		t.Error("Monday should be selected when nothing was saved")
	}
	if html := render(english, map[string]string{"week_start": "sunday"}); !selectedItem(html, "sunday") || selectedItem(html, "monday") {
		t.Error("the saved Sunday is not the selected choice")
	}
	if html := render(english, map[string]string{"week_start": "friday"}); !selectedItem(html, "monday") {
		t.Error("a day Gofer doesn't offer should leave Monday selected")
	}
	spanish := render(i18n.WithLocale(context.Background(), i18n.Locale("es")), map[string]string{"week_start": "saturday"})
	for _, want := range []string{"Primer día de la semana", "Sábado", "Domingo", "Lunes"} {
		if !strings.Contains(spanish, want) {
			t.Errorf("Spanish Regional tab is missing %q", want)
		}
	}
}

// Date pickers and scripts start the week on the day the request carries.
func TestDatePickersAndScriptsFollowTheWeekStart(t *testing.T) {
	for setting, day := range map[string]string{"": "1", "sunday": "0", "saturday": "6"} {
		ctx := i18n.WithWeekStart(i18n.WithLocale(context.Background(), i18n.English), setting)
		for name, component := range map[string]templ.Component{
			"mail date filter":   MailFilterCalendarField("date_from", "filter-date-from", "From"),
			"scheduled send":     ComposeSchedulePopover(false, ""),
			"event date":         calendarCreateDateTimeLine("start", "Starts", "2026-10-02", "09:00", false),
			"repeat until":       CalendarRecurrenceFields(CalendarCreateData{}),
			"script date helper": I18nScripts(),
		} {
			var out strings.Builder
			if err := component.Render(ctx, &out); err != nil {
				t.Fatal(err)
			}
			attribute := `data-tui-calendar-start-of-week="` + day + `"`
			if name == "script date helper" {
				attribute = `data-week-start="` + day + `"`
			}
			if !strings.Contains(out.String(), attribute) {
				t.Errorf("%s with week_start %q is missing %s", name, setting, attribute)
			}
		}
	}
}
