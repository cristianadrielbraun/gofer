package handler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
)

func TestUserCalendarDiscoveryTraversalLimitsAndPrimaryFallback(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider+"/source-limit", func(t *testing.T) {
			fetch := func(_ string, out any) error {
				if provider == "gmail" {
					page := out.(*googleCalendarListResponse)
					for i := 0; i <= calendarDiscoveryMaxSources; i++ {
						page.Items = append(page.Items, googleCalendarListEntry{ID: fmt.Sprint(i)})
					}
				} else {
					page := out.(*outlookCalendarListResponse)
					for i := 0; i <= calendarDiscoveryMaxSources; i++ {
						page.Calendars = append(page.Calendars, outlookCalendar{ID: fmt.Sprint(i)})
					}
				}
				return nil
			}
			var result []calendar.RemoteCalendar
			var err error
			if provider == "gmail" {
				result, err = discoverGoogleCalendarsWithFetch(t.Context(), fetch)
			} else {
				result, err = discoverOutlookCalendarsWithFetch(t.Context(), fetch)
			}
			if err == nil || result != nil || !strings.Contains(err.Error(), "source limit") {
				t.Fatal("unbounded discovery returned results", len(result), err)
			}
		})
		t.Run(provider+"/page-limit", func(t *testing.T) {
			calls := 0
			fetch := func(_ string, out any) error {
				calls++
				if provider == "gmail" {
					out.(*googleCalendarListResponse).NextPageToken = fmt.Sprint(calls)
				} else {
					out.(*outlookCalendarListResponse).NextLink = fmt.Sprintf("https://graph.microsoft.com/v1.0/me/calendars?skip=%d", calls)
				}
				return nil
			}
			var err error
			if provider == "gmail" {
				_, err = discoverGoogleCalendarsWithFetch(t.Context(), fetch)
			} else {
				_, err = discoverOutlookCalendarsWithFetch(t.Context(), fetch)
			}
			if err == nil || calls != calendarDiscoveryMaxPages || !strings.Contains(err.Error(), "page limit") {
				t.Fatal("page traversal not bounded", calls, err)
			}
		})
	}
	for _, values := range [][]calendar.RemoteCalendar{
		{{RemoteID: "first"}, {RemoteID: "second"}},
		{{RemoteID: "first"}, {RemoteID: "second", Primary: true}},
	} {
		sources := calendarDiscoveredSources(values)
		selected := 0
		for _, source := range sources {
			if source.IsSelected {
				selected++
			}
		}
		if selected != 1 || sources[1].IsSelected != values[1].Primary {
			t.Fatal("primary or first-calendar selection changed", sources)
		}
	}
}

func TestUserCalendarDiscoveryPaginationRejectsCredentialAndCollectionChanges(t *testing.T) {
	base := "https://graph.microsoft.com/v1.0/me/calendars"
	for _, next := range []string{"https://foreign.test/v1.0/me/calendars", "http://graph.microsoft.com/v1.0/me/calendars", "https://user:secret@graph.microsoft.com/v1.0/me/calendars", "/v1.0/me/mailFolders", "/v1.0/me/calendars#fragment", "/v1.0/me/%63alendars"} {
		if _, err := calendarDiscoveryPageURL(base, next); err == nil {
			t.Fatal("foreign or ambiguous pagination accepted", next)
		}
	}
	if next, err := calendarDiscoveryPageURL(base, "?$skiptoken=two"); err != nil || next != base+"?$skiptoken=two" {
		t.Fatal("valid relative pagination", next, err)
	}
}
