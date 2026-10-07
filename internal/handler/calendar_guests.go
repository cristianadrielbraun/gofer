package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/a-h/templ"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleCalendarGuestSuggestions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	query := r.URL.Query().Get("guests")
	if len(query) > 16384 {
		return
	}
	query = strings.TrimSpace(query[calendarGuestLastSeparator(query)+1:])
	if len(query) < 2 || len(query) > 255 {
		return
	}
	ctx := r.Context()
	h.renderMailboxView(w, r, &ctx, func(local *Handler) (templ.Component, error) {
		contacts, err := local.db.SearchContacts(ctx, local.userID(ctx), query, 8)
		if err != nil {
			return nil, err
		}
		return views.CalendarGuestSuggestions(contacts), nil
	})
}

// Ignore commas inside quoted display names when completing the last address.
func calendarGuestLastSeparator(value string) int {
	last, quoted, escaped := -1, false, false
	for i, ch := range value {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && quoted {
			escaped = true
			continue
		}
		if ch == '"' {
			quoted = !quoted
		}
		if ch == ',' && !quoted {
			last = i
		}
	}
	return last
}

// Decode the three cache representations without accepting rooms or groups.
func calendarMeetingGuests(raw json.RawMessage) ([]calendar.GuestDraft, error) {
	if !calendarUpdateHasDetails(raw) {
		return nil, nil
	}
	var people []struct {
		Email        string
		Name         string
		DisplayName  string
		Optional     bool
		Resource     bool
		Type         string
		Role         string
		EmailAddress struct{ Address, Name string }
	}
	if json.Unmarshal(raw, &people) != nil || len(people) > 51 {
		return nil, fmt.Errorf("this meeting's guests are unsupported")
	}
	var guests []calendar.GuestDraft
	seen := map[string]bool{}
	for _, person := range people {
		email, name := person.Email, person.DisplayName
		if name == "" {
			name = person.Name
		}
		if email == "" {
			email, name = person.EmailAddress.Address, person.EmailAddress.Name
		}
		email = strings.ToLower(strings.TrimSpace(email))
		if calendarReplyAddress("mailto:"+email) == "" || seen[email] || person.Resource || (person.Type != "" && person.Type != "required" && person.Type != "optional") || (person.Role != "" && person.Role != "REQ-PARTICIPANT" && person.Role != "OPT-PARTICIPANT" && person.Role != "CHAIR") {
			return nil, fmt.Errorf("this meeting's guests are unsupported")
		}
		seen[email] = true
		guests = append(guests, calendar.GuestDraft{Email: email, Name: name, Optional: person.Optional || person.Type == "optional" || person.Role == "OPT-PARTICIPANT"})
	}
	return guests, nil
}

func calendarMeetingGuestText(event storage.CalendarEvent) string {
	guests, _ := calendarMeetingGuests(json.RawMessage(event.AttendeesJSON))
	var values []string
	for _, guest := range guests {
		if !strings.EqualFold(guest.Email, event.OrganizerEmail) {
			values = append(values, (&mail.Address{Name: guest.Name, Address: guest.Email}).String())
		}
	}
	return strings.Join(values, ", ")
}

func calendarMeetingGuestsMatch(raw json.RawMessage, requested []calendar.GuestDraft, organizer string) bool {
	actual, err := calendarMeetingGuests(raw)
	if err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, guest := range actual {
		if !strings.EqualFold(guest.Email, organizer) {
			seen[strings.ToLower(guest.Email)] = true
		}
	}
	if len(seen) != len(requested) {
		return false
	}
	for _, guest := range requested {
		if !seen[strings.ToLower(guest.Email)] {
			return false
		}
	}
	return true
}

func calendarMeetingScopeRestriction(organizer string, attendees json.RawMessage, draft calendar.EventDraft, recurring bool) error {
	if recurring && (len(draft.Guests) > 0 || calendarUpdateHasDetails(attendees)) {
		return calendarUpdateUnsupported("Recurring meetings with guests are not supported yet.")
	}
	if len(draft.Guests) > 0 && organizer != "organizer" {
		return calendarUpdateUnsupported("Only the organizer can invite guests.")
	}
	return nil
}

func calendarMeetingCheckSelfGuest(draft calendar.EventDraft, organizer string) error {
	for _, guest := range draft.Guests {
		if strings.EqualFold(guest.Email, organizer) {
			return calendarUpdateUnsupported("You are already the organizer; add other people as guests.")
		}
	}
	return nil
}

func calendarCachedOrganizer(event storage.CalendarEvent) bool {
	if event.SourceProvider == storage.CalendarSourceProviderCalDAV {
		return calendarReplyAddress("mailto:"+event.AccountEmail) != "" && strings.EqualFold(event.AccountEmail, event.OrganizerEmail)
	}
	return event.ResponseStatus == "organizer"
}

func calendarStoredOrganizerStatus(event storage.CalendarEvent) string {
	if calendarCachedOrganizer(event) {
		return "organizer"
	}
	return event.ResponseStatus
}
func calendarGoogleOrganizerStatus(event googleCalendarEvent) string {
	if event.Organizer.Self != nil && *event.Organizer.Self {
		return "organizer"
	}
	return ""
}
func calendarOutlookOrganizerStatus(event outlookCalendarEvent) string {
	if event.IsOrganizer != nil && *event.IsOrganizer {
		return "organizer"
	}
	return ""
}

func calendarMeetingGoogleGuests(draft calendar.EventDraft, current json.RawMessage) []map[string]any {
	var old []map[string]any
	_ = json.Unmarshal(current, &old)
	guests := make([]map[string]any, 0, len(draft.Guests))
	for _, previous := range old {
		if email, _ := previous["email"].(string); draft.OrganizerEmail != "" && strings.EqualFold(email, draft.OrganizerEmail) {
			guests = append(guests, previous)
		}
	}
	for _, guest := range draft.Guests {
		value := map[string]any{"email": guest.Email, "displayName": guest.Name, "optional": guest.Optional}
		for _, previous := range old {
			if email, _ := previous["email"].(string); strings.EqualFold(email, guest.Email) {
				value = previous
				break
			}
		}
		guests = append(guests, value)
	}
	return guests
}

func calendarMeetingOutlookGuests(draft calendar.EventDraft, current json.RawMessage) []map[string]any {
	var old []map[string]any
	_ = json.Unmarshal(current, &old)
	guests := make([]map[string]any, 0, len(draft.Guests))
	for _, previous := range old {
		if address, ok := previous["emailAddress"].(map[string]any); ok {
			if email, _ := address["address"].(string); draft.OrganizerEmail != "" && strings.EqualFold(email, draft.OrganizerEmail) {
				guests = append(guests, previous)
			}
		}
	}
	for _, guest := range draft.Guests {
		kind := "required"
		if guest.Optional {
			kind = "optional"
		}
		value := map[string]any{"emailAddress": map[string]string{"address": guest.Email, "name": guest.Name}, "type": kind}
		for _, previous := range old {
			if address, ok := previous["emailAddress"].(map[string]any); ok {
				if email, _ := address["address"].(string); strings.EqualFold(email, guest.Email) {
					value = previous
					break
				}
			}
		}
		guests = append(guests, value)
	}
	return guests
}
