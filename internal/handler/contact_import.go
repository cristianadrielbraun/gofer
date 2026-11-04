package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A vCard import stores its contacts in one place, store_in: Gofer Local or
// an account or address book. With gofer_sync on they also get Gofer Sync,
// replicated to the places in sync_with. A contact counts as already there
// when one with its email address has a copy in the store place; existing
// says whether those are updated from the file or left as they are.
//
// A preview first counts the file's contacts and the ones already there.
// The import then streams its progress as JSON lines like a calendar
// import: {"phase":"saving","done":n,"total":m} as it saves the contacts in
// Gofer, then {"phase":"writing",…} while it writes them to the accounts,
// then the result, or {"error":"…"} when it stops. Without a JSON lines
// Accept header it answers with the old redirect to the contacts page.

type contactImportRequest struct {
	contacts     []models.Contact
	preview      bool
	skipExisting bool
	streaming    bool
	// storeIn is the one place the contacts live; syncWith the other places
	// Gofer Sync replicates them to, when sync is on.
	storeIn  string
	sync     bool
	syncWith []string
}

// targets are the places an imported contact is saved with, before the
// places an existing contact already has are added.
func (r contactImportRequest) targets() []string {
	targets := []string{r.storeIn}
	if r.sync {
		for _, target := range r.syncWith {
			if !slices.Contains(targets, target) {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

type contactImportPreview struct {
	Total    int `json:"total"`
	Existing int `json:"existing"`
}

type contactImportResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	// The writes to accounts: done, failed once and to be tried again in the
	// background, or failed for good, with why one of them failed.
	Written  int    `json:"written"`
	Retrying int    `json:"retrying"`
	Failed   int    `json:"failed"`
	Reason   string `json:"reason,omitempty"`
}

func (r contactImportResult) done() int { return r.Added + r.Updated + r.Skipped }

// contactImportSaved is what saving one contact did.
type contactImportSaved struct {
	existed, saved bool
	// operation is the queued write of the contact to an account, if any.
	operation string
}

// contactImportStore is how one storage mode answers an import.
type contactImportStore struct {
	// accepted says whether a place is one contacts can be saved to.
	accepted func(ctx context.Context, place string) (bool, error)
	// storedIn says whether a contact with this email has a copy in place.
	storedIn func(ctx context.Context, email, place string) (bool, error)
	// run calls work as the storage mode needs it, for the saves.
	run  func(ctx context.Context, work func(context.Context) error) error
	save func(ctx context.Context, contact models.Contact, request contactImportRequest) (contactImportSaved, error)
	// deliver runs one queued contact operation, and says whether there was
	// one to run; progress reads the state of the import's operations.
	deliver  func(ctx context.Context) bool
	progress func(ctx context.Context, ids []string) (storage.ContactOperationProgress, error)
	// fail answers an import that stopped before it saved anything.
	fail func(http.ResponseWriter, *http.Request, error)
}

// contactImportWriteTimeout is how long an import waits for its writes to
// accounts. The queue carries on with the rest in the background.
const contactImportWriteTimeout = 3 * time.Minute

// readContactImport reads the multipart vCard import form. It answers the
// request itself, and returns false, when the form can't be imported.
func readContactImport(w http.ResponseWriter, r *http.Request, store contactImportStore) (contactImportRequest, bool) {
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
	contacts, err := parseVCardContacts(bytes.NewReader(data), nil)
	if err != nil {
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	request := contactImportRequest{
		contacts:  contacts,
		preview:   r.FormValue("preview") == "1",
		streaming: strings.Contains(r.Header.Get("Accept"), "application/x-ndjson"),
		storeIn:   strings.TrimSpace(r.FormValue("store_in")),
		sync:      r.FormValue("gofer_sync") == "1",
		// Without a choice, contacts already there are updated, as imports
		// always did.
		skipExisting: r.FormValue("existing") == "skip",
	}
	if request.storeIn == "" {
		request.storeIn = "local"
	}
	if ok, err := store.accepted(r.Context(), request.storeIn); err != nil || !ok {
		http.Error(w, i18n.T(r.Context(), "server.error.choose_where_to_store_imported_contacts"), http.StatusBadRequest)
		return contactImportRequest{}, false
	}
	for _, target := range models.ContactFilterValues(r.FormValue("sync_with")) {
		if target == request.storeIn || slices.Contains(request.syncWith, target) {
			continue
		}
		// A place that can't sync is left out, as the contact editor does.
		if ok, err := store.accepted(r.Context(), target); err == nil && ok {
			request.syncWith = append(request.syncWith, target)
		}
	}
	return request, true
}

// importedContact is contact as an import saves it: an existing contact
// keeps the places and Gofer Sync it already has, and gains the import's.
func importedContact(contact models.Contact, previous *models.Contact, request contactImportRequest) models.Contact {
	contact.Source = models.ContactOriginImported
	// A new contact lives in the store place; an existing one keeps its home.
	contact.StoredIn = request.storeIn
	contact.SaveTargets = request.targets()
	contact.GoferSyncEnabled = request.sync
	if previous != nil {
		for _, target := range previous.SaveTargets {
			if !slices.Contains(contact.SaveTargets, target) {
				contact.SaveTargets = append(contact.SaveTargets, target)
			}
		}
		contact.GoferSyncEnabled = contact.GoferSyncEnabled || previous.GoferSyncEnabled
	}
	return contact
}

// contactImportNeedsStore says whether a saved contact still has to be
// written to its store place: Gofer Sync writes it there itself when on.
func contactImportNeedsStore(contact models.Contact, request contactImportRequest) bool {
	return request.storeIn != "local" && !contact.GoferSyncEnabled
}

// previewContactImport answers how many of the contacts are already in the
// store place.
func previewContactImport(w http.ResponseWriter, r *http.Request, request contactImportRequest, store contactImportStore) {
	preview := contactImportPreview{Total: len(request.contacts)}
	seen := make(map[string]bool)
	for _, contact := range request.contacts {
		email := strings.ToLower(strings.TrimSpace(contact.Email))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		existing, err := store.storedIn(r.Context(), email, request.storeIn)
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

// runContactImport saves the contacts one by one, then waits for the writes
// to accounts they queued, reporting both as it goes.
func runContactImport(w http.ResponseWriter, r *http.Request, request contactImportRequest, store contactImportStore) {
	// The browser may go; the import carries on regardless.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), contactImportWriteTimeout+2*time.Minute)
	defer cancel()
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
		Phase string `json:"phase"`
		Done  int    `json:"done"`
		Total int    `json:"total"`
	}
	var result contactImportResult
	var operations []string
	line(progressLine{Phase: "saving", Total: total})
	err := store.run(ctx, func(ctx context.Context) error {
		for _, contact := range request.contacts {
			saved, err := store.save(ctx, contact, request)
			if err != nil {
				return err
			}
			switch {
			case !saved.saved:
				result.Skipped++
			case saved.existed:
				result.Updated++
			default:
				result.Added++
			}
			if saved.operation != "" {
				operations = append(operations, saved.operation)
			}
			line(progressLine{Phase: "saving", Done: result.done(), Total: total})
		}
		return nil
	})
	if err != nil {
		saved := result.Added + result.Updated
		if !started {
			if saved == 0 {
				store.fail(w, r, err)
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
	if len(operations) > 0 {
		line(progressLine{Phase: "writing", Total: len(operations)})
		progress := waitContactOperations(ctx, operations, time.Now().Add(contactImportWriteTimeout), store.deliver, store.progress, func(progress storage.ContactOperationProgress) {
			line(progressLine{Phase: "writing", Done: len(operations) - progress.Waiting, Total: len(operations)})
		})
		result.Written, result.Failed = progress.Done, progress.Failed
		// What is still waiting goes on in the background.
		result.Retrying = progress.Retrying + progress.Waiting
		result.Reason = serverText(r.Context(), progress.Reason)
	}
	if !started {
		http.Redirect(w, r, fmt.Sprintf("/contacts?imported=%d", result.Added+result.Updated), http.StatusSeeOther)
		return
	}
	line(result)
}

// waitContactOperations runs queued contact operations, the request's own
// and any others due, until the ids are all finished or have failed once,
// or deadline passes; report hears each step. It gives their last state:
// the ones still waiting then carry on in the background.
func waitContactOperations(ctx context.Context, ids []string, deadline time.Time, deliver func(context.Context) bool, read func(context.Context, []string) (storage.ContactOperationProgress, error), report func(storage.ContactOperationProgress)) storage.ContactOperationProgress {
	var last storage.ContactOperationProgress
	last.Waiting = len(ids)
	for {
		progress, err := read(ctx, ids)
		if err != nil {
			return last
		}
		last = progress
		if report != nil {
			report(progress)
		}
		if progress.Waiting == 0 || ctx.Err() != nil || time.Now().After(deadline) {
			return progress
		}
		if !deliver(ctx) {
			// The background worker holds the rest; check again shortly.
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

func contactImportStopped(ctx context.Context, saved int) string {
	noun := "contacts"
	if saved == 1 {
		noun = "contact"
	}
	return serverText(ctx, fmt.Sprintf("Import stopped after saving %d %s. The remaining contacts were not saved.", saved, noun))
}
