package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	ical "github.com/emersion/go-ical"
)

func TestCalendarRichDescriptionJSONProviders(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(map[bool]string{true: "Outlook", false: "Google"}[graph], func(t *testing.T) {
			draft := calendarProviderDraft(t, false)
			rich := `<p><strong>Agenda</strong></p><ul><li>Review</li></ul>`
			draft.DescriptionHTML, draft.Description = &rich, calendar.DescriptionPlainText(rich)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("bad create request")
					w.WriteHeader(400)
					return
				}
				if graph {
					body := payload["body"].(map[string]any)
					if body["contentType"] != "html" || body["content"] != rich {
						t.Error("Graph lost rich description")
					}
					payload["id"], payload["changeKey"] = "created", "v1"
				} else {
					if payload["description"] != rich {
						t.Error("Google lost rich description")
					}
					payload["etag"] = `"v1"`
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			var remote calendar.RemoteEvent
			var err error
			if graph {
				old := outlookGraphBaseURL
				outlookGraphBaseURL = server.URL
				defer func() { outlookGraphBaseURL = old }()
				remote, err = createOutlookCalendarEvent(t.Context(), "test-token", "work", draft)
			} else {
				old := googleCalendarAPIBaseURL
				googleCalendarAPIBaseURL = server.URL
				defer func() { googleCalendarAPIBaseURL = old }()
				remote, err = createGoogleCalendarEvent(t.Context(), "test-token", "work", draft)
			}
			if err != nil || remote.Description != rich {
				t.Fatal("provider HTML round trip", remote.Description, err)
			}
		})
	}
}

func TestCalendarRichDescriptionDraftAndCalDAVRoundTrip(t *testing.T) {
	values := calendarCreateForm()
	values.Set("description", "untrusted fallback")
	values.Set("description_html", `<p><strong>Agenda</strong></p><ul><li>One</li><li>Two</li></ul><a href="javascript:evil()">bad</a><script>secret()</script>`)
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if draft.DescriptionHTML == nil || strings.Contains(*draft.DescriptionHTML, "script") || strings.Contains(draft.Description, "untrusted") || !strings.Contains(draft.Description, "Agenda\nOne\nTwo") {
		t.Fatalf("draft: %#v", draft)
	}
	if body := calendarDraftOutlookBody(draft); body.ContentType != "html" || body.Content != *draft.DescriptionHTML {
		t.Fatal(body)
	}
	raw, err := calendarCreateICS(draft)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := calendarUpdateDecodeICS([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	props := decoded.Events()[0].Props
	if props.Get("X-ALT-DESC").Params.Get("FMTTYPE") != "text/html" {
		t.Fatal("missing HTML alternate")
	}
	plain, _ := props.Text("DESCRIPTION")
	if plain != draft.Description || strings.Contains(plain, "<strong>") {
		t.Fatal("plain fallback lost", plain)
	}
	normalized, err := normalizeCalDAVEvent(decoded.Events()[0], "https://example.com/event.ics", `"v1"`, nil)
	if err != nil || normalized.Description != *draft.DescriptionHTML {
		t.Fatal("HTML sync round trip", normalized.Description, err)
	}
	if !calendarUpdateMatchesDraft(normalized, draft) {
		t.Fatal("rich confirmation rejected")
	}
	if calendarDescriptionChanged(normalized.Description, draft) {
		t.Fatal("unchanged rich description would be rewritten")
	}
	values.Set("description_html", `<p><em>Agenda</em></p><ul><li>One</li><li>Two</li></ul>bad`)
	changed, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil || !calendarDescriptionChanged(normalized.Description, changed) {
		t.Fatal("format-only edit missed", err)
	}
	if calendarUpdateMatchesDraft(normalized, changed) {
		t.Fatal("lost formatting passed confirmation")
	}
	clearedProps := ical.Props{}
	clearedProps.SetText("DESCRIPTION", "")
	calendarUpdateDescriptionProps(props, clearedProps, normalized.Description, calendar.EventDraft{Description: ""})
	if props.Get("X-ALT-DESC") != nil {
		t.Fatal("clearing description kept stale rich body")
	}
	values.Set("description_html", "")
	cleared, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil || cleared.DescriptionHTML == nil || cleared.Description != "" {
		t.Fatal("clearing rich description", err)
	}
	values.Set("description_html", strings.Repeat("a", 65537))
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Fatal("oversized HTML accepted")
	}
}

func TestCalendarOutlookEmptyDescriptionDoesNotRestoreStalePreview(t *testing.T) {
	var remote outlookCalendarEvent
	if err := json.Unmarshal([]byte(`{"id":"event","body":{"contentType":"html","content":""},"bodyPreview":"Stale description","start":{"dateTime":"2026-10-02T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-02T10:00:00","timeZone":"UTC"}}`), &remote); err != nil {
		t.Fatal(err)
	}
	event, err := normalizeOutlookCalendarEvent(remote)
	if err != nil || event.Description != "" {
		t.Fatal("cleared description restored stale preview", event.Description, err)
	}
	remote.Body.ContentType = ""
	event, err = normalizeOutlookCalendarEvent(remote)
	if err != nil || event.Description != remote.BodyPreview {
		t.Fatal("missing body lost preview fallback", event.Description, err)
	}
}
