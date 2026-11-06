package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func requestContactImportForm(ctx context.Context, f *userStorageFixture, owner string, fields map[string]string, wire string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("store_in", "local")
	for name, value := range fields {
		_ = form.WriteField(name, value)
	}
	file, err := form.CreateFormFile("vcard", "contacts.vcf")
	if err != nil {
		panic(err)
	}
	_, _ = io.WriteString(file, wire)
	_ = form.Close()
	r := httptest.NewRequest("POST", "/api/contacts/import", &body).WithContext(ctx)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Accept", "application/x-ndjson, application/json")
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	response := httptest.NewRecorder()
	f.http.ServeHTTP(response, r)
	return response
}

// contactImportLines decodes a streamed import: its progress lines and the
// result that ends it.
func contactImportLines(t *testing.T, response *httptest.ResponseRecorder) ([][2]int, map[string]any) {
	t.Helper()
	if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatal("import answer", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var progress [][2]int
	var result map[string]any
	for _, line := range strings.Split(strings.TrimSpace(response.Body.String()), "\n") {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatal(line, err)
		}
		if total, ok := message["total"].(float64); ok {
			done, _ := message["done"].(float64)
			progress = append(progress, [2]int{int(done), int(total)})
			continue
		}
		result = message
	}
	return progress, result
}

func TestUserContactImportPreviewAndExistingChoice(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), "alice", models.Contact{Name: "Kept name", Email: "same-import@example.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wire := contactImportOne + contactImportTwo

	preview := requestContactImportForm(t.Context(), f, "alice", map[string]string{"preview": "1"}, wire)
	var counts map[string]int
	if err := json.Unmarshal(preview.Body.Bytes(), &counts); err != nil || preview.Code != 200 || counts["total"] != 2 || counts["existing"] != 1 {
		t.Fatal("preview", preview.Code, preview.Body.String(), err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		profile, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", "second-import@example.com")
		if err == nil && profile != nil {
			t.Fatal("preview saved a contact")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	name := func(email string) string {
		var name string
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			profile, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", email)
			if err == nil && profile != nil {
				name = profile.DisplayName
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return name
	}

	progress, result := contactImportLines(t, requestContactImportForm(t.Context(), f, "alice", map[string]string{"existing": "skip"}, wire))
	if len(progress) != 3 || progress[0] != [2]int{0, 2} || progress[2] != [2]int{2, 2} {
		t.Fatal("skip progress", progress)
	}
	if result["added"] != float64(1) || result["updated"] != float64(0) || result["skipped"] != float64(1) {
		t.Fatal("skip result", result)
	}
	if name("same-import@example.com") != "Kept name" || name("second-import@example.com") != "Imported two" {
		t.Fatal("skip changed an existing contact", name("same-import@example.com"))
	}

	_, result = contactImportLines(t, requestContactImportForm(t.Context(), f, "alice", map[string]string{"existing": "update"}, wire))
	if result["added"] != float64(0) || result["updated"] != float64(2) || result["skipped"] != float64(0) {
		t.Fatal("update result", result)
	}
	if name("same-import@example.com") != "Imported one" {
		t.Fatal("update kept the old details", name("same-import@example.com"))
	}
}
