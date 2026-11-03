package views

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// themePreviewEmails are the made-up messages in the theme builder's preview.
// The second one is open in the reader.
func themePreviewEmails(ctx context.Context) []models.Email {
	t := func(key string) string { return i18n.T(ctx, "settings.theme.preview."+key) }
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
		mail("1", models.Contact{Name: "Robin Hale", Email: "robin@example.com", Initials: "RH"}, t("subject_1"), t("preview_1"), t("date_1"), t("date_1_full"), false),
		mail("2", models.Contact{Name: "Ana Ortiz", Email: "ana@example.org", Initials: "AO"}, t("subject_2"), t("preview_2"), t("date_2"), t("date_2_full"), true),
		mail("3", models.Contact{Name: "Sam Whitfield", Email: "sam@example.net", Initials: "SW"}, t("subject_3"), t("preview_3"), t("date_3"), t("date_3_full"), false),
	}
	emails[0].Labels = []models.Label{{Name: t("label"), Color: "bg-sky-500/10 text-sky-700 dark:text-sky-300"}}
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

// secondarySuggestionName names a secondary color suggestion in the page's
// language.
func secondarySuggestionName(ctx context.Context, name string) string {
	return i18n.T(ctx, "settings.theme.suggestion."+strings.ReplaceAll(strings.ToLower(name), " ", "_"))
}
