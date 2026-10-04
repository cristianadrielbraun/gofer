package handler

import (
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	ical "github.com/emersion/go-ical"
)

func calendarDescriptionChanged(existing string, draft calendar.EventDraft) bool {
	if draft.DescriptionHTML != nil {
		return calendar.DescriptionHTML(existing) != *draft.DescriptionHTML
	}
	return draft.Description != calendarDescriptionText(existing)
}

func calendarDraftOutlookBody(draft calendar.EventDraft) outlookCalendarItemBody {
	if draft.DescriptionHTML != nil {
		return outlookCalendarItemBody{ContentType: "html", Content: *draft.DescriptionHTML}
	}
	return outlookCalendarItemBody{ContentType: "text", Content: draft.Description}
}

// Update the plain fallback and HTML alternate together, including removing an
// obsolete alternate on a legacy plain-text edit. Untouched descriptions retain
// the provider's original representation.
func calendarUpdateDescriptionProps(props, edited ical.Props, existing string, draft calendar.EventDraft) {
	if !calendarDescriptionChanged(existing, draft) {
		return
	}
	props.Set(edited.Get("DESCRIPTION"))
	delete(props, "X-ALT-DESC")
	if alternate := edited.Get("X-ALT-DESC"); alternate != nil {
		props.Set(alternate)
	}
}
