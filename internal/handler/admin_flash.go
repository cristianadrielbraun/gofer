package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

// An admin flash is the result of a change, shown as a toast on the page the
// change redirects to. It travels in a short-lived cookie rather than in the
// address, so it stays out of history, bookmarks and referrers, and shows
// once. The cookie is signed with a key that lives only as long as the
// process: a result older than a restart isn't worth showing.
const (
	adminFlashCookie   = "gofer_admin_flash"
	adminFlashPath     = "/admin"
	adminFlashLifetime = time.Minute
	// A notice is one sentence; the bound keeps the cookie small.
	adminFlashMaxMessage = 600
)

var adminFlashKey = sync.OnceValue(func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("admin flash key: " + err.Error())
	}
	return key
})

type adminFlash struct {
	Variant string `json:"v"`
	Message string `json:"m"`
	Expires int64  `json:"e"`
}

// setAdminFlashSuccess and setAdminFlashError leave a result for the next
// admin page. Call them before writing the response.
func (h *Handler) setAdminFlashSuccess(w http.ResponseWriter, message string) {
	h.setAdminFlash(w, views.AdminToastSuccess, message)
}

func (h *Handler) setAdminFlashError(w http.ResponseWriter, message string) {
	h.setAdminFlash(w, views.AdminToastError, message)
}

func (h *Handler) setAdminFlash(w http.ResponseWriter, variant, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if len(message) > adminFlashMaxMessage {
		message = message[:adminFlashMaxMessage]
	}
	payload, err := json.Marshal(adminFlash{Variant: variant, Message: message, Expires: time.Now().Add(adminFlashLifetime).Unix()})
	if err != nil {
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     adminFlashCookie,
		Value:    encoded + "." + adminFlashSignature(encoded),
		Path:     adminFlashPath,
		MaxAge:   int(adminFlashLifetime / time.Second),
		HttpOnly: true,
		Secure:   h.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// takeAdminFlash returns the result left for this page, if any, and clears
// it so a reload doesn't show it again. A cookie that doesn't verify, or has
// expired, is cleared and ignored.
func (h *Handler) takeAdminFlash(w http.ResponseWriter, r *http.Request) []views.AdminToast {
	cookie, err := r.Cookie(adminFlashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminFlashCookie, Value: "", Path: adminFlashPath, MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	flash, ok := readAdminFlash(cookie.Value, time.Now())
	if !ok {
		return nil
	}
	return []views.AdminToast{{Variant: flash.Variant, Message: flash.Message}}
}

func readAdminFlash(value string, now time.Time) (adminFlash, bool) {
	encoded, signature, found := strings.Cut(value, ".")
	if !found || !hmac.Equal([]byte(signature), []byte(adminFlashSignature(encoded))) {
		return adminFlash{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return adminFlash{}, false
	}
	var flash adminFlash
	if err := json.Unmarshal(payload, &flash); err != nil || now.Unix() > flash.Expires || flash.Message == "" {
		return adminFlash{}, false
	}
	if flash.Variant != views.AdminToastSuccess && flash.Variant != views.AdminToastError {
		return adminFlash{}, false
	}
	return flash, true
}

func adminFlashSignature(encoded string) string {
	mac := hmac.New(sha256.New, adminFlashKey())
	mac.Write([]byte(adminFlashCookie + "\x00" + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// secureCookies reports whether cookies need the Secure flag: when Gofer is
// served over HTTPS.
func (h *Handler) secureCookies() bool {
	return h.auth != nil && h.auth.Config().SecureCookies
}

// adminErrorToasts shows a refusal the page renders itself as an error toast.
func adminErrorToasts(message string) []views.AdminToast {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	return []views.AdminToast{{Variant: views.AdminToastError, Message: message}}
}
