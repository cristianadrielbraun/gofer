package handler

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// While its owner has Gofer open, a calendar is asked every minute whether
// anything in it changed, an answer of a few hundred bytes, and downloaded
// again only when something did. Otherwise it is refreshed on the owner's
// sync interval, as before. Outlook can't say cheaply that an event was
// deleted: deletions there wait for the regular refresh.
const calendarQuickCheckInterval = time.Minute

// calendarChangeSkew covers clocks that disagree a little: a change is
// looked for from a little before the last download began.
const calendarChangeSkew = 10 * time.Second

// calendarChangeMark is what a calendar was like when it was last downloaded:
// a CalDAV calendar's change tag, or when the download began. Marks live in
// memory; after a restart every calendar is downloaded first anyway.
type calendarChangeMark struct {
	tag     string
	since   time.Time
	checked time.Time
}

type calendarChangeMarks struct {
	mu    sync.Mutex
	marks map[string]calendarChangeMark
}

func calendarChangeKey(source storage.CalendarSource) string {
	return source.UserID + "\x00" + source.ID
}

func (m *calendarChangeMarks) get(source storage.CalendarSource) (calendarChangeMark, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mark, ok := m.marks[calendarChangeKey(source)]
	return mark, ok
}

// downloaded records a download that began at since, of a calendar whose
// change tag was then tag.
func (m *calendarChangeMarks) downloaded(source storage.CalendarSource, tag string, since time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.marks == nil {
		m.marks = make(map[string]calendarChangeMark)
	}
	m.marks[calendarChangeKey(source)] = calendarChangeMark{tag: tag, since: since, checked: since}
}

func (m *calendarChangeMarks) checked(source storage.CalendarSource, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mark, ok := m.marks[calendarChangeKey(source)]; ok {
		mark.checked = at
		m.marks[calendarChangeKey(source)] = mark
	}
}

// forget drops the marks of an owner's calendars that are no longer
// selected, so the map holds only calendars that are synced.
func (m *calendarChangeMarks) forget(owner string, selected []storage.CalendarSource) {
	keep := make(map[string]bool, len(selected))
	for _, source := range selected {
		keep[calendarChangeKey(source)] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.marks {
		if strings.HasPrefix(key, owner+"\x00") && !keep[key] {
			delete(m.marks, key)
		}
	}
}

// changeTag is the change tag of the request's CalDAV or JMAP calendar.
func (p *userCalendarRequest) changeTag(ctx context.Context, source storage.CalendarSource) (string, error) {
	if source.Provider == storage.CalendarSourceProviderJMAP {
		cfg, password, err := p.jmapConnection(ctx)
		if err != nil {
			return "", err
		}
		return jmapCalendarChangeTag(ctx, cfg, password)
	}
	return p.calDAVChangeTag(ctx, source)
}

// calDAVChangeTag is the change tag of the request's CalDAV calendar.
func (p *userCalendarRequest) calDAVChangeTag(ctx context.Context, source storage.CalendarSource) (string, error) {
	username, password, err := p.calDAVCredentials(source)
	if err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, calendarDiscoveryDAVGuardKey{}, calendarDiscoveryDAVCallbacks{ready: p.ready, failure: p.recordRetry})
	return calDAVCalendarChangeTag(ctx, source.RemoteID, username, password)
}

// calDAVCalendarChangeTag is a CalDAV calendar's change tag: its ctag, or
// its sync token. Empty when the server offers neither.
func calDAVCalendarChangeTag(ctx context.Context, calendarURL, username, password string) (string, error) {
	multi, err := calDAVRequest(ctx, "PROPFIND", calendarURL, username, password, "0",
		`<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/><d:sync-token/></d:prop></d:propfind>`, 15*time.Second)
	if err != nil {
		return "", err
	}
	for _, response := range multi.Responses {
		for _, propStat := range response.PropStats {
			if propStat.Status != "" && !strings.Contains(propStat.Status, " 200 ") {
				continue
			}
			if tag := strings.TrimSpace(propStat.Prop.CTag); tag != "" {
				return "ctag:" + tag, nil
			}
			if tag := strings.TrimSpace(propStat.Prop.SyncToken); tag != "" {
				return "sync:" + tag, nil
			}
		}
	}
	return "", nil
}

// calendarChanged asks the provider whether anything in the calendar changed
// since mark. getJSON reads from Google or Outlook, changeTag reads a CalDAV
// or JMAP calendar's change tag.
func calendarChanged(source storage.CalendarSource, mark calendarChangeMark, getJSON func(endpoint string, out any) error, changeTag func() (string, error)) (changed bool, tag string, err error) {
	since := mark.since.Add(-calendarChangeSkew).UTC().Format(time.RFC3339)
	switch source.Provider {
	case "gmail":
		values := url.Values{"updatedMin": {since}, "showDeleted": {"true"}, "maxResults": {"1"}, "fields": {"items(id)"}}
		var page struct{ Items []struct{ ID string } }
		err := getJSON(googleCalendarAPIBaseURL+"/calendars/"+url.PathEscape(source.RemoteID)+"/events?"+values.Encode(), &page)
		return len(page.Items) > 0, "", err
	case "outlook":
		values := url.Values{"$filter": {"lastModifiedDateTime ge " + since}, "$top": {"1"}, "$select": {"id"}}
		var page struct{ Value []struct{ ID string } }
		err := getJSON(outlookGraphBaseURL+"/me/calendars/"+url.PathEscape(source.RemoteID)+"/events?"+values.Encode(), &page)
		return len(page.Value) > 0, "", err
	case storage.CalendarSourceProviderCalDAV, storage.CalendarSourceProviderJMAP:
		tag, err := changeTag()
		// A server without change tags is only refreshed on its schedule.
		return err == nil && tag != "" && tag != mark.tag, tag, err
	}
	return false, "", nil
}

// calendarChangeDue reports whether a calendar is to be asked for changes
// now: downloaded before, not being downloaded, not due for its regular
// refresh, and not asked within the last minute.
func calendarChangeDue(source storage.CalendarSource, mark calendarChangeMark, known bool, now time.Time) bool {
	return known && source.SyncState != "syncing" && source.NextAttemptAt != nil && source.NextAttemptAt.After(now) && now.Sub(mark.checked) >= calendarQuickCheckInterval-calendarChangeSkew
}

// checkUserCalendarChanges downloads again those of an account's calendars
// that changed since their last download, for an owner who has Gofer open.
// Calendars due for their regular refresh are left to it.
func (h *Handler) checkUserCalendarChanges(ctx context.Context, owner, account string, start, end time.Time) error {
	return h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		var sources []storage.CalendarSource
		if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			sources, err = db.ListSelectedCalendarSources(ctx, owner)
			return err
		}); err != nil {
			return err
		}
		h.calendarChanges.forget(owner, sources)
		var failures []error
		for _, source := range sources {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			mark, ok := h.calendarChanges.get(source)
			now := time.Now()
			if source.AccountID != account || !calendarChangeDue(source, mark, ok, now) {
				continue
			}
			changed, err := h.userCalendarSourceChanged(ctx, source, mark)
			h.calendarChanges.checked(source, now)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if changed {
				if _, err := h.syncUserCalendarSource(ctx, source, start, end, true, true); err != nil {
					failures = append(failures, err)
				}
			}
		}
		return errors.Join(failures...)
	})
}

func (h *Handler) userCalendarSourceChanged(ctx context.Context, source storage.CalendarSource, mark calendarChangeMark) (changed bool, err error) {
	err = h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, 30*time.Second, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarSource(ctx, source.UserID, source.ID)
		if err != nil {
			return err
		}
		current := snapshot.Source()
		if current.AccountID != source.AccountID || current.RemoteID != source.RemoteID || current.Provider != source.Provider {
			return storage.ErrCalendarSourceChanged
		}
		p := &userCalendarRequest{h: h, source: snapshot}
		if calendarProviderUsesOAuth(source.Provider) && h.userCredentials == nil {
			return errors.New("mailbox OAuth is not configured")
		}
		if calendarProviderUsesOAuth(source.Provider) {
			p.credentials = h.userCredentials.CalendarAccount(source.UserID, source.AccountID, false)
		}
		changed, _, err = calendarChanged(current, mark, func(endpoint string, out any) error { return p.getJSON(ctx, endpoint, out) }, func() (string, error) { return p.changeTag(ctx, current) })
		return err
	})
	return changed, err
}
