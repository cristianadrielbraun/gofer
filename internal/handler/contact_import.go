package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A vCard import first asks for a preview, which counts the file's contacts
// and how many of them are already in Gofer by email address. The import
// itself then updates those contacts or leaves them as they are, as the
// person chose, and streams its progress as JSON lines like a calendar
// import: {"done":n,"total":m} after each contact, then the result, or
// {"error":"…"} when it stops. Without a JSON lines Accept header it answers
// with the old redirect to the contacts page.

type contactImportRequest struct {
	contacts     []models.Contact
	preview      bool
	skipExisting bool
	streaming    bool
}

type contactImportPreview struct {
	Total    int `json:"total"`
	Existing int `json:"existing"`
}

type contactImportResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

func (r contactImportResult) done() int { return r.Added + r.Updated + r.Skipped }

// contactImportSave saves one contact, or skips it; it reports whether the
// contact was already in Gofer and whether it was saved.
type contactImportSave func(ctx context.Context, contact models.Contact, skipExisting bool) (existed, saved bool, err error)

// readContactImport reads the multipart vCard import form. It answers the
// request itself, and returns false, when the form can't be imported.
func readContactImport(w http.ResponseWriter, r *http.Request, targets func(string) []string) (contactImportRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, contactImportMaxBytes+(128<<10))
	if err := r.ParseMultipartForm(contactImportMaxBytes); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_vcard_import"), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("vcard")
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.missing_vcard_file"), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, contactImportMaxBytes+1))
	if err != nil || int64(len(data)) > contactImportMaxBytes {
		http.Error(w, i18n.T(r.Context(), "server.error.vcard_file_is_too_large"), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	contacts, err := parseVCardContacts(bytes.NewReader(data), targets(r.FormValue("save_targets")))
	if err != nil {
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	return contactImportRequest{
		contacts: contacts,
		preview:  r.FormValue("preview") == "1",
		// Without a choice, contacts already in Gofer are updated, as
		// imports always did.
		skipExisting: r.FormValue("existing") == "skip",
		streaming:    strings.Contains(r.Header.Get("Accept"), "application/x-ndjson"),
	}, true
}

// previewContactImport answers how many of the contacts are already in
// Gofer.
func previewContactImport(w http.ResponseWriter, r *http.Request, contacts []models.Contact, inUse func(email string) (bool, error)) {
	preview := contactImportPreview{Total: len(contacts)}
	seen := make(map[string]bool)
	for _, contact := range contacts {
		email := strings.ToLower(strings.TrimSpace(contact.Email))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		existing, err := inUse(email)
		if err != nil {
			http.Error(w, i18n.T(r.Context(), "server.error.failed_to_import_contacts"), http.StatusInternalServerError)
			return
		}
		if existing {
			preview.Existing++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(preview)
}

// runContactImport saves the contacts one by one through save, which run
// calls inside whatever work wrapper the storage mode needs.
// fail answers an import that stopped before it saved anything.
func runContactImport(w http.ResponseWriter, r *http.Request, request contactImportRequest, run func(func(context.Context) error) error, save contactImportSave, fail func(http.ResponseWriter, *http.Request, error)) {
	total := len(request.contacts)
	controller := http.NewResponseController(w)
	encoder := json.NewEncoder(w)
	started := false
	line := func(value any) {
		if !request.streaming {
			return
		}
		if !started {
			started = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusOK)
		}
		_ = encoder.Encode(value)
		_ = controller.Flush()
	}
	type progressLine struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	}
	var result contactImportResult
	line(progressLine{Total: total})
	err := run(func(ctx context.Context) error {
		for _, contact := range request.contacts {
			existed, saved, err := save(ctx, contact, request.skipExisting)
			if err != nil {
				return err
			}
			switch {
			case !saved:
				result.Skipped++
			case existed:
				result.Updated++
			default:
				result.Added++
			}
			line(progressLine{Done: result.done(), Total: total})
		}
		return nil
	})
	if err != nil {
		saved := result.Added + result.Updated
		if !started {
			if saved == 0 {
				fail(w, r, err)
				return
			}
			http.Error(w, contactImportStopped(r.Context(), saved), http.StatusServiceUnavailable)
			return
		}
		message := i18n.T(r.Context(), "server.error.failed_to_import_contacts")
		if saved > 0 {
			message = contactImportStopped(r.Context(), saved)
		}
		line(map[string]string{"error": message})
		return
	}
	if !started {
		http.Redirect(w, r, fmt.Sprintf("/contacts?imported=%d", result.Added+result.Updated), http.StatusSeeOther)
		return
	}
	line(result)
}

func contactImportStopped(ctx context.Context, saved int) string {
	noun := "contacts"
	if saved == 1 {
		noun = "contact"
	}
	return serverText(ctx, fmt.Sprintf("Import stopped after saving %d %s. The remaining contacts were not saved.", saved, noun))
}
