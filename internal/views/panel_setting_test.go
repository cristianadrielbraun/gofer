package views

import "testing"

func TestPanelSettingKeepsSectionsApart(t *testing.T) {
	settings := map[string]string{
		"sidebar_width":       "220",
		"mail_list_width":     "40%",
		"contacts_list_width": "30%",
	}
	cases := []struct {
		section, key, want string
	}{
		{"mail", "mail_list_width", "40%"},
		{"contacts", "mail_list_width", "30%"},
		{"calendar", "mail_list_width", "40%"},
		{"contacts", "sidebar_width", "220"},
		{"calendar", "sidebar_width", "220"},
	}
	for _, c := range cases {
		if got := PanelSetting(settings, c.section, c.key, "fallback"); got != c.want {
			t.Errorf("PanelSetting(%s, %s) = %q, want %q", c.section, c.key, got, c.want)
		}
	}
	settings["calendar_sidebar_width"] = "300"
	if got := PanelSetting(settings, "calendar", "sidebar_width", "256px"); got != "300" {
		t.Errorf("calendar sidebar = %q, want 300", got)
	}
	if got := PanelSetting(map[string]string{}, "contacts", "sidebar_width", "256px"); got != "256px" {
		t.Errorf("empty settings = %q, want 256px", got)
	}
}

func TestPaneLayoutIsSetPerSection(t *testing.T) {
	settings := map[string]string{"mail_pane_layout": "stacked", "calendar_pane_layout": "stacked"}
	for section, want := range map[string]string{"mail": "stacked", "contacts": "side", "calendar": "stacked"} {
		if got := PaneLayout(settings, section); got != want {
			t.Errorf("PaneLayout(%s) = %q, want %q", section, got, want)
		}
	}
}

// Only mail can do without its reading pane.
func TestPaneLayoutListIsMailOnly(t *testing.T) {
	settings := map[string]string{"mail_pane_layout": "list", "contacts_pane_layout": "list", "calendar_pane_layout": "list"}
	for section, want := range map[string]string{"mail": "list", "contacts": "side", "calendar": "side"} {
		if got := PaneLayout(settings, section); got != want {
			t.Errorf("PaneLayout(%s) = %q, want %q", section, got, want)
		}
	}
	if got := mailPaneLayoutSettingLabel("list"); got != "No reading pane" {
		t.Errorf("label = %q", got)
	}
}
