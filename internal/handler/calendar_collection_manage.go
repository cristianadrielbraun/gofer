package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// errCalendarPrimary refuses deleting a Google or Microsoft account's main
// calendar, which neither allows.
var errCalendarPrimary = errors.New("The account's main calendar can't be deleted.")

// calendarManageLogin signs in to a calendar's provider to rename or delete
// it: an OAuth token, a CalDAV login, or the JMAP connection.
type calendarManageLogin struct {
	token                       string
	baseURL, username, password string
	jmap                        *models.AccountConfig
}

// manageProviderCalendar renames a calendar on its provider, or with name
// empty deletes it with all its events. A calendar already gone counts as
// deleted.
func manageProviderCalendar(ctx context.Context, source storage.CalendarSource, login calendarManageLogin, name string) error {
	switch source.Provider {
	case providers.ProviderGmail, providers.ProviderOutlook:
		endpoint, payload := googleCalendarAPIBaseURL+"/calendars/"+url.PathEscape(source.RemoteID), map[string]any{"summary": name}
		if source.Provider == providers.ProviderOutlook {
			endpoint, payload = outlookGraphBaseURL+"/me/calendars/"+url.PathEscape(source.RemoteID), map[string]any{"name": name}
		}
		var err error
		if name == "" {
			err = userContactProviderJSON(ctx, source.Provider, http.MethodDelete, endpoint, login.token, nil, nil)
		} else {
			err = userContactProviderJSON(ctx, source.Provider, http.MethodPatch, endpoint, login.token, payload, nil)
		}
		return calendarManageError(source.Provider, name, err)
	case storage.CalendarSourceProviderCalDAV:
		target, err := resolveCalDAVHref(login.baseURL, source.RemoteID)
		if err != nil {
			return err
		}
		if name == "" {
			return deleteDAVCollection(ctx, calDAVHTTPTransport, target, login.username, login.password, "calendar")
		}
		return renameDAVCollection(ctx, calDAVHTTPTransport, target, login.username, login.password, name, "calendar")
	case storage.CalendarSourceProviderJMAP:
		id, collection, ok := jmapCalendarDAVParse(source.RemoteID)
		if !ok || collection != "" || login.jmap == nil {
			return errors.New("this calendar's JMAP id can't be read")
		}
		if name == "" {
			return mail.DeleteJMAPCalendar(ctx, login.jmap, login.password, id)
		}
		return mail.RenameJMAPCalendar(ctx, login.jmap, login.password, id, name)
	}
	return errors.New("this calendar's provider can't rename or delete calendars")
}

// calendarManageError explains a Google or Microsoft refusal to rename or
// delete a calendar. Google lets Gofer change only the calendars it made.
func calendarManageError(provider, name string, err error) error {
	if err == nil {
		return nil
	}
	status := 0
	var google googleAPIError
	var outlook outlookAPIError
	if errors.As(err, &google) {
		status = google.Status
	} else if errors.As(err, &outlook) {
		status = outlook.Status
	}
	// Google answers 404 for a calendar Gofer didn't make, as if it weren't
	// there, rather than refusing it; it isn't gone.
	hidden := provider == providers.ProviderGmail && status == http.StatusNotFound
	if name == "" && !hidden && (status == http.StatusNotFound || status == http.StatusGone) {
		return nil
	}
	if hidden || status == http.StatusForbidden || status == http.StatusUnauthorized {
		if provider == providers.ProviderGmail {
			return errors.New("Google lets Gofer rename or delete only the calendars it created. Change this one in Google Calendar.")
		}
		return errors.New("Microsoft didn't allow this change. Reconnect the account, or change the calendar in Outlook.")
	}
	return err
}

// readCalendarChange reads a rename request's new name, or none for DELETE.
func readCalendarChange(w http.ResponseWriter, r *http.Request) (string, error) {
	if r.Method == http.MethodDelete {
		return "", nil
	}
	var request struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("The new name could not be read.")
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return "", errors.New("Enter a name.")
	}
	return name, checkCollectionName(name)
}

// handleCalendarSourceManage renames a synced calendar on its provider, or
// with DELETE deletes it there with all its events, then reads the account's
// calendar list again so Gofer shows the change.
func (h *Handler) handleCalendarSourceManage(w http.ResponseWriter, r *http.Request) {
	sourceID, owner := strings.TrimSpace(r.PathValue("id")), h.userID(r.Context())
	name, err := readCalendarChange(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.userStorage != nil {
		err = h.manageUserCalendar(r.Context(), owner, sourceID, name)
	} else {
		err = h.manageCalendar(r.Context(), owner, sourceID, name)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "This calendar is no longer synced. Refresh the page.", http.StatusNotFound)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner):
		userAccountError(w, r, err)
	case errors.Is(err, errCalendarPrimary):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// syncedCalendarSource is one of the owner's synced calendars.
func syncedCalendarSource(ctx context.Context, db *storage.DB, owner, sourceID string) (storage.CalendarSource, error) {
	sources, err := db.ListSelectedCalendarSources(ctx, owner)
	if err != nil {
		return storage.CalendarSource{}, err
	}
	for _, source := range sources {
		if source.ID == sourceID {
			if source.IsPrimary && (source.Provider == providers.ProviderGmail || source.Provider == providers.ProviderOutlook) {
				return source, errCalendarPrimaryIfDelete
			}
			return source, nil
		}
	}
	return storage.CalendarSource{}, sql.ErrNoRows
}

// errCalendarPrimaryIfDelete marks a Google or Microsoft main calendar,
// which can be renamed but not deleted.
var errCalendarPrimaryIfDelete = errors.New("main calendar")

// manageUserCalendar is the per-user store's rename or delete.
func (h *Handler) manageUserCalendar(ctx context.Context, owner, sourceID, name string) error {
	var source storage.CalendarSource
	if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		source, err = syncedCalendarSource(ctx, db, owner, sourceID)
		return err
	}); err != nil {
		if !errors.Is(err, errCalendarPrimaryIfDelete) {
			return err
		}
		if name == "" {
			return errCalendarPrimary
		}
	}
	err := h.userIMAP.RunAccountService(ctx, owner, source.AccountID, mail.AccountServiceCalendar, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarDiscovery(ctx, owner, source.AccountID)
		if err != nil {
			return err
		}
		request := &userCalendarRequest{h: h, snapshot: snapshot}
		if err := request.ready(ctx); err != nil {
			return err
		}
		var login calendarManageLogin
		switch source.Provider {
		case storage.CalendarSourceProviderCalDAV:
			form := url.Values{}
			if baseURL, _, useAccount := snapshot.Service().CalDAVSettings(); baseURL != "" {
				form.Set("caldav_url", baseURL)
				if useAccount {
					form.Set("use_account_credentials", "1")
				}
			}
			if login.baseURL, login.username, login.password, _, err = request.calDAVLogin(form); err != nil {
				return err
			}
		case storage.CalendarSourceProviderJMAP:
			if login.jmap, login.password, err = request.jmapConnection(ctx); err != nil {
				return err
			}
		default:
			if h.userCredentials == nil {
				return errors.New("Mailbox OAuth is not configured for this Gofer instance.")
			}
			authorization, err := h.userCredentials.CalendarCreateAccount(owner, source.AccountID).ServiceAuthorization(ctx, false)
			if err != nil {
				return request.recordRetry(ctx, err)
			}
			login.token = authorization.Token()
		}
		return request.recordRetry(ctx, manageProviderCalendar(ctx, source, login, name))
	})
	if err != nil {
		return err
	}
	if name == "" {
		// Unticked at once, in case the list read below can't drop it.
		var ids []string
		if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			_, ids, err = calendarSourceSelection(ctx, db, owner, sourceID, false)
			return err
		}); err == nil {
			if err := h.userAccounts.SetCalendarSourcesSelected(ctx, owner, source.AccountID, ids); err != nil {
				log.Printf("owned calendar: untick deleted calendar %s: %v", sourceID, err)
			}
		}
	}
	h.calendarListRead.Store(source.AccountID, time.Now())
	if _, err := h.runUserCalendarDiscovery(ctx, owner, source.AccountID, nil, nil, true); err != nil {
		log.Printf("owned calendar: refresh calendar list %s: %v", source.AccountID, err)
	}
	return nil
}

// manageCalendar is the shared store's rename or delete.
func (h *Handler) manageCalendar(ctx context.Context, userID, sourceID, name string) error {
	source, err := syncedCalendarSource(ctx, h.db, userID, sourceID)
	if err != nil {
		if !errors.Is(err, errCalendarPrimaryIfDelete) {
			return err
		}
		if name == "" {
			return errCalendarPrimary
		}
	}
	credential := h.calendarCredentialsForSource(ctx, userID, source)
	login := calendarManageLogin{baseURL: credential.baseURL, username: credential.username, password: credential.password, jmap: credential.jmap}
	if source.Provider == providers.ProviderGmail || source.Provider == providers.ProviderOutlook {
		if h.mailCredentials() == nil {
			return errors.New("Mailbox OAuth is not configured for this Gofer instance.")
		}
		if login.token, err = h.mailCredentials().GetCalendarCreateTokenForAccount(ctx, source.AccountID, source.Provider); err != nil {
			return err
		}
	} else if credential.err != nil {
		return credential.err
	}
	if err := manageProviderCalendar(ctx, source, login, name); err != nil {
		return err
	}
	if name == "" {
		if accountID, ids, err := calendarSourceSelection(ctx, h.db, userID, sourceID, false); err == nil {
			if err := h.db.SetCalendarSourceSelection(ctx, userID, accountID, ids); err != nil {
				log.Printf("calendar: untick deleted calendar %s: %v", sourceID, err)
			}
		}
	}
	h.calendarListRead.Store(source.AccountID, time.Now())
	if err := h.refreshCalendarList(ctx, source, map[string]calendarCredentials{}); err != nil {
		log.Printf("calendar list %s: %v", source.AccountID, err)
	}
	return nil
}
