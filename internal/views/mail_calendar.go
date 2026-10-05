package views

type MailCalendarEventData struct {
	Kind, Summary, Date, Time, Location, Organizer, OrganizerName, Status, JoinURL, EventID, Note string
	Response                                                                                      *CalendarResponseData
}

func mailCalendarResponseMessage(status string) string {
	switch status {
	case "accepted":
		return "You already accepted this meeting."
	case "tentative":
		return "You already replied \"Maybe\" to this meeting."
	case "declined":
		return "You already rejected this meeting."
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
