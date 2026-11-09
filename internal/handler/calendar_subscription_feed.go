package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// A calendar subscription is an iCalendar feed at an address the user
// entered, such as a holiday calendar or one a work calendar publishes. Gofer
// reads all of it each time and keeps the events of the calendar's window, as
// it does for other calendars; nothing is ever written back.

// calendarFeedMaxBytes caps a feed's size. Feeds of busy calendars spanning
// years run to a few megabytes.
const calendarFeedMaxBytes = 20 << 20

const calendarFeedTimeout = 60 * time.Second

var (
	errCalendarFeedURL      = errors.New("Enter the calendar's address, starting with https:// or webcal://.")
	errCalendarFeedNotICS   = errors.New("The address didn't return an iCalendar (.ics) calendar.")
	errCalendarFeedTooLarge = errors.New("The calendar is larger than 20 MB.")
	errCalendarFeedEvents   = errors.New("None of the calendar's events could be read.")
)

// normalizeCalendarFeedURL reads the address of a feed: webcal:// and
// webcals:// are the same feeds over HTTPS.
func normalizeCalendarFeedURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", errCalendarFeedURL
	}
	lower := strings.ToLower(raw)
	for _, prefix := range []string{"webcals://", "webcal://"} {
		if strings.HasPrefix(lower, prefix) {
			raw = "https://" + raw[len(prefix):]
			break
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (!strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http")) {
		return "", errCalendarFeedURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment = ""
	return u.String(), nil
}

// calendarFeedTransport carries feed requests through the same checks as
// CalDAV servers: private addresses only with a private-target exception,
// and plain HTTP only with an HTTP plaintext exception. Each redirect is
// checked again.
type calendarFeedTransport struct{}

func (calendarFeedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch strings.ToLower(req.URL.Scheme) {
	case "https":
	case "http":
		host := req.URL.Hostname()
		port, _ := strconv.Atoi(davURLPort(req.URL))
		if !davPlaintextAllowed(req.Context(), host, port) {
			return nil, fmt.Errorf("Gofer won't read a calendar from %s without encryption; an administrator can add an HTTP plaintext exception for %s:%d under Mail security", req.URL.Host, host, port)
		}
	default:
		return nil, fmt.Errorf("calendar subscriptions don't support %q addresses", req.URL.Scheme)
	}
	// A subscription belongs to no account: its callers mark the request
	// with the default proxy (defaultProxyContext).
	if davRequireProxyChoice.Load() && !netguard.ProxyChosen(req.Context()) {
		return nil, fmt.Errorf("calendar feed request to %s was not given the default proxy", req.URL.Host)
	}
	return davHTTPTransport.RoundTrip(req)
}

var calendarFeedClient = &http.Client{
	Timeout:   calendarFeedTimeout,
	Transport: calendarFeedTransport{},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("the calendar's address redirected too many times")
		}
		return nil
	},
}

// fetchCalendarFeed downloads a feed. Errors leave out the address, which
// may hold a private token.
func fetchCalendarFeed(ctx context.Context, address string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errCalendarFeedURL
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	req.Header.Set("User-Agent", "Gofer")
	resp, err := calendarFeedClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the calendar's server could not be reached: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, calendarFeedHTTPError(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, calendarFeedMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("the calendar could not be downloaded: %v", err)
	}
	if len(data) > calendarFeedMaxBytes {
		return nil, errCalendarFeedTooLarge
	}
	return data, nil
}

// calendarFeedHTTPError is a feed's server refusing to send it.
type calendarFeedHTTPError int

func (e calendarFeedHTTPError) Error() string {
	switch int(e) {
	case http.StatusNotFound, http.StatusGone:
		return "The calendar is no longer at this address."
	case http.StatusUnauthorized, http.StatusForbidden:
		return "The calendar's server refused to send it. Check that the address is complete, including any private key in it."
	}
	return fmt.Sprintf("The calendar's server answered HTTP %d.", int(e))
}

// calendarFeed is a downloaded feed: its calendars, and the name and color
// it suggests for itself.
type calendarFeed struct {
	calendars   []*ical.Calendar
	name, color string
	timezone    string
}

// decodeCalendarFeed reads a feed, which may hold several VCALENDARs.
func decodeCalendarFeed(data []byte) (feed calendarFeed, err error) {
	// go-ical's parameter parser can panic on a truncated parameter.
	defer func() {
		if recover() != nil {
			feed, err = calendarFeed{}, errCalendarFeedNotICS
		}
	}()
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	decoder := ical.NewDecoder(bytes.NewReader(data))
	for {
		cal, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(feed.calendars) > 0 {
				break // Trailing junk after a whole calendar.
			}
			return calendarFeed{}, errCalendarFeedNotICS
		}
		feed.calendars = append(feed.calendars, cal)
		if feed.name == "" {
			feed.name, _ = cal.Props.Text("X-WR-CALNAME")
			if feed.name == "" {
				feed.name, _ = cal.Props.Text("NAME")
			}
		}
		if feed.color == "" {
			feed.color, _ = cal.Props.Text("X-APPLE-CALENDAR-COLOR")
		}
		if feed.timezone == "" {
			feed.timezone, _ = cal.Props.Text("X-WR-TIMEZONE")
		}
	}
	if len(feed.calendars) == 0 {
		return calendarFeed{}, errCalendarFeedNotICS
	}
	feed.name = strings.TrimSpace(feed.name)
	if len([]rune(feed.name)) > 100 {
		feed.name = string([]rune(feed.name)[:100])
	}
	feed.color = calendarFeedColor(feed.color)
	return feed, nil
}

// calendarFeedColor is a feed's suggested color as #rrggbb, or empty.
func calendarFeedColor(color string) string {
	color = strings.ToLower(strings.TrimSpace(color))
	if len(color) == 9 && color[0] == '#' {
		color = color[:7] // #rrggbbaa
	}
	if len(color) != 7 || color[0] != '#' {
		return ""
	}
	for _, r := range color[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return color
}

// calendarFeedEvents reads a feed's events that overlap the window, with
// recurring ones expanded into their dates. An event Gofer can't read is
// left out rather than failing the whole calendar, unless none can be read.
func calendarFeedEvents(feed calendarFeed, start, end time.Time, location *time.Location) ([]calendar.RemoteEvent, error) {
	if feed.timezone != "" {
		if zone, err := time.LoadLocation(calendarFeedZoneName(feed.timezone)); err == nil && zone.String() != "Local" {
			location = zone // For times written without a zone.
		}
	}
	type series struct {
		master  *ical.Event
		changed []ical.Event
	}
	byUID := map[string]*series{}
	var order []string
	total, read, failed := 0, 0, 0
	for _, cal := range feed.calendars {
		for _, component := range cal.Events() {
			total++
			uid, _ := component.Props.Text(ical.PropUID)
			uid = strings.TrimSpace(uid)
			if uid == "" {
				continue
			}
			if calendarFeedTooFrequent(component.Component) {
				failed++
				continue
			}
			calendarFeedFixZones(component.Component)
			entry := byUID[uid]
			if entry == nil {
				entry = &series{}
				byUID[uid] = entry
				order = append(order, uid)
			}
			if component.Props.Get(ical.PropRecurrenceID) != nil {
				entry.changed = append(entry.changed, component)
			} else if entry.master == nil {
				event := component
				entry.master = &event
			}
		}
	}
	var events []calendar.RemoteEvent
	seen := map[string]bool{}
	add := func(occurrences []jmapOccurrence) {
		for _, occurrence := range occurrences {
			if seen[occurrence.remote.RemoteID] {
				continue
			}
			seen[occurrence.remote.RemoteID] = true
			events = append(events, occurrence.remote)
		}
	}
	for _, uid := range order {
		entry := byUID[uid]
		if entry.master != nil {
			occurrences, err := expandCalendarSeries(append([]ical.Event{*entry.master}, entry.changed...), uid, "", start, end, location)
			if err != nil {
				failed++
				continue
			}
			read++
			add(occurrences)
			continue
		}
		// Only some dates of a series, such as an invitation to them.
		for _, component := range entry.changed {
			occurrences, err := expandCalendarSeries([]ical.Event{component}, uid, "", start, end, location)
			if err != nil {
				failed++
				continue
			}
			read++
			add(occurrences)
		}
	}
	if read == 0 && (failed > 0 || total > 0) {
		return nil, errCalendarFeedEvents
	}
	if len(events) > calendarSyncMaxEvents {
		return nil, fmt.Errorf("the calendar has more than %d events in the dates Gofer keeps", calendarSyncMaxEvents)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].RemoteID < events[j].RemoteID })
	return events, nil
}

// calendarFeedFixZones rewrites time zone names Go doesn't know into ones it
// does: Outlook publishes Windows names such as "W. Europe Standard Time",
// and some apps prefix the IANA name, as in "/mozilla.org/20050126_1/Europe/Berlin".
// A name that still can't be read is dropped, so the time is read in the
// calendar's own zone instead of failing.
func calendarFeedFixZones(component *ical.Component) {
	for name, props := range component.Props {
		switch name {
		case ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID, ical.PropExceptionDates, ical.PropRecurrenceDates, ical.PropDue:
		default:
			continue
		}
		for i := range props {
			zone := props[i].Params.Get(ical.ParamTimezoneID)
			if zone == "" {
				continue
			}
			if fixed := calendarFeedZoneName(zone); fixed != "" {
				if location, err := time.LoadLocation(fixed); err == nil && location.String() != "Local" {
					props[i].Params.Set(ical.ParamTimezoneID, fixed)
					continue
				}
			}
			props[i].Params.Del(ical.ParamTimezoneID)
		}
		component.Props[name] = props
	}
}

// calendarFeedTooFrequent reports whether an event repeats every second or
// minute: months of those dates would be millions of events, which no
// published calendar means.
func calendarFeedTooFrequent(component *ical.Component) bool {
	for _, prop := range component.Props[ical.PropRecurrenceRule] {
		value := strings.ToUpper(prop.Value)
		if strings.Contains(value, "FREQ=SECONDLY") || strings.Contains(value, "FREQ=MINUTELY") {
			return true
		}
	}
	return false
}

// calendarFeedZoneName is the IANA name for a feed's time zone name.
func calendarFeedZoneName(zone string) string {
	zone = strings.Trim(strings.TrimSpace(zone), `"`)
	if zone == "" || strings.EqualFold(zone, "Local") {
		return ""
	}
	if _, err := time.LoadLocation(zone); err == nil {
		return zone
	}
	if iana := outlookWindowsZones[zone]; iana != "" {
		return iana
	}
	// "/mozilla.org/20050126_1/Europe/Berlin" and the like: try the last
	// two parts, then the last three ("America/Argentina/Buenos_Aires").
	parts := strings.Split(strings.Trim(zone, "/"), "/")
	for _, n := range []int{2, 3} {
		if len(parts) > n {
			candidate := strings.Join(parts[len(parts)-n:], "/")
			if _, err := time.LoadLocation(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

// listCalendarFeedEvents reads a subscription's events in the query's window.
func listCalendarFeedEvents(ctx context.Context, source storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
	data, err := fetchCalendarFeed(ctx, source.RemoteID)
	if err != nil {
		return calendar.EventPage{}, err
	}
	feed, err := decodeCalendarFeed(data)
	if err != nil {
		return calendar.EventPage{}, err
	}
	location := query.WindowStart.Location()
	events, err := calendarFeedEvents(feed, query.WindowStart, query.WindowEnd, location)
	if err != nil {
		return calendar.EventPage{}, err
	}
	return calendar.EventPage{Events: events}, nil
}
