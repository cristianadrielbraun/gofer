package jmap

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestQueryCalendarEventsSendsTheFilter(t *testing.T) {
	var using []string
	var calls [][]json.RawMessage
	server := contactServer(t, `{"methodResponses": [["CalendarEvent/query", {"accountId": "a1", "ids": ["e1", "e2"], "position": 0, "total": 2}, "0"]], "sessionState": "x"}`, &using, &calls)
	client := &Client{HTTP: server.Client()}
	result, err := client.QueryCalendarEvents(context.Background(), &Session{APIURL: server.URL + "/jmap/"}, "a1",
		CalendarEventFilter{InCalendar: "c1", After: "2026-10-01T00:00:00Z"}, 0, 50)
	if err != nil || !slices.Equal(result.IDs, []string{"e1", "e2"}) {
		t.Fatalf("query = %+v, %v", result, err)
	}
	if !slices.Contains(using, CapabilityCalendars) {
		t.Errorf("using = %v", using)
	}
	var args struct {
		Filter            map[string]string `json:"filter"`
		Limit             int               `json:"limit"`
		ExpandRecurrences *bool             `json:"expandRecurrences"`
	}
	_ = json.Unmarshal(calls[0][1], &args)
	if args.Filter["inCalendar"] != "c1" || args.Filter["after"] != "2026-10-01T00:00:00Z" || args.Filter["before"] != "" || args.Limit != 50 || args.ExpandRecurrences != nil {
		t.Errorf("args = %+v", args)
	}
}

func TestParseCalendarEventsReadsOneOrManyEvents(t *testing.T) {
	var using []string
	var calls [][]json.RawMessage
	server := contactServer(t, `{"methodResponses": [["CalendarEvent/parse", {"accountId": "a1", "parsed": {
	  "b1": [{"@type": "Event", "uid": "one@example.test"}, {"@type": "Event", "uid": "two@example.test"}],
	  "b2": {"@type": "Event", "uid": "three@example.test"}}}, "0"]], "sessionState": "x"}`, &using, &calls)
	client := &Client{HTTP: server.Client()}
	parsed, err := client.ParseCalendarEvents(context.Background(), &Session{APIURL: server.URL + "/jmap/"}, "a1", []string{"b1", "b2"})
	if err != nil || len(parsed["b1"]) != 2 || len(parsed["b2"]) != 1 || parsed["b2"][0]["uid"] != "three@example.test" {
		t.Fatalf("parsed = %+v, %v", parsed, err)
	}
	if !slices.Contains(using, CapabilityCalendarsParse) {
		t.Errorf("using = %v", using)
	}
}

func TestParseCalendarEventsReportsUnreadableData(t *testing.T) {
	var using []string
	var calls [][]json.RawMessage
	server := contactServer(t, `{"methodResponses": [["CalendarEvent/parse", {"accountId": "a1", "parsed": {}, "notParsable": ["b1"]}, "0"]], "sessionState": "x"}`, &using, &calls)
	client := &Client{HTTP: server.Client()}
	if _, err := client.ParseCalendarEvents(context.Background(), &Session{APIURL: server.URL + "/jmap/"}, "a1", []string{"b1"}); err == nil {
		t.Fatal("no error for an unreadable blob")
	}
}
