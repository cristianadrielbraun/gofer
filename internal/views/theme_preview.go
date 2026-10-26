package views

import "github.com/cristianadrielbraun/gofer/internal/models"

// themePreviewEmails are the made-up messages in the theme builder's preview.
// The second one is open in the reader.
func themePreviewEmails() []models.Email {
	me := models.Contact{Name: "Jamie Doe", Email: "jamie@example.com", Initials: "JD"}
	mail := func(id string, from models.Contact, subject, preview, date, dateFull string, read bool) models.Email {
		return models.Email{
			ID:       "theme-preview-" + id,
			From:     from,
			To:       []models.Contact{me},
			Subject:  subject,
			Preview:  preview,
			Date:     date,
			DateFull: dateFull,
			IsRead:   read,
		}
	}
	emails := []models.Email{
		mail("1", models.Contact{Name: "Robin Hale", Email: "robin@example.com", Initials: "RH"}, "Trip to the coast", "The train leaves at eight, so let's meet at the station a little before.", "9:41 AM", "Today, 9:41 AM", false),
		mail("2", models.Contact{Name: "Ana Ortiz", Email: "ana@example.org", Initials: "AO"}, "Garden plans", "I've drawn up the beds and marked where the tomatoes go.", "Yesterday", "Yesterday, 6:20 PM", true),
		mail("3", models.Contact{Name: "Sam Whitfield", Email: "sam@example.net", Initials: "SW"}, "Book club", "Next month's pick is the one with the lighthouse on the cover.", "Thu", "Thursday, 11:05 AM", false),
	}
	emails[0].Labels = []models.Label{{Name: "Travel", Color: "bg-sky-500/10 text-sky-700 dark:text-sky-300"}}
	emails[0].IsStarred = true
	emails[1].HasAttachment = true
	emails[1].AttachmentCount = 1
	return emails
}

// secondarySuggestion is a secondary color the theme builder offers, made
// from the primary color: its hue turned by Turn degrees, with chroma "same"
// as the primary (the first follows it), "match" for as colorful, "muted" or
// "neutral".
type secondarySuggestion struct {
	Name   string
	Turn   int
	Chroma string
}

var secondarySuggestions = []secondarySuggestion{
	{"Same as primary", 0, "same"},
	{"Analogous", -30, "match"},
	{"Analogous", 30, "match"},
	{"Complementary", 180, "match"},
	{"Split complementary", 150, "match"},
	{"Split complementary", -150, "match"},
	{"Triadic", 120, "match"},
	{"Triadic", -120, "match"},
	{"Muted primary", 0, "muted"},
	{"Neutral", 0, "neutral"},
}
