package views

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListViewSettingUsesCardsOnPhones(t *testing.T) {
	settings := map[string]string{"mail_list_view": "table"}
	var got []string
	handler := WithListViewCookie(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, ListViewSetting(r.Context(), settings, "mail_list_view"))
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	phone := httptest.NewRequest(http.MethodGet, "/", nil)
	phone.AddCookie(&http.Cookie{Name: ListViewCookie, Value: "cards"})
	handler.ServeHTTP(httptest.NewRecorder(), phone)

	if len(got) != 2 || got[0] != "table" || got[1] != "cards" {
		t.Fatalf("list views = %v, want the saved table on desktop and cards on a phone", got)
	}
	if view := ListViewSetting(phone.Context(), map[string]string{}, "contacts_list_view"); view != "cards" {
		t.Fatalf("missing setting = %q, want cards", view)
	}
}
