package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/google/uuid"
)

// Importing an .ics file into a synced calendar, and exporting one as an .ics
// file, from the calendar's menu. An import adds every event in the file as a
// new event; one the calendar already has, by its UID, or one the provider
// refuses is skipped and counted. Nothing in the calendar is changed or
// removed, and no invitations are sent for imported events. CalDAV and JMAP
// calendars take the file's events as they are (JMAP through
// jmapCalendarDAV); Google and Microsoft calendars get them through their
// APIs, in calendar_ics_transfer_providers.go.

const (
	calendarImportMaxSize   = 10 << 20
	calendarImportMaxEvents = 5000
	calendarTransferTimeout = time.Hour
)

// calendarTransferProblem is a failure already worded for the person.
type calendarTransferProblem string

func (e calendarTransferProblem) Error() string { return string(e) }

// calendarImportResult counts the events added, the ones the calendar
// already had, by UID, and the ones it couldn't take, Reason saying why for
// the first of those.
type calendarImportResult struct {
	Added    int    `json:"added"`
	Existing int    `json:"existing"`
	Failed   int    `json:"failed"`
	Reason   string `json:"reason,omitempty"`
	Calendar string `json:"calendar,omitempty"`
	Created  bool   `json:"created,omitempty"`
}

// done is how many events the import has been through.
func (r calendarImportResult) done() int { return r.Added + r.Existing + r.Failed }

// fail counts an event the calendar couldn't take, keeping the first reason.
func (r *calendarImportResult) fail(resource *ical.Calendar, err error) {
	r.Failed++
	if r.Reason != "" {
		return
	}
	var problem calendarTransferProblem
	var provider calendarCreateProviderError
	why := "it couldn't be read"
	switch {
	case errors.As(err, &problem):
		why = strings.TrimSuffix(problem.Error(), ".")
	case errors.As(err, &provider):
		why = fmt.Sprintf("the calendar refused it (HTTP %d)", provider.Status)
	}
	summary := ""
	for _, event := range resource.Events() {
		if summary, _ = event.Props.Text(ical.PropSummary); summary != "" {
			break
		}
	}
	if summary = strings.TrimSpace(summary); summary == "" {
		summary = "An event"
	} else {
		summary = "“" + summary + "”"
	}
	r.Reason = summary + ": " + why + "."
}

// calendarImportMadeUID is a UID for an event the file gave none: one made
// from what the event says, so importing the file again finds it.
func calendarImportMadeUID(event *ical.Component) string {
	var lines []string
	for name, props := range event.Props {
		switch name {
		case ical.PropDateTimeStamp, ical.PropCreated, ical.PropLastModified, ical.PropSequence:
			continue
		}
		for _, prop := range props {
			var params []string
			for param, values := range prop.Params {
				params = append(params, param+"="+strings.Join(values, ","))
			}
			sort.Strings(params)
			lines = append(lines, name+";"+strings.Join(params, ";")+":"+prop.Value)
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "gofer-import-" + hex.EncodeToString(sum[:16])
}

// calendarImportResources reads an .ics file into one calendar resource per
// event UID, each with the time zones it uses. A recurring event's changed
// dates share its UID, so they stay in its resource.
func calendarImportResources(data []byte) (resources []*ical.Calendar, err error) {
	defer func() {
		if recover() != nil {
			resources, err = nil, calendarTransferProblem("The file could not be read as an iCalendar (.ics) file.")
		}
	}()
	decoder := ical.NewDecoder(bytes.NewReader(data))
	zones := map[string]*ical.Component{}
	groups := map[string][]*ical.Component{}
	var order []string
	read := 0
	for {
		cal, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, calendarTransferProblem("The file could not be read as an iCalendar (.ics) file.")
		}
		read++
		for _, child := range cal.Children {
			switch child.Name {
			case ical.CompTimezone:
				if zone, _ := child.Props.Text("TZID"); zone != "" && zones[zone] == nil {
					zones[zone] = child
				}
			case ical.CompEvent:
				uid, _ := child.Props.Text("UID")
				if uid = strings.TrimSpace(uid); uid == "" {
					uid = calendarImportMadeUID(child)
					child.Props.SetText("UID", uid)
				}
				if groups[uid] == nil {
					order = append(order, uid)
				}
				groups[uid] = append(groups[uid], child)
			}
		}
	}
	if read == 0 {
		return nil, calendarTransferProblem("The file could not be read as an iCalendar (.ics) file.")
	}
	if len(order) == 0 {
		return nil, calendarTransferProblem("The file has no events.")
	}
	if len(order) > calendarImportMaxEvents {
		return nil, calendarTransferProblem(fmt.Sprintf("The file has more than %d events. Split it into smaller files.", calendarImportMaxEvents))
	}
	for _, uid := range order {
		resource := ical.NewCalendar()
		resource.Props.SetText(ical.PropProductID, "-//Gofer//Calendar//EN")
		resource.Props.SetText(ical.PropVersion, "2.0")
		used := map[string]bool{}
		var events []*ical.Component
		for _, event := range groups[uid] {
			event = cloneCalendarComponent(event)
			calendarImportWithoutScheduling(event)
			for _, props := range event.Props {
				for _, prop := range props {
					if zone := prop.Params.Get("TZID"); zone != "" && !used[zone] {
						used[zone] = true
						if component := zones[zone]; component != nil {
							resource.Children = append(resource.Children, cloneCalendarComponent(component))
						}
					}
				}
			}
			events = append(events, event)
		}
		resource.Children = append(resource.Children, events...)
		calendarInvitationZones(resource)
		resources = append(resources, resource)
	}
	return resources, nil
}

// calendarImportWithoutScheduling asks the server not to send invitations or
// updates for an imported event's organizer and guests (RFC 6638): importing
// a meeting must not email everyone in it.
func calendarImportWithoutScheduling(event *ical.Component) {
	for _, name := range []string{ical.PropOrganizer, ical.PropAttendee} {
		props := event.Props[name]
		for i := range props {
			for param := range props[i].Params {
				if strings.HasPrefix(strings.ToUpper(param), "SCHEDULE-") {
					delete(props[i].Params, param)
				}
			}
			props[i].Params.Set("SCHEDULE-AGENT", "CLIENT")
		}
	}
}

// importCalDAVEvents adds each resource to the calendar as a new event,
// unless existing, the UIDs the calendar has, names it; nil existing leaves
// that to the server, which refuses a UID it has. It stops at a failure
// that isn't about the one event, returning what it did. progress, when
// set, is told the counts after each event.
func importCalDAVEvents(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, resources []*ical.Calendar, existing map[string]bool, progress func(calendarImportResult)) (calendarImportResult, error) {
	var result calendarImportResult
	report := func() {
		if progress != nil {
			progress(result)
		}
	}
	if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
		return result, err
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if existing[calendarImportUID(resource)] {
			result.Existing++
			report()
			continue
		}
		body, err := encodeCalendarReply(resource)
		if err != nil {
			result.fail(resource, err)
			report()
			continue
		}
		endpoint := strings.TrimRight(source.RemoteID, "/") + "/" + uuid.NewString() + ".ics"
		if _, err := calDAVResponseEndpoint(source, endpoint); err != nil {
			return result, err
		}
		req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, credentials.username, credentials.password, strings.NewReader(body))
		if err != nil {
			return result, err
		}
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		req.Header.Set("If-None-Match", "*")
		response, err := calendarProviderDo(client, req)
		if err != nil {
			return result, err
		}
		response.Body.Close()
		switch status := response.StatusCode; {
		case status >= 200 && status < 300:
			result.Added++
		case status == http.StatusUnauthorized || status == http.StatusTooManyRequests || status >= 500:
			return result, calendarCreateProviderError{status}
		case status == http.StatusPreconditionFailed:
			// An event the calendar has under this UID (JMAP calendars
			// answer so).
			result.Existing++
		case existing != nil:
			// Not one of the calendar's UIDs, so it failed.
			result.fail(resource, calendarCreateProviderError{status})
		default:
			// Servers refuse an event whose UID the calendar already has
			// in different ways; one that isn't there failed.
			has, err := calDAVCalendarHasUID(ctx, source, credentials, resource)
			var provider calendarCreateProviderError
			if errors.As(err, &provider) && calendarImportAborts(err) {
				return result, err
			}
			if has {
				result.Existing++
			} else {
				result.fail(resource, calendarCreateProviderError{status})
			}
		}
		report()
	}
	return result, nil
}

// exportCalDAVEvents reads every event in the calendar into one .ics file.
func exportCalDAVEvents(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) ([]byte, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><d:getetag/><c:calendar-data/></d:prop>
 <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter>
</c:calendar-query>`
	multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, credentials.username, credentials.password, "1", body, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, response := range multi.Responses {
		for _, stat := range response.PropStats {
			if strings.Contains(stat.Status, " 200 ") && stat.Prop.CalendarData != "" {
				files = append(files, stat.Prop.CalendarData)
			}
		}
	}
	return calendarExportFile(source.Name, files)
}

// calendarExportFile puts the events of the iCalendar files in one .ics
// file named name, each time zone they use once.
func calendarExportFile(name string, files []string) ([]byte, error) {
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropProductID, "-//Gofer//Calendar//EN")
	out.Props.SetText(ical.PropVersion, "2.0")
	if name := strings.TrimSpace(name); name != "" {
		prop := ical.NewProp("X-WR-CALNAME")
		prop.SetText(name)
		prop.Params.Del(ical.ParamValue)
		out.Props.Set(prop)
	}
	zones := map[string]bool{}
	var events []*ical.Component
	for _, data := range files {
		if strings.TrimSpace(data) == "" {
			continue
		}
		decoder := ical.NewDecoder(strings.NewReader(data))
		for {
			cal, err := decoder.Decode()
			if err != nil {
				break
			}
			for _, child := range cal.Children {
				switch child.Name {
				case ical.CompTimezone:
					if zone, _ := child.Props.Text("TZID"); zone != "" && !zones[zone] {
						zones[zone] = true
						out.Children = append(out.Children, child)
					}
				case ical.CompEvent:
					events = append(events, child)
				}
			}
		}
	}
	out.Children = append(out.Children, events...)
	var buffer bytes.Buffer
	if len(events) == 0 {
		// go-ical refuses a calendar with no components; an empty
		// calendar is still a valid file.
		buffer.WriteString("BEGIN:VCALENDAR\r\nPRODID:-//Gofer//Calendar//EN\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")
		return buffer.Bytes(), nil
	}
	if err := ical.NewEncoder(&buffer).Encode(out); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// calendarExportFilename is the calendar's name as a file name.
func calendarExportFilename(name string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return -1
	}, name)
	if clean = strings.Trim(strings.TrimSpace(clean), "."); clean == "" {
		clean = "calendar"
	}
	return clean + ".ics"
}

// transferCalendarSource runs work on one of the owner's synced calendars
// with its sign-in. write locks the calendar against a sync
// meanwhile, and asks for a sync after.
func (h *Handler) transferCalendarSource(ctx context.Context, owner, sourceID string, write bool, work func(context.Context, storage.CalendarSource, calendarCredentials) error) error {
	check := func(source storage.CalendarSource) error {
		if !calendarTransferProvider(source.Provider) {
			return calendarTransferProblem("This calendar can't be imported into or exported.")
		}
		if write && !calendarSourceWritable(source) {
			return calendarTransferProblem("This calendar is read-only.")
		}
		return nil
	}
	if h.userStorage == nil {
		sources, err := h.db.ListSelectedCalendarSources(ctx, owner)
		if err != nil {
			return err
		}
		var source storage.CalendarSource
		for _, candidate := range sources {
			if candidate.ID == sourceID {
				source = candidate
			}
		}
		if source.ID == "" {
			return sql.ErrNoRows
		}
		if err := check(source); err != nil {
			return err
		}
		if write {
			if !h.calendarWriteAuthorized(ctx, source) {
				return calendarTransferProblem("This account can't write to its calendar. Check it in Accounts.")
			}
			unlock, err := h.lockCalendarCreate(ctx, source)
			if err != nil {
				return err
			}
			defer unlock()
		}
		credentials := h.calendarUpdateCredentials(ctx, source)
		if credentials.err != nil {
			return credentials.err
		}
		err = work(ctx, source, credentials)
		if write {
			if err := h.db.ScheduleCalendarSync(context.WithoutCancel(ctx), owner, source.ID, time.Now()); err != nil {
				log.Printf("calendar import: schedule sync %s: %v", source.ID, err)
			}
		}
		return err
	}
	p := &userCalendarRequest{h: h}
	var err error
	if p.source, err = h.userAccounts.SnapshotCalendarSource(ctx, owner, sourceID); err != nil {
		return err
	}
	source := p.source.Source()
	if err := check(source); err != nil {
		return err
	}
	if write && !h.userCalendarWriteAuthorized(ctx, source) {
		return calendarTransferProblem("This account can't write to its calendar. Check it in Accounts.")
	}
	err = h.userIMAP.RunAccountService(ctx, owner, source.AccountID, mail.AccountServiceCalendar, calendarTransferTimeout, func(operation context.Context) error {
		if write {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
		}
		action, err := p.actionContext(operation, write)
		if err != nil {
			return err
		}
		credentials, err := p.actionCredentials()
		if err != nil {
			return err
		}
		return work(action, source, credentials)
	})
	if write {
		h.wakeUserCalendarAccount(context.WithoutCancel(ctx), owner, source.AccountID)
	}
	return err
}

// calendarTransferError answers a failed import or export.
func calendarTransferError(w http.ResponseWriter, r *http.Request, err error, failed string) {
	var problem calendarTransferProblem
	switch {
	case errors.As(err, &problem):
		http.Error(w, problem.Error(), http.StatusBadRequest)
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, storage.ErrCalendarSourceChanged):
		http.Error(w, "This calendar is no longer synced. Refresh the page.", http.StatusNotFound)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner):
		userAccountError(w, r, err)
	default:
		log.Printf("calendar %s: %v", r.PathValue("id"), err)
		http.Error(w, failed, http.StatusBadGateway)
	}
}

// readCalendarImport reads the .ics file in the request body, answering the
// request itself when it can't.
func readCalendarImport(w http.ResponseWriter, r *http.Request) ([]*ical.Calendar, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, calendarImportMaxSize))
	if err != nil {
		http.Error(w, "The file is larger than 10 MB. Split it into smaller files.", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	resources, err := calendarImportResources(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return resources, true
}

// handleCalendarSourceImport adds the events of the .ics file in the request
// body to a synced calendar, and answers how many were added and
// skipped.
func (h *Handler) handleCalendarSourceImport(w http.ResponseWriter, r *http.Request) {
	resources, ok := readCalendarImport(w, r)
	if !ok {
		return
	}
	h.importIntoCalendarSource(w, r, strings.TrimSpace(r.PathValue("id")), resources, false)
}

// handleCalendarImport imports the .ics file in the request body: into the
// synced calendar ?source names, or into a new calendar named ?name on the
// CalDAV account ?account. The file is read before the calendar is made, so
// a file that can't be imported leaves no empty calendar behind.
func (h *Handler) handleCalendarImport(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	sourceID, accountID := strings.TrimSpace(query.Get("source")), strings.TrimSpace(query.Get("account"))
	name := strings.TrimSpace(query.Get("name"))
	if (sourceID == "") == (accountID == "") {
		http.Error(w, "Choose a calendar to import into.", http.StatusBadRequest)
		return
	}
	if accountID != "" {
		if name == "" {
			http.Error(w, "Enter a name for the new calendar.", http.StatusBadRequest)
			return
		}
		if err := checkCollectionName(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if h.userStorage == nil && !h.requireOwnedAccount(w, r, accountID) {
			return
		}
	}
	resources, ok := readCalendarImport(w, r)
	if !ok {
		return
	}
	if accountID != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cardDAVDiscoveryOverallTimeout)
		created, err := h.createCalendarForImport(ctx, r, h.userID(r.Context()), accountID, name)
		cancel()
		if err != nil {
			calendarTransferError(w, r, err, "The new calendar couldn't be created. Try again.")
			return
		}
		sourceID = created
	}
	h.importIntoCalendarSource(w, r, sourceID, resources, accountID != "")
}

// importIntoCalendarSource adds resources to a synced calendar. Once it has
// signed in, it streams its progress as JSON lines, {"done":n,"total":m},
// one per event, and ends with the result, or with {"error":"…"} when it
// stops; a failure before that is an ordinary error answer. created says the
// calendar was just made for them.
func (h *Handler) importIntoCalendarSource(w http.ResponseWriter, r *http.Request, sourceID string, resources []*ical.Calendar, created bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarTransferTimeout)
	defer cancel()
	controller := http.NewResponseController(w)
	encoder := json.NewEncoder(w)
	streaming := false
	line := func(value any) {
		if !streaming {
			streaming = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusOK)
		}
		// The browser may have gone; the import carries on regardless.
		_ = encoder.Encode(value)
		_ = controller.Flush()
	}
	type progressLine struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	}
	var result calendarImportResult
	err := h.transferCalendarSource(ctx, h.userID(r.Context()), sourceID, true, func(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) error {
		result.Calendar = source.Name
		line(progressLine{Total: len(resources)})
		name := source.Name
		var err error
		result, err = importCalendarEvents(ctx, source, credentials, resources, func(counts calendarImportResult) {
			line(progressLine{Done: counts.done(), Total: len(resources)})
		})
		result.Calendar = name
		return err
	})
	result.Created = created
	if err != nil {
		failed := "The calendar server didn't accept the events. Try again."
		if result.Added > 0 {
			failed = fmt.Sprintf("The calendar server stopped accepting events after %d of %d. Importing the file again into the same calendar skips the ones already added.", result.Added, len(resources))
		}
		if created {
			failed += " The new calendar was created and is in the sidebar."
		}
		if !streaming {
			calendarTransferError(w, r, err, failed)
			return
		}
		log.Printf("calendar import %s: %v", sourceID, err)
		var problem calendarTransferProblem
		if errors.As(err, &problem) {
			failed = problem.Error()
		}
		line(map[string]string{"error": failed})
		return
	}
	if !streaming {
		w.Header().Set("Content-Type", "application/json")
		_ = encoder.Encode(result)
		return
	}
	line(result)
}

// createCalendarForImport makes a calendar named name on an account that
// syncs calendars, signing in as its calendar settings say, through the same
// discovery the account's Calendar settings use. It returns the new calendar's id, synced.
func (h *Handler) createCalendarForImport(ctx context.Context, r *http.Request, owner, accountID, name string) (string, error) {
	var data *models.EditAccountData
	var err error
	if h.userStorage == nil {
		data, err = h.accountStore.GetEditData(ctx, accountID)
	} else {
		data, err = h.userEditData(ctx, owner, accountID)
	}
	if err != nil {
		return "", err
	}
	form := url.Values{}
	switch {
	case data.Provider == providers.ProviderGmail || data.Provider == providers.ProviderOutlook:
		// Their own calendars; discovery needs nothing more.
	case data.Provider == providers.ProviderJMAP && data.CalendarSync.Protocol != storage.CalendarSourceProviderCalDAV:
		form.Set("calendar_protocol", storage.CalendarSourceProviderJMAP)
	case strings.TrimSpace(data.CalendarSync.CalDAVBaseURL) != "":
		form.Set("caldav_url", data.CalendarSync.CalDAVBaseURL)
		if data.CalendarSync.CalDAVUseAccountCredentials {
			form.Set("use_account_credentials", "1")
		}
	default:
		return "", calendarTransferProblem("This account doesn't sync calendars. Set up its calendars in Accounts first.")
	}
	create := &davCreate{Name: name}
	if h.userStorage == nil {
		request := r.Clone(ctx)
		request.Form, request.PostForm = form, form
		found := -1
		message, failed, _ := h.discoverAccountCalendars(ctx, discardResponse{}, request, data, owner, accountID, create, &found)
		if failed {
			return "", calendarTransferProblem(message)
		}
	} else {
		result, err := h.runUserCalendarDiscovery(ctx, owner, accountID, form, create, false)
		if err != nil {
			return "", err
		}
		if result.failed {
			return "", calendarTransferProblem(result.message)
		}
	}
	if create.Created == "" {
		return "", calendarTransferProblem("The calendar server didn't confirm the new calendar.")
	}
	// Discovery ticks the new calendar; find it, and tick it if it didn't.
	find := func(db *storage.DB) (string, error) {
		for _, list := range []func(context.Context, string) ([]storage.CalendarSource, error){db.ListSelectedCalendarSources, db.ListUnselectedCalendarSources} {
			sources, err := list(ctx, owner)
			if err != nil {
				return "", err
			}
			for _, source := range sources {
				if source.AccountID == accountID && davSameCollection(source.RemoteID, create.Created) {
					return source.ID, nil
				}
			}
		}
		return "", calendarTransferProblem("The new calendar was created, but Gofer couldn't find it. Refresh the page and import into it.")
	}
	var id string
	var ids []string
	selected := false
	if h.userStorage == nil {
		if id, err = find(h.db); err != nil {
			return "", err
		}
		if _, selectedIDs, err := calendarSourceSelection(ctx, h.db, owner, id, true); err == nil {
			ids = selectedIDs
		} else {
			selected = true // Already synced.
		}
		if !selected {
			return id, h.db.SetCalendarSourceSelection(ctx, owner, accountID, ids)
		}
		return id, nil
	}
	err = h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		if id, err = find(db); err != nil {
			return err
		}
		if _, ids, err = calendarSourceSelection(ctx, db, owner, id, true); err != nil {
			selected = true
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !selected {
		if err := h.userAccounts.SetCalendarSourcesSelected(ctx, owner, accountID, ids); err != nil {
			return "", err
		}
	}
	return id, nil
}

// discardResponse is a ResponseWriter for discovery run on Gofer's own
// behalf, which may set headers nobody reads.
type discardResponse struct{}

func (discardResponse) Header() http.Header         { return http.Header{} }
func (discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (discardResponse) WriteHeader(int)             {}

// handleCalendarSourceExport downloads a synced calendar's events as
// an .ics file.
func (h *Handler) handleCalendarSourceExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), calendarTransferTimeout)
	defer cancel()
	var data []byte
	var name string
	err := h.transferCalendarSource(ctx, h.userID(r.Context()), strings.TrimSpace(r.PathValue("id")), false, func(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) error {
		var err error
		name = source.Name
		data, err = exportCalendarEvents(ctx, source, credentials)
		return err
	})
	if err != nil {
		calendarTransferError(w, r, err, "The calendar server didn't send the calendar. Try again.")
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": calendarExportFilename(name)}))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// calDAVCalendarUIDs are the UIDs of a CalDAV calendar's events, read in
// one request with nothing else of them.
func calDAVCalendarUIDs(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) (map[string]bool, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><c:calendar-data><c:comp name="VCALENDAR"><c:comp name="VEVENT"><c:prop name="UID"/></c:comp></c:comp></c:calendar-data></d:prop>
 <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter>
</c:calendar-query>`
	multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, credentials.username, credentials.password, "1", body, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	uids := map[string]bool{}
	for _, response := range multi.Responses {
		for _, stat := range response.PropStats {
			if !strings.Contains(stat.Status, " 200 ") || stat.Prop.CalendarData == "" {
				continue
			}
			cal, err := calendarUpdateDecodeICS([]byte(stat.Prop.CalendarData))
			if err != nil {
				return nil, fmt.Errorf("read event UID: %w", err)
			}
			for _, event := range cal.Events() {
				if uid, _ := event.Props.Text(ical.PropUID); uid != "" {
					uids[uid] = true
				}
			}
		}
	}
	return uids, nil
}

// calDAVCalendarHasUID says whether a CalDAV calendar has an event with the
// resource's UID. A JMAP calendar answers a create with an event it has
// itself, so it is never asked.
func calDAVCalendarHasUID(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, resource *ical.Calendar) (bool, error) {
	if source.Provider != storage.CalendarSourceProviderCalDAV || len(resource.Events()) == 0 {
		return false, nil
	}
	uid, _ := resource.Events()[0].Props.Text(ical.PropUID)
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(uid))
	body := `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><c:calendar-data><c:comp name="VCALENDAR"><c:comp name="VEVENT"><c:prop name="UID"/></c:comp></c:comp></c:calendar-data></d:prop>
 <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:prop-filter name="UID"><c:text-match collation="i;octet">` + escaped.String() + `</c:text-match></c:prop-filter></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`
	multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, credentials.username, credentials.password, "1", body, 30*time.Second)
	if err != nil {
		return false, err
	}
	// A text-match finds the UID inside longer ones too.
	for _, response := range multi.Responses {
		for _, stat := range response.PropStats {
			cal, err := calendarUpdateDecodeICS([]byte(stat.Prop.CalendarData))
			if err != nil {
				continue
			}
			for _, event := range cal.Events() {
				if found, _ := event.Props.Text(ical.PropUID); found == uid {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
