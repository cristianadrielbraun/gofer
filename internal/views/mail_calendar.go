package views

import (
	"context"
	"encoding/json"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

type MailCalendarEventData struct {
	Kind, Summary, Date, Time, Location, Organizer, OrganizerName, Status, JoinURL, EventID, Note string
	Response                                                                                      *CalendarResponseData
	Invitation                                                                                    *MailCalendarInvitation
}

// MailCalendarInvitation offers to add an invitation to a CalDAV calendar of
// the invited account, which doesn't add invitations by itself, or to update
// or remove the event a later email is for.
type MailCalendarInvitation struct {
	Label, Note, UID, SourceID string
}

func mailCalendarResponseMessage(ctx context.Context, status string) string {
	switch status {
	case "accepted", "tentative", "declined":
		return i18n.T(ctx, "calendar.mail_response."+status)
	default:
		return ""
	}
}

func mailCalendarResponseEditQuery(editing bool) string {
	if editing {
		return "&edit=1"
	}
	return ""
}

func mailCalendarInvitationValues(invitation MailCalendarInvitation) string {
	raw, _ := json.Marshal(map[string]string{"uid": invitation.UID, "source_id": invitation.SourceID})
	return string(raw)
}
