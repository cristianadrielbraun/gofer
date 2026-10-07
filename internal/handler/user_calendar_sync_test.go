package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
)

func TestUserCalendarEventTraversalRejectsIncompleteAndUnboundedResults(t *testing.T) {
	query := calendar.EventQuery{WindowStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for _, provider := range []string{"gmail", "outlook"} {
		for _, fault := range []string{"page-limit", "partial", "repeated", "canceled", "event-limit"} {
			t.Run(provider+"/"+fault, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				fetch := func(_ string, out any) error {
					calls++
					if fault == "partial" && calls == 2 {
						return fmt.Errorf("synthetic next page failure")
					}
					if fault == "canceled" {
						cancel()
					}
					next := fmt.Sprint(calls)
					if fault == "repeated" {
						next = "same"
					}
					if provider == "gmail" {
						page := out.(*googleCalendarEventsResponse)
						page.NextPageToken = next
						if fault == "event-limit" {
							page.NextPageToken = ""
							for i := 0; i <= calendarSyncMaxEvents; i++ {
								page.Items = append(page.Items, googleCalendarEvent{ID: fmt.Sprint(i), Status: "cancelled"})
							}
						} else {
							page.Items = []googleCalendarEvent{{ID: "item", Status: "cancelled"}}
						}
					} else {
						page := out.(*outlookCalendarEventsResponse)
						page.NextLink = outlookGraphBaseURL + "/me/calendars/primary/calendarView?skip=" + next
						if fault == "event-limit" {
							page.NextLink = ""
							for i := 0; i <= calendarSyncMaxEvents; i++ {
								page.Events = append(page.Events, outlookCalendarEvent{ID: fmt.Sprint(i), IsCancelled: true})
							}
						} else {
							page.Events = []outlookCalendarEvent{{ID: "item", IsCancelled: true}}
						}
					}
					return nil
				}
				var result calendar.EventPage
				var err error
				if provider == "gmail" {
					result, err = listGoogleCalendarEventsWithFetch(ctx, "primary", query, fetch)
				} else {
					result, err = listOutlookCalendarEventsWithFetch(ctx, "primary", query, fetch)
				}
				if err == nil || len(result.Events) != 0 {
					t.Fatal("incomplete traversal returned cacheable events", fault, len(result.Events), err)
				}
				if fault == "page-limit" && (calls != calendarSyncMaxPages || !strings.Contains(err.Error(), "page limit")) {
					t.Fatal("page bound", calls, err)
				}
				if fault == "event-limit" && !strings.Contains(err.Error(), "event limit") {
					t.Fatal("event bound", err)
				}
			})
		}
	}
}

func TestUserCalendarEventPaginationPreservesEncodedCalendarIdentity(t *testing.T) {
	base := "https://graph.microsoft.com/v1.0/me/calendars/opaque%2Fidentifier/calendarView"
	if next, err := calendarDiscoveryPageURL(base, "?$skiptoken=two"); err != nil || next != base+"?$skiptoken=two" {
		t.Fatal("encoded identity lost", next, err)
	}
	for _, next := range []string{"/v1.0/me/calendars/opaque/identifier/calendarView", "/v1.0/me/calendars/other/calendarView", "/v1.0/me/calendars/opaque%2fidentifier/calendarView"} {
		if _, err := calendarDiscoveryPageURL(base, next); err == nil {
			t.Fatal("alternate collection encoding accepted", next)
		}
	}
}
