package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestCalendarDescriptionRichEditorAndSafePreview(t *testing.T) {
	var output bytes.Buffer
	data := CalendarCreateData{DescriptionHTML: `<p><b>Agenda</b></p><p>Meeting ID: 123</p><a href="https://teams.example/123">Join</a><script>secret()</script><img src="https://tracker.example">`}
	if err := CalendarDescriptionEditor(data).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{`contenteditable="true"`, "Meeting ID: 123", `href="https://teams.example/123"`, `<p><b>Agenda</b></p>`, `data-calendar-rich-editor`, `name="description_html"`, `role="toolbar"`, `data-calendar-description-command="bold"`, `data-calendar-description-toggle`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, "<img") {
		t.Fatal("unsafe rich editor HTML")
	}
	if strings.Contains(got, `contenteditable="false"`) || strings.Contains(got, "Additional notes") {
		t.Fatal("original description is still restricted")
	}
	output.Reset()
	if err := CalendarEventDialog(CalendarEventDetails{Description: "Agenda", DescriptionHTML: data.DescriptionHTML}, nil).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "<p><b>Agenda</b></p>") || strings.Contains(got, "<script>") {
		t.Fatal("unsafe or flattened event preview")
	}
}
