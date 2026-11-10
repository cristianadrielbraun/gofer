package handler

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminFlashOf returns the result a response left for the next admin page.
func adminFlashOf(t *testing.T, response *httptest.ResponseRecorder) (adminFlash, bool) {
	t.Helper()
	var flash adminFlash
	found := false
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name != adminFlashCookie || cookie.MaxAge < 0 || cookie.Value == "" {
			continue
		}
		var ok bool
		flash, ok = readAdminFlash(cookie.Value, time.Now())
		if !ok {
			t.Fatalf("admin flash cookie %q doesn't verify", cookie.Value)
		}
		found = true
	}
	return flash, found
}

func TestAdminFlashCarriesOneResultToTheNextPage(t *testing.T) {
	h := &Handler{}
	set := httptest.NewRecorder()
	h.setAdminFlashSuccess(set, "User disabled.")
	flash, ok := adminFlashOf(t, set)
	if !ok || flash.Variant != views.AdminToastSuccess || flash.Message != "User disabled." {
		t.Fatalf("flash = %+v %v", flash, ok)
	}
	cookie := set.Result().Cookies()[0]
	if cookie.Path != "/admin" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 60 {
		t.Fatalf("flash cookie attributes = %+v", cookie)
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	request.AddCookie(cookie)
	take := httptest.NewRecorder()
	toasts := h.takeAdminFlash(take, request)
	if len(toasts) != 1 || toasts[0].Message != "User disabled." || toasts[0].Variant != views.AdminToastSuccess {
		t.Fatalf("toasts = %+v", toasts)
	}
	cleared := take.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != adminFlashCookie || cleared[0].MaxAge >= 0 {
		t.Fatalf("taking the flash didn't clear it: %+v", cleared)
	}
}

func TestAdminFlashRejectsForgedExpiredAndUnknownResults(t *testing.T) {
	h := &Handler{}
	set := httptest.NewRecorder()
	h.setAdminFlashError(set, "Refused.")
	value := set.Result().Cookies()[0].Value
	if _, ok := readAdminFlash(value, time.Now()); !ok {
		t.Fatal("a fresh flash didn't verify")
	}
	encoded, signature, _ := strings.Cut(value, ".")
	forged := strings.Replace(encoded, encoded[:4], "AAAA", 1) + "." + signature
	for name, candidate := range map[string]string{
		"forged":   forged,
		"unsigned": encoded,
		"empty":    "",
	} {
		if _, ok := readAdminFlash(candidate, time.Now()); ok {
			t.Fatalf("%s flash verified", name)
		}
	}
	if _, ok := readAdminFlash(value, time.Now().Add(2*adminFlashLifetime)); ok {
		t.Fatal("an expired flash verified")
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	request.AddCookie(&http.Cookie{Name: adminFlashCookie, Value: forged})
	if toasts := h.takeAdminFlash(httptest.NewRecorder(), request); toasts != nil {
		t.Fatalf("a forged flash became a toast: %+v", toasts)
	}
}

func TestAdminPagesShowTheFlashAsAToastOnce(t *testing.T) {
	h := &Handler{}
	set := httptest.NewRecorder()
	h.setAdminFlashSuccess(set, `Saved "everything" <now>.`)
	cookie := set.Result().Cookies()[0]

	request := httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	request.AddCookie(cookie)
	page := httptest.NewRecorder()
	h.renderAdminPage(page, request, http.StatusOK, views.AdminShellPage{Section: views.AdminSectionMailSecurity, Content: views.AdminNavigationLoading()})
	body := page.Body.String()
	if toasts := pageToasts(t, body); len(toasts) != 1 || toasts[0].Variant != views.AdminToastSuccess ||
		toasts[0].Message != `Saved "everything" <now>.` || toasts[0].Duration != "5000" {
		t.Fatalf("page didn't show the flash as a toast that fades: %+v", toasts)
	}
	if strings.Contains(body, "<now>") {
		t.Fatal("the flash reached the page unescaped")
	}

	// A refusal the page shows itself wins, and the flash is still cleared.
	request = httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	request.AddCookie(cookie)
	page = httptest.NewRecorder()
	h.renderAdminPage(page, request, http.StatusBadRequest, views.AdminShellPage{
		Section: views.AdminSectionMailSecurity, Content: views.AdminNavigationLoading(),
		Toasts: adminErrorToasts("Refused."),
	})
	body = page.Body.String()
	if toasts := pageToasts(t, body); len(toasts) != 1 || toasts[0].Variant != views.AdminToastError ||
		toasts[0].Message != "Refused." || toasts[0].Duration != "-1" {
		t.Fatalf("the page's own error didn't win, or fades: %+v", toasts)
	}
	if cleared := page.Result().Cookies(); len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("the flash wasn't cleared: %+v", cleared)
	}
}

// adminFlashFailed reports whether a response left an error for the next
// admin page.
func adminFlashFailed(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	flash, ok := adminFlashOf(t, response)
	return ok && flash.Variant == views.AdminToastError
}

// pageToastPattern finds templui toasts in an admin page: their timer, their
// variant and the message in their description.
var pageToastPattern = regexp.MustCompile(`data-tui-toast data-tui-toast-duration="(-?\d+)" data-position="[^"]*" data-variant="([a-z]+)"[\s\S]*?<p class="text-sm opacity-90 mt-1">([^<]*)</p>`)

type pageToast struct {
	views.AdminToast
	Duration string
}

// pageToasts returns the toasts an admin page shows.
func pageToasts(t *testing.T, body string) []pageToast {
	t.Helper()
	var toasts []pageToast
	for _, match := range pageToastPattern.FindAllStringSubmatch(body, -1) {
		toasts = append(toasts, pageToast{
			AdminToast: views.AdminToast{Variant: match[2], Message: html.UnescapeString(match[3])},
			Duration:   match[1],
		})
	}
	return toasts
}

// adminFlashMessage is the text a response left for the next admin page.
func adminFlashMessage(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	flash, _ := adminFlashOf(t, response)
	return flash.Message
}
