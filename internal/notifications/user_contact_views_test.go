package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactViewsHTTPPrivateProfilesAndFragments(t *testing.T) {
	f := newUserStorageFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		for _, path := range []string{"/contacts", "/contacts?partial=detail&contact=same-contact-id", "/contacts?partial=activity&contact=same-contact-id", "/contacts/items?selected=same-contact-id&start=0&limit=1", "/contacts?q=" + owner + "-contact", "/contacts?new=1", "/contacts?view=table"} {
			response := f.request(owner, "GET", path, "")
			other := "bob"
			if owner == "bob" {
				other = "alice"
			}
			if response.Code != 200 || strings.Contains(response.Body.String(), other+"-contact@example.com") || strings.Contains(response.Body.String(), other+"@example.com") {
				t.Fatal("private contact view", owner, path, response.Code, response.Body.String())
			}
			if !strings.Contains(path, "partial=activity") && !strings.Contains(response.Body.String(), owner+"-contact@example.com") {
				t.Fatal("owned contact omitted", owner, path)
			}
		}
		for _, target := range []string{"mail-list", "app-shell", "contacts-page"} {
			request := httptest.NewRequest("GET", "/contacts?contact=same-contact-id", nil)
			request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
			request.Header.Set("HX-Request", "true")
			request.Header.Set("HX-Target", target)
			response := httptest.NewRecorder()
			f.http.ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), owner+"-contact@example.com") {
				t.Fatal("HTMX contact view", owner, target, response.Code, response.Body.String())
			}
		}
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&count); err != nil || count != 0 {
		t.Fatal("contact view central fallback", count, err)
	}
}

type blockedContactViewWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedContactViewWriter) Write(body []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(body)
}

func TestUserContactViewHTTPRenderingReleasesDatabaseLease(t *testing.T) {
	f := newUserStorageFixture(t)
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(writer.release) }) }
	defer release()
	request := httptest.NewRequest("GET", "/contacts?contact=same-contact-id", nil).WithContext(t.Context())
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(writer, request); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("render not reached")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error {
		contact, err := db.GetContact(ctx, "bob", "same-contact-id")
		if err == nil && (contact == nil || contact.Email != "bob-contact@example.com") {
			t.Error("wrong owner during blocked render")
		}
		return err
	}); err != nil {
		t.Fatal("render retained sole cache slot", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not finish")
	}
	if writer.Code != 200 || !strings.Contains(writer.Body.String(), "alice-contact@example.com") || strings.Contains(writer.Body.String(), "bob-contact@example.com") {
		t.Fatal("copied component changed during eviction", writer.Code, writer.Body.String())
	}
}
