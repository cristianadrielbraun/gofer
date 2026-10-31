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

// errGoogleCalendarNeedsAccess is Google refusing to change a calendar Gofer
// didn't create, which full calendar access allows.
var errGoogleCalendarNeedsAccess = errors.New("Google lets Gofer rename or delete only the calendars it created. Change this one in Google Calendar, or allow Gofer full calendar access in the account's Calendar settings.")

// errGoogleCalendarListNeedsAccess is Google refusing to change the account's
// own calendar list, which full calendar access allows.
var errGoogleCalendarListNeedsAccess = errors.New("Google lets Gofer change your calendar list only with full calendar access. Change it in Google Calendar, or allow Gofer full calendar access in the account's Calendar settings.")

// errCalendarUndeletable refuses deleting a calendar its provider won't
// delete, such as a Google or Microsoft account's main calendar, saying why.
type errCalendarUndeletable string

func (e errCalendarUndeletable) Error() string { return string(e) }

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
		if err == nil && name != "" && source.Provider == providers.ProviderGmail {
			clearGoogleCalendarListName(ctx, source, login.token)
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

// clearGoogleCalendarListName drops the account's own name for a calendar it
// just renamed, which Google shows instead of the calendar's name: Google's
// family calendar comes with one, for example. Without full calendar access
// Google refuses it, and the calendar was renamed anyway.
func clearGoogleCalendarListName(ctx context.Context, source storage.CalendarSource, token string) {
	endpoint := googleCalendarAPIBaseURL + "/users/me/calendarList/" + url.PathEscape(source.RemoteID)
	if err := userContactProviderJSON(ctx, providers.ProviderGmail, http.MethodPatch, endpoint, token, map[string]any{"summaryOverride": nil}, nil); err != nil {
		log.Printf("calendar: clear own name for renamed calendar %s: %v", source.ID, err)
	}
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
			return errGoogleCalendarNeedsAccess
		}
		return errors.New("Microsoft didn't allow this change. Reconnect the account, or change the calendar in Outlook.")
	}
	return err
}

// manageGoogleCalendarListEntry renames a Google calendar only in the
// account's own calendar list, or with name empty removes it from that list.
// The calendar itself, and what everyone else sees, stays as it is. A
// calendar already gone from the list counts as removed.
func manageGoogleCalendarListEntry(ctx context.Context, source storage.CalendarSource, token, name string) error {
	endpoint := googleCalendarAPIBaseURL + "/users/me/calendarList/" + url.PathEscape(source.RemoteID)
	var err error
	if name == "" {
		err = userContactProviderJSON(ctx, providers.ProviderGmail, http.MethodDelete, endpoint, token, nil, nil)
	} else {
		err = userContactProviderJSON(ctx, providers.ProviderGmail, http.MethodPatch, endpoint, token, map[string]any{"summaryOverride": name}, nil)
	}
	var google googleAPIError
	if err == nil || !errors.As(err, &google) {
		return err
	}
	switch {
	case google.Status == http.StatusForbidden || google.Status == http.StatusUnauthorized:
		return errGoogleCalendarListNeedsAccess
	case name == "" && (google.Status == http.StatusNotFound || google.Status == http.StatusGone):
		return nil
	case google.Status == http.StatusNotFound:
		return errors.New("This calendar is no longer in your Google calendar list. Refresh the page.")
	}
	return err
}

// calendarChange is a rename's new name, or none for a delete. Personal
// renames or removes a Google calendar only in the account's own list.
type calendarChange struct {
	name     string
	personal bool
}

// readCalendarChange reads a rename request's new name, or none for DELETE.
// ?just_for_me=1 on a DELETE, or "just_for_me" in a rename, changes only the
// account's own calendar list.
func readCalendarChange(w http.ResponseWriter, r *http.Request) (calendarChange, error) {
	if r.Method == http.MethodDelete {
		return calendarChange{personal: r.URL.Query().Get("just_for_me") == "1"}, nil
	}
	var request struct {
		Name      string `json:"name"`
		JustForMe bool   `json:"just_for_me"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return calendarChange{}, errors.New("The new name could not be read.")
	}
	change := calendarChange{name: strings.TrimSpace(request.Name), personal: request.JustForMe}
	if change.name == "" {
		return change, errors.New("Enter a name.")
	}
	return change, checkCollectionName(change.name)
}

// manageCalendarSource makes a change on the calendar's provider: in the
// account's own Google calendar list when it is personal.
func manageCalendarSource(ctx context.Context, source storage.CalendarSource, login calendarManageLogin, change calendarChange) error {
	if !change.personal {
		return manageProviderCalendar(ctx, source, login, change.name)
	}
	if source.Provider != providers.ProviderGmail {
		return errors.New("Only Google calendars can be renamed or removed just for you.")
	}
	return manageGoogleCalendarListEntry(ctx, source, login.token, change.name)
}

// handleCalendarSourceManage renames a synced calendar on its provider, or
// with DELETE deletes it there with all its events, then reads the account's
// calendar list again so Gofer shows the change.
func (h *Handler) handleCalendarSourceManage(w http.ResponseWriter, r *http.Request) {
	sourceID, owner := strings.TrimSpace(r.PathValue("id")), h.userID(r.Context())
	change, err := readCalendarChange(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.userStorage != nil {
		err = h.manageUserCalendar(r.Context(), owner, sourceID, change)
	} else {
		err = h.manageCalendar(r.Context(), owner, sourceID, change)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "This calendar is no longer synced. Refresh the page.", http.StatusNotFound)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner):
		userAccountError(w, r, err)
	case errors.As(err, new(errCalendarUndeletable)):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errGoogleCalendarNeedsAccess) || errors.Is(err, errGoogleCalendarListNeedsAccess):
		// The page offers to sign in again with full calendar access.
		w.Header().Set("X-Gofer-Needs-Calendar-Access", "google")
		http.Error(w, err.Error(), http.StatusForbidden)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// syncedCalendarSource is one of the owner's synced calendars, refusing a
// delete its provider won't do. Removing one just from the account's own
// list is allowed.
func syncedCalendarSource(ctx context.Context, db *storage.DB, owner, sourceID string, change calendarChange) (storage.CalendarSource, error) {
	sources, err := db.ListSelectedCalendarSources(ctx, owner)
	if err != nil {
		return storage.CalendarSource{}, err
	}
	for _, source := range sources {
		if source.ID == sourceID {
			if reason := source.UndeletableReason(); reason != "" && change.name == "" && !change.personal {
				return source, errCalendarUndeletable(reason)
			}
			return source, nil
		}
	}
	return storage.CalendarSource{}, sql.ErrNoRows
}

// manageUserCalendar is the per-user store's rename or delete.
func (h *Handler) manageUserCalendar(ctx context.Context, owner, sourceID string, change calendarChange) error {
	name := change.name
	var source storage.CalendarSource
	if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		source, err = syncedCalendarSource(ctx, db, owner, sourceID, change)
		return err
	}); err != nil {
		return err
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
		return request.recordRetry(ctx, manageCalendarSource(ctx, source, login, change))
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
func (h *Handler) manageCalendar(ctx context.Context, userID, sourceID string, change calendarChange) error {
	name := change.name
	source, err := syncedCalendarSource(ctx, h.db, userID, sourceID, change)
	if err != nil {
		return err
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
	if err := manageCalendarSource(ctx, source, login, change); err != nil {
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
