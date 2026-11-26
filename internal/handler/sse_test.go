package handler

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
)

func TestSSEEventVisibleRequiresAnOwnedAudience(t *testing.T) {
	accounts := map[string]bool{"owned": true}
	tests := []struct {
		name    string
		event   mail.Event
		userID  string
		isAdmin bool
		want    bool
	}{
		{name: "owned account", event: mail.Event{AccountID: "owned"}, userID: "user-a", want: true},
		{name: "foreign account", event: mail.Event{AccountID: "foreign"}, userID: "user-a", want: false},
		{name: "owned user", event: mail.Event{UserID: "user-a"}, userID: "user-a", want: true},
		{name: "foreign user", event: mail.Event{UserID: "user-b"}, userID: "user-a", want: false},
		{name: "owned user list", event: mail.Event{UserIDs: []string{"user-b", "user-a"}}, userID: "user-a", want: true},
		{name: "foreign user list", event: mail.Event{UserIDs: []string{"user-b"}}, userID: "user-a", want: false},
		{name: "legacy payload owner", event: mail.Event{Payload: map[string]any{"user_id": "user-a"}}, userID: "user-a", want: true},
		{name: "legacy payload foreign", event: mail.Event{Payload: map[string]any{"user_id": "user-b"}}, userID: "user-a", want: false},
		{name: "admin only admin", event: mail.Event{AdminOnly: true}, userID: "admin", isAdmin: true, want: true},
		{name: "admin only regular user", event: mail.Event{AdminOnly: true}, userID: "user-a", want: false},
		{name: "unscoped", event: mail.Event{}, userID: "user-a", want: false},
		{name: "admin does not bypass account ownership", event: mail.Event{AccountID: "foreign"}, userID: "admin", isAdmin: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sseEventVisible(tt.event, tt.userID, accounts, tt.isAdmin); got != tt.want {
				t.Fatalf("sseEventVisible() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGlobalProcessingStatusRequiresAdmin(t *testing.T) {
	h := &Handler{}
	next := h.adminOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tt := range []struct {
		name string
		user *auth.User
		want int
	}{
		{name: "unauthenticated", user: nil, want: http.StatusForbidden},
		{name: "regular user", user: &auth.User{ID: "user-a"}, want: http.StatusForbidden},
		{name: "administrator", user: &auth.User{ID: "admin", IsAdmin: true}, want: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/system/processing", nil)
			if tt.user != nil {
				req = req.WithContext(auth.ContextWithUser(req.Context(), tt.user))
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, req)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}

// A browser's stream carries the events of its user and accounts, and none
// of another user's, now that events are routed to the connections they are
// about.
func TestSSEStreamCarriesOnlyTheUsersEvents(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	h.syncer = mail.NewSyncOrchestrator(h.db, h.accountStore, nil, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleSSE(w, attackerRequest(r))
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := bufio.NewScanner(response.Body)
	next := func() string {
		for lines.Scan() {
			if name, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
				return name
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return ""
	}
	if got := next(); got != "connected" {
		t.Fatalf("first event %q", got)
	}
	bus := h.syncer.Events()
	bus.Publish(mail.Event{Type: mail.EventNewMail, AccountID: "victim-account", FolderID: "inbox"})
	bus.Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: "owner"})
	bus.Publish(mail.Event{Type: mail.EventAvatarBackfill, AdminOnly: true})
	bus.Publish(mail.Event{Type: mail.EventMutation, AccountID: "attacker-account", FolderID: "inbox"})
	bus.Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: "attacker"})
	bus.Publish(mail.Event{Type: mail.EventSendResult, UserIDs: []string{"owner", "attacker"}})
	var got []string
	for len(got) < 3 {
		got = append(got, next())
	}
	want := []string{string(mail.EventMutation), string(mail.EventCalendarChanged), string(mail.EventSendResult)}
	if !slices.Equal(got, want) {
		t.Fatalf("stream carried %v, want %v", got, want)
	}
}

// Background mail work that changes reaches only its owner's pages, with
// nothing about the work itself.
func TestSSEStreamTellsOnlyTheOwnerThatMailOperationsChanged(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mail.NewSyncOrchestrator(h.db, h.accountStore, nil, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleSSE(w, attackerRequest(r))
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// An event that never comes ends the stream instead of the test run.
	stop := time.AfterFunc(10*time.Second, func() { response.Body.Close() })
	defer stop.Stop()
	lines := bufio.NewScanner(response.Body)
	next := func() (string, string) {
		name := ""
		for lines.Scan() {
			if value, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
				name = value
			} else if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok && name != "" {
				return name, data
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return "", ""
	}
	if got, _ := next(); got != "connected" {
		t.Fatalf("first event %q", got)
	}
	// The owner's work changes first, then the attacker's own.
	for _, account := range []string{"victim-account", "attacker-account"} {
		if _, err := db.Write().Exec(`INSERT INTO outgoing_sends (id, account_id, transport, envelope_from, send_after) VALUES (?, ?, 'smtp', 'someone@example.com', CURRENT_TIMESTAMP)`, "send-"+account, account); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: "attacker"})
	var got []string
	for len(got) < 2 {
		name, data := next()
		if name == string(mail.EventMailOperationsChanged) && strings.Contains(data, "someone@example.com") {
			t.Fatalf("event carried the work: %s", data)
		}
		got = append(got, name)
	}
	want := []string{string(mail.EventMailOperationsChanged), string(mail.EventCalendarChanged)}
	if !slices.Equal(got, want) {
		t.Fatalf("stream carried %v, want %v", got, want)
	}
}
