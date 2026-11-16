package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// sharedMailboxPolls is how many times, two seconds apart, the shared
// mailbox panel asks again while a sync looks for an account's mailboxes.
const sharedMailboxPolls = 45

// sharedMailboxChoices gives an account's shared mailboxes as the account
// wizard lists them: those still shared first, by address.
func (h *Handler) sharedMailboxChoices(ctx context.Context, accountID string) ([]views.SharedMailboxChoice, error) {
	mailboxes, err := h.db.ListSharedMailboxes(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]views.SharedMailboxChoice, 0, len(mailboxes))
	for _, m := range mailboxes {
		out = append(out, views.SharedMailboxChoice{
			Email: m.Email, Name: m.Name, CustomName: m.CustomName, DefaultName: sharedmailbox.MailboxName(m.Email, m.ServerName, ""),
			Enabled: m.Enabled, Removed: m.State == storage.SharedMailboxRemoved,
		})
	}
	slices.SortStableFunc(out, func(a, b views.SharedMailboxChoice) int {
		if a.Removed != b.Removed {
			if a.Removed {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Email, b.Email)
	})
	return out, nil
}

// sharedMailboxPanelAccount checks that the path's account is the user's.
func (h *Handler) sharedMailboxPanelAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	accountID := r.PathValue("id")
	account, err := h.ownedAccount(r.Context(), accountID)
	if err != nil || account == nil {
		http.NotFound(w, r)
		return "", false
	}
	return accountID, true
}

// renderSharedMailboxesPanel draws the panel. With no mailbox found yet it
// keeps searching, while polls remain, until a sync has looked for them at
// or after since (any time, when since is zero); a look that found none says
// so.
func (h *Handler) renderSharedMailboxesPanel(w http.ResponseWriter, r *http.Request, accountID string, poll int, since time.Time, failure string) {
	choices, err := h.sharedMailboxChoices(r.Context(), accountID)
	if err != nil {
		log.Printf("shared mailboxes account=%s: %v", accountID, err)
		failure = i18n.T(r.Context(), "settings.shared_mailboxes.load_failed")
	}
	data := views.SharedMailboxesPanelData{AccountID: accountID, Mailboxes: choices, Error: failure, Since: since}
	if len(choices) == 0 && failure == "" {
		checkedAt, checked, err := h.db.SharedMailboxesCheckedAt(r.Context(), accountID)
		if err != nil {
			log.Printf("shared mailboxes account=%s: %v", accountID, err)
		}
		looked := checked && !checkedAt.Before(since)
		switch {
		case looked:
		case poll > 0:
			data.Searching, data.Poll = true, poll
		default:
			data.NotYet = true
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SharedMailboxesPanel(data).Render(r.Context(), w); err != nil {
		log.Printf("render shared mailboxes: %v", err)
	}
}

// handleSharedMailboxesPanel shows an account's shared mailboxes. A poll
// count keeps it asking while a search runs.
func (h *Handler) handleSharedMailboxesPanel(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxPanelAccount(w, r)
	if !ok {
		return
	}
	poll, _ := strconv.Atoi(r.URL.Query().Get("poll"))
	var since time.Time
	if n, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && n > 0 {
		since = time.Unix(0, n)
	}
	h.renderSharedMailboxesPanel(w, r, accountID, min(max(poll, 0), sharedMailboxPolls), since, "")
}

// handleDiscoverSharedMailboxes looks for an account's shared mailboxes now,
// with a sync of the account, which finds them as it lists the folders.
func (h *Handler) handleDiscoverSharedMailboxes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxPanelAccount(w, r)
	if !ok {
		return
	}
	since := time.Now()
	if err := h.db.RequestSharedMailboxCheck(r.Context(), accountID, since); err != nil {
		log.Printf("shared mailboxes account=%s: %v", accountID, err)
	}
	h.startSharedMailboxSync(accountID, h.userID(r.Context()))
	h.renderSharedMailboxesPanel(w, r, accountID, sharedMailboxPolls, since, "")
}

// handleSetSharedMailboxEnabled turns one of an account's shared mailboxes
// on or off (form values email and enabled). Turned on, a sync brings in its
// mail; turned off, its folders are hidden and it is no longer synced.
func (h *Handler) handleSetSharedMailboxEnabled(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxPanelAccount(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	enabled := slices.Contains(r.Form["enabled"], "true")
	failure := ""
	err := h.db.SetSharedMailboxEnabled(r.Context(), accountID, r.FormValue("email"), enabled)
	switch {
	case errors.Is(err, storage.ErrSharedMailboxNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		log.Printf("shared mailbox enabled account=%s: %v", accountID, err)
		failure = i18n.T(r.Context(), "settings.shared_mailboxes.change_failed")
	default:
		h.startSharedMailboxSync(accountID, h.userID(r.Context()))
	}
	h.renderSharedMailboxesPanel(w, r, accountID, 0, time.Time{}, failure)
}

// startSharedMailboxSync syncs the account in the background, which finds
// its shared mailboxes and brings in or leaves out their mail.
func (h *Handler) startSharedMailboxSync(accountID, userID string) {
	if h.syncer == nil {
		return
	}
	h.syncer.SyncAccounts(context.Background(), userID, []string{accountID})
}

// handleSharedMailboxesSummary shows, on an account's card, the shared
// mailboxes it opens.
func (h *Handler) handleSharedMailboxesSummary(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxPanelAccount(w, r)
	if !ok {
		return
	}
	choices, err := h.sharedMailboxChoices(r.Context(), accountID)
	if err != nil {
		log.Printf("shared mailboxes account=%s: %v", accountID, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SharedMailboxesSummary(accountID, choices).Render(r.Context(), w); err != nil {
		log.Printf("render shared mailbox summary: %v", err)
	}
}

// handleSetSharedMailboxName gives one of an account's shared mailboxes the
// name the user chose (form values email and name); an empty name goes back
// to the server's or the address's.
func (h *Handler) handleSetSharedMailboxName(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxPanelAccount(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	failure := ""
	err := h.db.SetSharedMailboxName(r.Context(), accountID, r.FormValue("email"), r.FormValue("name"))
	switch {
	case errors.Is(err, storage.ErrSharedMailboxNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		log.Printf("shared mailbox name account=%s: %v", accountID, err)
		failure = i18n.T(r.Context(), "settings.shared_mailboxes.change_failed")
	}
	h.renderSharedMailboxesPanel(w, r, accountID, 0, time.Time{}, failure)
}

// handleSharedMailboxesUnavailable takes the place of the shared mailbox
// step where choosing shared mailboxes is not served.
func handleSharedMailboxesUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SharedMailboxesUnavailable().Render(r.Context(), w); err != nil {
		log.Printf("render shared mailboxes: %v", err)
	}
}
