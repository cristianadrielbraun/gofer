package handler

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const calDAVTaskStored = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VTODO\r\nUID:water-plants\r\nDTSTAMP:20260101T000000Z\r\nDUE:20261205T090000Z\r\nSUMMARY:Water the plants\r\nEND:VTODO\r\n" +
	"END:VCALENDAR\r\n"

// Task lists are calendar collections too; only those that can hold events
// are offered as calendars.
func TestCalDAVDiscoveryLeavesOutTaskLists(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "supported-calendar-component-set") {
			t.Errorf("calendar list didn't ask for the supported components: %s", body)
		}
		collection := func(path, name, comps string) string {
			set := ""
			if comps != "" {
				set = `<c:supported-calendar-component-set>` + comps + `</c:supported-calendar-component-set>`
			}
			return `<d:response><d:href>` + path + `</d:href><d:propstat><d:prop><d:displayname>` + name + `</d:displayname><d:resourcetype><d:collection/><c:calendar/></d:resourcetype>` + set + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			collection("/cal/tasks/", "Tasks", `<c:comp name="VTODO"/>`)+
			collection("/cal/home/", "Home", `<c:comp name="VEVENT"/>`)+
			collection("/cal/mixed/", "Mixed", `<c:comp name="VTODO"/><c:comp name="VEVENT"/>`)+
			collection("/cal/journal/", "Journal", `<c:comp name="VJOURNAL"/>`)+
			collection("/cal/any/", "Any", "")+
			`</d:multistatus>`)
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()

	calendars, err := listCalDAVCalendars(davTestContext(t), server.URL+"/cal/", "user", "pass", "owner", "account")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cal := range calendars {
		names = append(names, cal.Name)
	}
	if got := strings.Join(names, ","); got != "Home,Mixed,Any" {
		t.Fatalf("calendars = %s, want Home,Mixed,Any", got)
	}
	if !calendars[0].IsPrimary {
		t.Fatal("the first event calendar should be the primary one")
	}
}

// A server that returns tasks to a query for events doesn't fail the sync:
// the tasks are passed over and the events still arrive.
func TestCalDAVSyncPassesOverTasks(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "expand") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
		for _, resource := range []struct{ href, data string }{{"/cal/task.ics", calDAVTaskStored}, {"/cal/lunch.ics", calDAVLunchStored}} {
			var escaped strings.Builder
			_ = xml.EscapeText(&escaped, []byte(resource.data))
			fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, resource.href, escaped.String())
		}
		fmt.Fprint(w, `</d:multistatus>`)
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/", TimeZone: "UTC"}
	query := calendar.EventQuery{WindowStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}

	page, err := listCalDAVCalendarEvents(davTestContext(t), source, "user", "pass", query)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Summary != "Lunch" {
		t.Fatalf("events = %+v, want the lunch alone", page.Events)
	}

	// Server expansion takes the same way through.
	if _, err := parseCalDAVEvents(server.URL+"/cal/task.ics", `"1"`, calDAVTaskStored, time.UTC); err != errCalDAVNoEvents {
		t.Fatalf("a task read as events: %v", err)
	}
}
