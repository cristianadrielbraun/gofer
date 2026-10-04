package calendar

import (
	"strings"
	"testing"
)

func TestDescriptionHTMLSanitizesAppDOM(t *testing.T) {
	raw := `<html><head><style>body{display:none}</style></head><body><p id="calendar-create-form" class="overlay" hx-get="/logout" onclick="alert(1)" style="color:white;position:fixed;font-weight:bold">Agenda</p><ul><li><em>Discuss</em></li></ul><a href="https://teams.example/meet?id=1&amp;key=2">Join</a><a href="java&#x09;script:alert(1)">bad</a><img src="https://tracker.example"><svg onload="alert(1)"><text>SVG</text></svg><script>secret()</script><input name="request_id"><iframe srcdoc="evil"></iframe></body></html>`
	got := SanitizeDescriptionHTML(raw)
	for _, bad := range []string{"<style", "<script", "secret()", "<svg", "<input", "<iframe", "<img", "tracker.example", "onclick", "hx-get", ` id=`, "position:", "color:", "javascript"} {
		if strings.Contains(got, bad) {
			t.Errorf("unsafe description contains %s: %s", bad, got)
		}
	}
	for _, want := range []string{`style="font-weight:bold"`, "<ul><li><em>Discuss</em></li></ul>", `href="https://teams.example/meet?id=1&amp;key=2"`, `rel="noopener noreferrer"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lost formatting/link %s: %s", want, got)
		}
	}
	if twice := SanitizeDescriptionHTML(got); twice != got {
		t.Errorf("sanitizer not idempotent: %s -> %s", got, twice)
	}
}

func TestDescriptionPlainFallbackAndNewlines(t *testing.T) {
	if got := DescriptionHTML("one\ntwo & things"); got != "one<br>two &amp; things" {
		t.Fatal(got)
	}
	if got := DescriptionPlainText(`<p>Agenda</p><ul><li>One</li><li>Two</li></ul><p><b>Join</b><br>Now</p>`); got != "Agenda\nOne\nTwo\nJoin\nNow" {
		t.Fatal(got)
	}
	if got := SanitizeDescriptionHTML(`<p>Safe</p><a href="data:text/html,evil">bad</a><a href="/api/calendar/events">local</a>`); strings.Contains(got, "href") {
		t.Fatal(got)
	}
}
