package handler

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

func TestCalendarCalDAVOccurrencePreservesResource(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, mode := range []string{"timed", "first", "all-day", "dst", "existing-override", "stale", "report-race", "missing-member", "wrong-uid", "range", "guests", "race", "redirect", "unconfirmed", "changed-series"} {
			t.Run(fmt.Sprintf("delete=%v/%s", deleting, mode), func(t *testing.T) {
				masterDates := "DTSTART:20261002T070000Z\nDTEND:20261002T080000Z"
				instanceDates := "RECURRENCE-ID:20261009T070000Z\nDTSTART:20261009T070000Z\nDTEND:20261009T080000Z"
				if mode == "first" {
					instanceDates = masterDates
				}
				siblingDates := "RECURRENCE-ID:20261016T070000Z\nDTSTART:20261016T110000Z\nDTEND:20261016T120000Z"
				if mode == "all-day" {
					masterDates = "DTSTART;VALUE=DATE:20261002\nDTEND;VALUE=DATE:20261004"
					instanceDates = "RECURRENCE-ID;VALUE=DATE:20261009\nDTSTART;VALUE=DATE:20261009\nDTEND;VALUE=DATE:20261011"
					siblingDates = "RECURRENCE-ID;VALUE=DATE:20261016\nDTSTART;VALUE=DATE:20261017\nDTEND;VALUE=DATE:20261019"
				}
				if mode == "dst" {
					masterDates = "DTSTART;TZID=Europe/Prague:20261002T090000\nDTEND;TZID=Europe/Prague:20261002T100000"
					instanceDates = "RECURRENCE-ID:20261030T080000Z\nDTSTART:20261030T080000Z\nDTEND:20261030T090000Z"
					siblingDates = "RECURRENCE-ID;TZID=Europe/Prague:20261016T090000\nDTSTART;TZID=Europe/Prague:20261016T130000\nDTEND;TZID=Europe/Prague:20261016T140000"
				}
				wrap := func(events string) string {
					return "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Fixture//EN\n" + events + "END:VCALENDAR\n"
				}
				component := func(props string) string {
					return "BEGIN:VEVENT\nUID:series-uid\nDTSTAMP:20260901T000000Z\n" + props + "\nEND:VEVENT\n"
				}
				alarm := "\nBEGIN:VALARM\nACTION:DISPLAY\nTRIGGER:-PT15M\nDESCRIPTION:Reminder\nEND:VALARM"
				original := wrap(component(masterDates+"\nRRULE:FREQ=WEEKLY\nSUMMARY:Series\nDESCRIPTION:Rich notes\nX-KEEP:master"+alarm) + component(siblingDates+"\nSUMMARY:Other exception\nX-KEEP:sibling"))
				expanded := wrap(component(instanceDates + "\nSUMMARY:Selected\nDESCRIPTION:Rich notes"))
				if mode == "existing-override" {
					// The recurrence key stays on the original day even after a move.
					instanceDates = strings.ReplaceAll(instanceDates, "DTSTART:20261009T070000Z", "DTSTART:20261010T110000Z")
					instanceDates = strings.ReplaceAll(instanceDates, "DTEND:20261009T080000Z", "DTEND:20261010T120000Z")
					original = strings.Replace(original, "END:VCALENDAR", component(instanceDates+"\nSUMMARY:Selected\nDESCRIPTION:Rich notes\nX-KEEP:selected"+alarm)+"END:VCALENDAR", 1)
					expanded = wrap(component(instanceDates + "\nSUMMARY:Selected\nDESCRIPTION:Rich notes"))
				}
				if mode == "wrong-uid" {
					original = strings.Replace(original, "UID:series-uid", "UID:foreign", 1)
				}
				if mode == "range" {
					original = strings.Replace(original, "RECURRENCE-ID:", "RECURRENCE-ID;RANGE=THISANDFUTURE:", 1)
				}
				if mode == "guests" {
					original = strings.Replace(original, "SUMMARY:Series", "SUMMARY:Series\nATTENDEE:mailto:guest@example.com", 1)
				}
				storedBody, writes, reads, reports := original, 0, 0, 0
				var existing storage.CalendarEvent
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					user, pass, ok := r.BasicAuth()
					if !ok || user != "user" || pass != "password" || r.URL.Fragment != "" {
						t.Error("incorrect credentials or fragment sent")
					}
					switch r.Method {
					case http.MethodGet:
						reads++
						if r.URL.Path != "/cal/series.ics" {
							t.Error("GET did not target the owned series resource")
						}
						etag := `"v1"`
						if mode == "stale" || writes > 0 {
							etag = `"v2"`
						}
						w.Header().Set("ETag", etag)
						if mode == "unconfirmed" && writes > 0 {
							_, _ = io.WriteString(w, "invalid")
							return
						}
						body := storedBody
						if mode == "changed-series" && writes > 0 {
							body = strings.Replace(body, "FREQ=WEEKLY", "FREQ=DAILY", 1)
						}
						_, _ = io.WriteString(w, body)
					case "REPORT":
						reports++
						body, _ := io.ReadAll(r.Body)
						if r.URL.Path != "/cal/" || !strings.Contains(string(body), "calendar-multiget") || !strings.Contains(string(body), "<c:expand start=") || !strings.Contains(string(body), "<d:href>/cal/series.ics</d:href>") {
							t.Errorf("unsafe membership report: %s", body)
						}
						var escaped bytes.Buffer
						data := expanded
						if mode == "missing-member" {
							data = wrap(component(siblingDates + "\nSUMMARY:Other exception"))
						}
						_ = xml.EscapeText(&escaped, []byte(data))
						etag := "&quot;v1&quot;"
						if mode == "report-race" {
							etag = "&quot;newer&quot;"
						}
						w.WriteHeader(207)
						_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/cal/series.ics</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, etag, escaped.String())
					case http.MethodPut:
						writes++
						if r.URL.Path != "/cal/series.ics" || r.Header.Get("If-Match") != `"v1"` || reads != 1 || reports != 1 {
							t.Error("PUT missing resource identity, conditional version or membership preflight")
						}
						if mode == "race" {
							w.WriteHeader(412)
							return
						}
						if mode == "redirect" {
							w.Header().Set("Location", "/cal/other.ics")
							w.WriteHeader(307)
							return
						}
						body, _ := io.ReadAll(r.Body)
						cal, err := calendarUpdateDecodeICS(body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						master, selected, rid, err := calendarCalDAVOccurrenceResource(cal, http.Header{}, existing)
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						if master.Props.Get("RRULE").Value != "FREQ=WEEKLY" || master.Props.Get("X-KEEP").Value != "master" || len(master.Children) != 1 {
							t.Error("master or its alarm changed")
						}
						if deleting {
							if selected != nil || len(master.Props["EXDATE"]) != 1 {
								t.Error("deletion did not exclude only the original occurrence")
							}
							key, _ := calendarRecurrenceKey(master.Props.Get("EXDATE"), time.UTC)
							want, _ := calendarRecurrenceKey(rid, time.UTC)
							if key != want {
								t.Error("deleted moved start instead of original recurrence identity")
							}
						} else {
							if selected == nil || selected.Props.Get("RRULE") != nil || len(selected.Children) != 1 {
								t.Error("override did not preserve its alarm or retained the series rule")
							}
							if mode == "dst" && (selected.Props.Get("RECURRENCE-ID").Value != "20261030T090000" || selected.Props.Get("RECURRENCE-ID").Params.Get("TZID") != "Europe/Prague") {
								t.Error("recurrence identity lost local DST semantics")
							}
							if mode == "existing-override" && selected.Props.Get("X-KEEP").Value != "selected" {
								t.Error("existing override metadata lost")
							}
						}
						if !strings.Contains(string(body), "X-KEEP:sibling") || !strings.Contains(string(body), "SUMMARY:Other exception") {
							t.Error("other exception lost")
						}
						storedBody = string(body)
						w.Header().Set("ETag", `"v2"`)
						w.WriteHeader(204)
					default:
						t.Errorf("unexpected method %s; occurrence deletion must never DELETE the resource", r.Method)
						w.WriteHeader(405)
					}
				}))
				defer server.Close()
				old := calDAVHTTPTransport
				calDAVHTTPTransport = server.Client().Transport
				defer func() { calDAVHTTPTransport = old }()
				source := storage.CalendarSource{RemoteID: server.URL + "/cal/", TimeZone: "Europe/Prague"}
				events, err := parseCalDAVEvents(server.URL+"/cal/series.ics", `"v1"`, expanded, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				existing = calendarStorageEvent("owner", "source", events[0])
				if mode == "first" {
					existing.SeriesRemoteID = existing.RemoteID
					existing.RemoteID += "#recurrence=2026-10-02T07%3A00%3A00Z"
					existing.RecurrenceJSON = `["RECURRENCE-ID:2026-10-02T07:00:00Z"]`
				}
				draft := calendarProviderDraft(t, mode == "all-day")
				draft.Description = "Rich notes"
				var edit *calendar.EventDraft
				if !deleting {
					edit = &draft
				}
				remote, err := updateCalDAVCalendarOccurrence(t.Context(), source, "user", "password", existing, edit)
				switch mode {
				case "timed", "first", "all-day", "dst", "existing-override":
					if err != nil || writes != 1 || reads != 2 || remote.RemoteID != existing.RemoteID || remote.SeriesRemoteID != existing.SeriesRemoteID {
						t.Fatalf("mutation failed: writes=%d reads=%d remote=%+v err=%v", writes, reads, remote, err)
					}
					if deleting != remote.Deleted {
						t.Fatal("incorrect deletion confirmation")
					}
				case "race", "stale", "report-race", "missing-member":
					if !errors.Is(err, errCalendarUpdateConflict) || mode != "race" && writes != 0 {
						t.Fatalf("unsafe conflict: writes=%d err=%v", writes, err)
					}
				case "wrong-uid", "range", "guests":
					if !errors.Is(err, errCalendarUpdateUnsupported) || writes != 0 {
						t.Fatalf("unsafe shape: writes=%d err=%v", writes, err)
					}
				default:
					if err == nil || writes != 1 {
						t.Fatalf("accepted unconfirmed mutation: %v", err)
					}
				}
			})
		}
	}
}

func TestCalendarCalDAVSeriesDeletionAfterOneOffChanges(t *testing.T) {
	draft := calendarProviderDraft(t, false)
	draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 1}
	raw, err := calendarCreateICS(draft)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendarUpdateDecodeICS([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	master := cal.Events()[0]
	exdate := *master.Props.Get("DTSTART")
	exdate.Name = "EXDATE"
	master.Props.Add(&exdate)
	override := cloneCalendarComponent(master.Component)
	delete(override.Props, "RRULE")
	delete(override.Props, "EXDATE")
	rid := *override.Props.Get("DTSTART")
	rid.Name = "RECURRENCE-ID"
	override.Props.Set(&rid)
	cal.Children = append(cal.Children, override)
	var body bytes.Buffer
	if err := ical.NewEncoder(&body).Encode(cal); err != nil {
		t.Fatal(err)
	}
	writes := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write(body.Bytes())
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/cal/series.ics" || r.Header.Get("If-Match") != `"v1"` {
			t.Error("incorrect whole-series deletion")
		}
		writes++
		w.WriteHeader(204)
	}))
	defer server.Close()
	old := calDAVHTTPTransport
	calDAVHTTPTransport = server.Client().Transport
	defer func() { calDAVHTTPTransport = old }()
	endpoint := server.URL + "/cal/series.ics"
	headers := http.Header{"Etag": []string{`"v1"`}}
	remote, err := calendarCalDAVDeleteSeriesEvent(cal, headers, endpoint, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarCalDAVSeriesEvent(cal, headers, endpoint, time.UTC); err == nil {
		t.Fatal("series editing must not silently discard exceptions")
	}
	if err := deleteCalDAVCalendarEventScope(t.Context(), storage.CalendarSource{RemoteID: server.URL + "/cal/"}, "user", "pass", calendarStorageEvent("owner", "source", remote), true); err != nil || writes != 1 {
		t.Fatalf("delete after one-off changes: writes=%d err=%v", writes, err)
	}
}
