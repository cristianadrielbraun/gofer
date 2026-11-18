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

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// sharedMailboxPolls is how many times, two seconds apart, the shared
// mailbox panel asks again while a sync looks for an account's mailboxes.
const sharedMailboxPolls = 45

var errSharedMailboxAccount = errors.New("account not found")

// withSharedMailboxStore runs fn on the store that holds the user's account:
// the user's own in managed mode, else the installation's. It fails for an
// account that is not the user's.
func (h *Handler) withSharedMailboxStore(ctx context.Context, accountID string, fn func(*storage.DB) error) error {
	if h.userAccounts != nil {
		return h.userAccounts.WithAccountForUser(ctx, h.userID(ctx), accountID, func(_ *config.AccountStore, db *storage.DB) error {
			return fn(db)
		})
	}
	account, err := h.ownedAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if account == nil {
		return errSharedMailboxAccount
	}
	return fn(h.db)
}

// sharedMailboxChoices gives an account's shared mailboxes as the shared
// mailboxes dialog lists them: those still shared first, by address.
func sharedMailboxChoices(ctx context.Context, db *storage.DB, accountID string) ([]views.SharedMailboxChoice, error) {
	mailboxes, err := db.ListSharedMailboxes(ctx, accountID)
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

// sharedMailboxRequest checks that the path's account is the user's, and
// answers not found when it isn't.
func (h *Handler) sharedMailboxRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	accountID := r.PathValue("id")
	if err := h.withSharedMailboxStore(r.Context(), accountID, func(*storage.DB) error { return nil }); err != nil {
		if !errors.Is(err, errSharedMailboxAccount) {
			log.Printf("shared mailboxes account=%s: %v", accountID, err)
		}
		http.NotFound(w, r)
		return "", false
	}
	return accountID, true
}

// sharedMailboxesPanelData gathers what the panel shows. With no mailbox
// found yet it keeps searching, while polls remain, until a sync has looked
// for them at or after since (any time, when since is zero); a look that
// found none says so.
func (h *Handler) sharedMailboxesPanelData(ctx context.Context, accountID string, poll int, since time.Time, failure string) views.SharedMailboxesPanelData {
	data := views.SharedMailboxesPanelData{AccountID: accountID, Error: failure, Since: since}
	var checkedAt time.Time
	var checked bool
	err := h.withSharedMailboxStore(ctx, accountID, func(db *storage.DB) error {
		var err error
		if data.Mailboxes, err = sharedMailboxChoices(ctx, db, accountID); err != nil {
			return err
		}
		checkedAt, checked, err = db.SharedMailboxesCheckedAt(ctx, accountID)
		return err
	})
	if err != nil {
		log.Printf("shared mailboxes account=%s: %v", accountID, err)
		data.Error = i18n.T(ctx, "settings.shared_mailboxes.load_failed")
		return data
	}
	if len(data.Mailboxes) == 0 && data.Error == "" {
		switch {
		case checked && !checkedAt.Before(since):
		case poll > 0:
			data.Searching, data.Poll = true, poll
		default:
			data.NotYet = true
		}
	}
	return data
}

// handleManageSharedMailboxes opens the shared mailboxes dialog of an
// account, from its menu in Settings → Accounts.
func (h *Handler) handleManageSharedMailboxes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxRequest(w, r)
	if !ok {
		return
	}
	data := h.sharedMailboxesPanelData(r.Context(), accountID, sharedMailboxPolls, time.Time{}, "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SharedMailboxesDialog(data).Render(r.Context(), w); err != nil {
		log.Printf("render shared mailboxes dialog: %v", err)
	}
}

// handleSharedMailboxesPanel shows an account's shared mailboxes. A poll
// count keeps it asking while a search runs.
func (h *Handler) handleSharedMailboxesPanel(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxRequest(w, r)
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

func (h *Handler) renderSharedMailboxesPanel(w http.ResponseWriter, r *http.Request, accountID string, poll int, since time.Time, failure string) {
	data := h.sharedMailboxesPanelData(r.Context(), accountID, poll, since, failure)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SharedMailboxesPanel(data).Render(r.Context(), w); err != nil {
		log.Printf("render shared mailboxes: %v", err)
	}
}

// handleDiscoverSharedMailboxes looks for an account's shared mailboxes now,
// with a sync of the account, which finds them as it lists the folders.
func (h *Handler) handleDiscoverSharedMailboxes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxRequest(w, r)
	if !ok {
		return
	}
	since := time.Now()
	if err := h.withSharedMailboxStore(r.Context(), accountID, func(db *storage.DB) error {
		return db.RequestSharedMailboxCheck(r.Context(), accountID, since)
	}); err != nil {
		log.Printf("shared mailboxes account=%s: %v", accountID, err)
	}
	h.startSharedMailboxSync(r.Context(), accountID)
	h.renderSharedMailboxesPanel(w, r, accountID, sharedMailboxPolls, since, "")
}

// handleSetSharedMailboxEnabled turns one of an account's shared mailboxes
// on or off (form values email and enabled). Turned on, a sync brings in its
// mail; turned off, its folders are hidden and it is no longer synced.
func (h *Handler) handleSetSharedMailboxEnabled(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxRequest(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	enabled := slices.Contains(r.Form["enabled"], "true")
	err := h.withSharedMailboxStore(r.Context(), accountID, func(db *storage.DB) error {
		return db.SetSharedMailboxEnabled(r.Context(), accountID, r.FormValue("email"), enabled)
	})
	if err == nil {
		h.startSharedMailboxSync(r.Context(), accountID)
	}
	h.finishSharedMailboxChange(w, r, accountID, err)
}

// handleSetSharedMailboxName gives one of an account's shared mailboxes the
// name the user chose (form values email and name); an empty name goes back
// to the server's or the address's.
func (h *Handler) handleSetSharedMailboxName(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.sharedMailboxRequest(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	err := h.withSharedMailboxStore(r.Context(), accountID, func(db *storage.DB) error {
		return db.SetSharedMailboxName(r.Context(), accountID, r.FormValue("email"), r.FormValue("name"))
	})
	h.finishSharedMailboxChange(w, r, accountID, err)
}

// finishSharedMailboxChange answers a change to one shared mailbox with the
// panel, saying when the change failed.
func (h *Handler) finishSharedMailboxChange(w http.ResponseWriter, r *http.Request, accountID string, err error) {
	failure := ""
	switch {
	case errors.Is(err, storage.ErrSharedMailboxNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		log.Printf("shared mailbox change account=%s: %v", accountID, err)
		failure = i18n.T(r.Context(), "settings.shared_mailboxes.change_failed")
	}
	h.renderSharedMailboxesPanel(w, r, accountID, 0, time.Time{}, failure)
}

// startSharedMailboxSync syncs the account in the background, which finds
// its shared mailboxes and brings in or leaves out their mail.
func (h *Handler) startSharedMailboxSync(ctx context.Context, accountID string) {
	if h.userIMAP != nil {
		if err := h.userIMAP.QueueAccount(ctx, accountID); err != nil {
			log.Printf("shared mailboxes account=%s: queue a sync: %v", accountID, err)
		}
		return
	}
	if h.syncer != nil {
		h.syncer.SyncAccounts(context.Background(), h.userID(ctx), []string{accountID})
	}
}
