package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// importServer answers every request with answer and keeps the calls.
func importServer(t *testing.T, answer string, calls *[][]json.RawMessage) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = decodeCalls(t, r)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server
}

// ImportEmail stores the message in the mailbox asked for, with its keywords
// and its own received time.
func TestImportEmailKeepsKeywordsAndDate(t *testing.T) {
	var calls [][]json.RawMessage
	server := importServer(t, `{"methodResponses": [
	  ["Email/import", {"accountId": "a1", "created": {"email": {"id": "e1"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	received := time.Date(2024, 3, 5, 9, 30, 0, 0, time.FixedZone("ART", -3*3600))
	imported, err := (&Client{HTTP: server.Client()}).ImportEmail(context.Background(), &Session{APIURL: server.URL}, "a1", "b1", "mb-inbox", []string{KeywordSeen, KeywordFlagged}, received)
	if err != nil || *imported != (Imported{ID: "e1"}) {
		t.Fatalf("ImportEmail = %+v, %v", imported, err)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0][1], &args)
	email := args["emails"].(map[string]any)["email"].(map[string]any)
	if email["blobId"] != "b1" || !reflect.DeepEqual(email["mailboxIds"], map[string]any{"mb-inbox": true}) ||
		!reflect.DeepEqual(email["keywords"], map[string]any{"$seen": true, "$flagged": true}) ||
		email["receivedAt"] != "2024-03-05T12:30:00Z" {
		t.Fatalf("import = %v", email)
	}
}

// A message the server already has is the copy it has; any other refusal is
// an *ImportError.
func TestImportEmailReportsWhatHappened(t *testing.T) {
	var calls [][]json.RawMessage
	existing := importServer(t, `{"methodResponses": [
	  ["Email/import", {"accountId": "a1", "notCreated": {"email": {"type": "alreadyExists", "existingId": "e0"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	imported, err := (&Client{HTTP: existing.Client()}).ImportEmail(context.Background(), &Session{APIURL: existing.URL}, "a1", "b1", "mb", nil, time.Time{})
	if err != nil || *imported != (Imported{ID: "e0", Existed: true}) {
		t.Fatalf("existing = %+v, %v", imported, err)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0][1], &args)
	if _, dated := args["emails"].(map[string]any)["email"].(map[string]any)["receivedAt"]; dated {
		t.Fatal("a zero date was sent")
	}
	refused := importServer(t, `{"methodResponses": [
	  ["Email/import", {"accountId": "a1", "notCreated": {"email": {"type": "invalidEmail"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	imported, err = (&Client{HTTP: refused.Client()}).ImportEmail(context.Background(), &Session{APIURL: refused.URL}, "a1", "b1", "mb", nil, time.Time{})
	var importErr *ImportError
	if imported != nil || !errors.As(err, &importErr) || importErr.Type != "invalidEmail" {
		t.Fatalf("refused = %+v, %v", imported, err)
	}
}

func TestCreateMailbox(t *testing.T) {
	var calls [][]json.RawMessage
	server := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "created": {"mailbox": {"id": "mb2"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	client := &Client{HTTP: server.Client()}
	for _, parent := range []string{"", "mb1"} {
		id, err := client.CreateMailbox(context.Background(), &Session{APIURL: server.URL}, "a1", "Receipts", parent)
		if err != nil || id != "mb2" {
			t.Fatalf("CreateMailbox = %q, %v", id, err)
		}
		var args map[string]any
		_ = json.Unmarshal(calls[0][1], &args)
		mailbox := args["create"].(map[string]any)["mailbox"].(map[string]any)
		var want any
		if parent != "" {
			want = parent
		}
		if mailbox["name"] != "Receipts" || mailbox["parentId"] != want {
			t.Fatalf("create = %v", mailbox)
		}
	}
	refused := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "notCreated": {"mailbox": {"type": "forbidden"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	_, err := (&Client{HTTP: refused.Client()}).CreateMailbox(context.Background(), &Session{APIURL: refused.URL}, "a1", "Receipts", "")
	var importErr *ImportError
	if !errors.As(err, &importErr) || importErr.Type != "forbidden" {
		t.Fatalf("refused = %v", err)
	}
}

func TestRenameMailbox(t *testing.T) {
	var calls [][]json.RawMessage
	server := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "updated": {"mb1": null}}, "0"]
	], "sessionState": "s1"}`, &calls)
	if err := (&Client{HTTP: server.Client()}).RenameMailbox(context.Background(), &Session{APIURL: server.URL}, "a1", "mb1", "Bills"); err != nil {
		t.Fatalf("RenameMailbox = %v", err)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0][1], &args)
	if update := args["update"].(map[string]any)["mb1"].(map[string]any); update["name"] != "Bills" || len(update) != 1 {
		t.Fatalf("update = %v", update)
	}
	refused := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "notUpdated": {"mb1": {"type": "forbidden"}}}, "0"]
	], "sessionState": "s1"}`, &calls)
	err := (&Client{HTTP: refused.Client()}).RenameMailbox(context.Background(), &Session{APIURL: refused.URL}, "a1", "mb1", "Bills")
	var importErr *ImportError
	if !errors.As(err, &importErr) || importErr.Type != "forbidden" {
		t.Fatalf("refused = %v", err)
	}
}

// A mailbox moves inside another, or to the top with a null parent.
func TestMoveMailbox(t *testing.T) {
	var calls [][]json.RawMessage
	server := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "updated": {"mb1": null}}, "0"]
	], "sessionState": "s1"}`, &calls)
	client := &Client{HTTP: server.Client()}
	for _, parent := range []string{"trash", ""} {
		if err := client.MoveMailbox(context.Background(), &Session{APIURL: server.URL}, "a1", "mb1", parent, "Bills 2"); err != nil {
			t.Fatalf("MoveMailbox(%q) = %v", parent, err)
		}
		var args map[string]any
		_ = json.Unmarshal(calls[0][1], &args)
		update := args["update"].(map[string]any)["mb1"].(map[string]any)
		var want any
		if parent != "" {
			want = parent
		}
		if got, ok := update["parentId"]; !ok || got != want || update["name"] != "Bills 2" {
			t.Fatalf("update = %v", update)
		}
	}
}

// Mailboxes are destroyed one call each, in the order given, with their
// emails.
func TestDestroyMailboxes(t *testing.T) {
	var calls [][]json.RawMessage
	server := importServer(t, `{"methodResponses": [
	  ["Mailbox/set", {"accountId": "a1", "destroyed": ["child"]}, "0"],
	  ["Mailbox/set", {"accountId": "a1", "notDestroyed": {"parent": {"type": "mailboxHasChild"}}}, "1"]
	], "sessionState": "s1"}`, &calls)
	err := (&Client{HTTP: server.Client()}).DestroyMailboxes(context.Background(), &Session{APIURL: server.URL}, "a1", []string{"child", "parent"})
	var importErr *ImportError
	if !errors.As(err, &importErr) || importErr.Type != "mailboxHasChild" {
		t.Fatalf("DestroyMailboxes = %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	for i, want := range []string{"child", "parent"} {
		var args map[string]any
		_ = json.Unmarshal(calls[i][1], &args)
		destroy := args["destroy"].([]any)
		if len(destroy) != 1 || destroy[0] != want || args["onDestroyRemoveEmails"] != true {
			t.Fatalf("call %d = %v", i, args)
		}
	}
}

// FindEmails sends the header search and every window in one request, uses
// the searches the server answers, and keeps only exact Message-IDs.
func TestFindEmailsSearchesTogether(t *testing.T) {
	var requests [][][]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := decodeCalls(t, r)
		requests = append(requests, calls)
		var answers []string
		for _, call := range calls {
			var name, id string
			_ = json.Unmarshal(call[0], &name)
			_ = json.Unmarshal(call[2], &id)
			var query EmailQuery
			_ = json.Unmarshal(call[1], &query)
			switch {
			case name == "Email/query" && query.Filter.Header != nil:
				answers = append(answers, `["error", {"type": "unsupportedFilter"}, "`+id+`"]`)
			case name == "Email/query":
				answers = append(answers, `["Email/query", {"accountId": "a1", "ids": ["e1", "e2"]}, "`+id+`"]`)
			case id == "g0":
				answers = append(answers, `["error", {"type": "invalidResultReference"}, "g0"]`)
			default:
				answers = append(answers, `["Email/get", {"accountId": "a1", "list": [
				  {"id": "e1", "messageId": ["m1@example.test"]}, {"id": "e2", "messageId": ["xm1@example.test"]}]}, "`+id+`"]`)
			}
		}
		_, _ = io.WriteString(w, `{"methodResponses": [`+strings.Join(answers, ",")+`], "sessionState": "s1"}`)
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client()}
	at := time.Date(2023, 7, 1, 8, 0, 0, 0, time.UTC)
	windows := []Window{{From: at.Add(-time.Second), To: at.Add(time.Second)}, {From: at.Add(time.Hour), To: at.Add(2 * time.Hour)}}
	found, err := client.FindEmails(context.Background(), &Session{APIURL: server.URL}, "a1", "mb", "m1@example.test", windows)
	if err != nil || !reflect.DeepEqual(found, []string{"e1"}) {
		t.Fatalf("FindEmails = %v, %v", found, err)
	}
	if len(requests) != 1 || len(requests[0]) != 6 {
		t.Fatalf("want one request of six calls, got %d requests", len(requests))
	}
	var first EmailQuery
	_ = json.Unmarshal(requests[0][2][1], &first)
	if first.Filter.InMailbox != "mb" || !first.Filter.After.Equal(at.Add(-time.Second)) || !first.Filter.Before.Equal(at.Add(2*time.Second)) {
		t.Fatalf("window filter = %+v", first.Filter)
	}

	// A server taking fewer calls per request gets several requests.
	requests = nil
	session := &Session{APIURL: server.URL, Core: CoreCapability{MaxCallsInRequest: 4}}
	if found, err := client.FindEmails(context.Background(), session, "a1", "mb", "m1@example.test", windows); err != nil || len(found) != 1 {
		t.Fatalf("split FindEmails = %v, %v", found, err)
	}
	if len(requests) != 2 || len(requests[0]) != 4 || len(requests[1]) != 2 {
		t.Fatalf("split into %d requests", len(requests))
	}

	// A server taking one call per request gets each search's query and get
	// in two requests.
	requests = nil
	session = &Session{APIURL: server.URL, Core: CoreCapability{MaxCallsInRequest: 1}}
	if found, err := client.FindEmails(context.Background(), session, "a1", "mb", "m1@example.test", windows); err != nil || !reflect.DeepEqual(found, []string{"e1"}) {
		t.Fatalf("one-call FindEmails = %v, %v", found, err)
	}
	// The header search, refused, then a query and a get for each window.
	if len(requests) != 5 {
		t.Fatalf("sent %d requests, want 5", len(requests))
	}
	for _, calls := range requests {
		if len(calls) != 1 {
			t.Fatalf("a request carried %d calls", len(calls))
		}
	}
}

func TestFindEmailsFailsWhenNoSearchIsAnswered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := (&Client{HTTP: server.Client()}).FindEmails(context.Background(), &Session{APIURL: server.URL}, "a1", "mb", "m1@example.test", []Window{{}})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("FindEmails = %v", err)
	}
}

// Calendars and address books are created, renamed and deleted with their
// /set, deleting what they hold too, and a refusal says what was refused.
func TestCalendarAndAddressBookSets(t *testing.T) {
	var calls [][]json.RawMessage
	args := func() map[string]any {
		var out map[string]any
		_ = json.Unmarshal(calls[0][1], &out)
		return out
	}
	ok := func(method string) (*Client, *Session) {
		server := importServer(t, `{"methodResponses": [["`+method+`", {"accountId": "a1", "created": {"new": {"id": "n1"}}}, "0"]], "sessionState": "s1"}`, &calls)
		t.Cleanup(server.Close)
		return &Client{HTTP: server.Client()}, &Session{APIURL: server.URL}
	}

	calendars, session := ok("Calendar/set")
	if id, err := calendars.CreateCalendar(context.Background(), session, "a1", "Club"); err != nil || id != "n1" || args()["create"].(map[string]any)["new"].(map[string]any)["name"] != "Club" {
		t.Fatalf("create calendar = %q, %v, %v", id, err, args())
	}
	if err := calendars.RenameCalendar(context.Background(), session, "a1", "c1", "Book club"); err != nil || args()["update"].(map[string]any)["c1"].(map[string]any)["name"] != "Book club" {
		t.Fatalf("rename calendar = %v, %v", err, args())
	}
	if err := calendars.DestroyCalendar(context.Background(), session, "a1", "c1"); err != nil || args()["onDestroyRemoveEvents"] != true || args()["destroy"].([]any)[0] != "c1" {
		t.Fatalf("delete calendar = %v, %v", err, args())
	}
	books, bookSession := ok("AddressBook/set")
	if err := books.DestroyAddressBook(context.Background(), bookSession, "a1", "b1"); err != nil || args()["onDestroyRemoveContents"] != true {
		t.Fatalf("delete address book = %v, %v", err, args())
	}

	refused := importServer(t, `{"methodResponses": [["Calendar/set", {"accountId": "a1", "notDestroyed": {"c1": {"type": "forbidden", "description": "default calendar"}}}, "0"]], "sessionState": "s1"}`, &calls)
	err := (&Client{HTTP: refused.Client()}).DestroyCalendar(context.Background(), &Session{APIURL: refused.URL}, "a1", "c1")
	if err == nil || !strings.Contains(err.Error(), "did not delete the calendar") || !strings.Contains(err.Error(), "default calendar") {
		t.Fatalf("refused delete = %v", err)
	}
}
