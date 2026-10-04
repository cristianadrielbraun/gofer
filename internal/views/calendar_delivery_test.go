package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestCalendarDeliveryFragment(t *testing.T) {
	data := CalendarDeliveryData{EventID: "event-id", HasEmailDelivery: true, Rows: []CalendarDeliveryRow{
		{ID: "send-id", Label: "Invitation/update failed", Recipient: "<script>bad</script>@example.com", State: "failed", CanRetry: true, Note: "Not sent."},
		{ID: "ambiguous", Label: "Delivery uncertain", State: "ambiguous", Note: "It may already have been sent."},
		{Label: "Guest reply could not be verified", State: "ignored", Note: "The guest’s RSVP is unchanged."},
	}, Error: "<script>error</script>"}
	var out bytes.Buffer
	if err := CalendarEventGuests(CalendarEventDetails{Event: CalendarEvent{ID: data.EventID}, Delivery: &data}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`id="calendar-event-delivery"`, `every 5s`, `hx-swap="outerHTML"`, `hx-sync="#calendar-event-guests:replace"`, `/delivery/send-id/retry`, `hx-disabled-elt="this"`, `max-h-40`, `aria-live="polite"`, `Retrying…`, `RSVP is unchanged`, `&lt;script&gt;error&lt;/script&gt;`, `not that the guest accepted it`} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "/delivery/ambiguous/retry") {
		t.Fatal("unsafe content or ambiguous resend")
	}
}
