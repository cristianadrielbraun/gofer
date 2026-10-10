package views

import (
	"context"
	"net/http"
)

// ListViewCookie is set by the browser while the screen is phone-sized. Phones only
// have the card layout for the mail and contact lists, whatever the saved setting.
const ListViewCookie = "gofer_list_view"

type cardsListViewKey struct{}

// WithListViewCookie marks the request's context when it comes from a phone-sized
// screen, so lists render as cards.
func WithListViewCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(ListViewCookie); err == nil && cookie.Value == "cards" {
			r = r.WithContext(context.WithValue(r.Context(), cardsListViewKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// ListViewSetting returns the list layout to render for a setting such as
// mail_list_view or contacts_list_view: cards on phones, otherwise the saved value.
func ListViewSetting(ctx context.Context, settings map[string]string, key string) string {
	if cards, _ := ctx.Value(cardsListViewKey{}).(bool); cards {
		return "cards"
	}
	return uiSettingGet(settings, key, "cards")
}
