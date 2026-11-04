package i18n

import "context"

// NoSubject is what Gofer saves as the subject of a message that has none.
const NoSubject = "(no subject)"

// Subject shows a message's subject in the context's language: the saved
// NoSubject is translated, and every real subject is kept as it is.
func Subject(ctx context.Context, subject string) string {
	if subject == NoSubject {
		return T(ctx, "mail.no_subject")
	}
	return subject
}

// CalendarOnlyBody is what Gofer saves as the body of a message that only
// carries a calendar file, so it still has text to show.
const CalendarOnlyBody = "Calendar file attached."

// Preview shows a message's preview text in the context's language: the
// saved CalendarOnlyBody is translated, and every real preview is kept.
func Preview(ctx context.Context, preview string) string {
	if preview == CalendarOnlyBody {
		return T(ctx, "mail.calendar_only_body")
	}
	return preview
}
