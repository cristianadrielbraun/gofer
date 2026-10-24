package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// TestJMAPCalendarLiveWrites creates, edits and deletes events and single
// occurrences on a real server through the CalDAV code, as the calendar
// does, and skips unless GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestJMAPCalendarLiveWrites(t *testing.T) {
	live := newJMAPCalendarLive(t)
	ctx, h, source, connection := live.ctx, live.h, live.source, live.connection
	serverEvent, cleanup, stored := live.serverEvent, live.cleanup, live.stored
	day := live.day
	tag := time.Now().UTC().Format("150405")

	// A single event.
	start, end := day, day.Add(time.Hour)
	draft := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live " + tag, Description: "First notes", TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end}
	created, err := h.createCalendarProviderEvent(ctx, source, draft)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(created.RemoteID)
	if event := serverEvent(created.RemoteID); event == nil || event["title"] != draft.Summary {
		t.Fatalf("created %q = %+v", created.RemoteID, event)
	}
	// A retried create finds the event instead of making another, once the
	// server's search has indexed it.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if found, _ := connection.ByUID(ctx, jmapLiveCalendarID(t, source), draft.RequestID+"@gofer"); found != nil {
			break
		}
	}
	again, err := h.createCalendarProviderEvent(ctx, source, draft)
	if err != nil || again.RemoteID != created.RemoteID {
		t.Fatalf("retried create = %q, %v; want %q", again.RemoteID, err, created.RemoteID)
	}
	// Something Gofer doesn't keep, set on the server, survives an edit.
	_, id, _ := jmapCalendarDAVParse(created.RemoteID)
	if _, err := connection.Update(ctx, id, jmap.PatchObject{"keywords": map[string]any{"gofer-live": true}}); err != nil {
		t.Fatal(err)
	}
	events := stored(draft.Summary)
	if len(events) != 1 {
		t.Fatalf("stored = %+v", events)
	}
	edit := draft
	edit.Summary = draft.Summary + " edited"
	editStart, editEnd := start.Add(30*time.Minute), end.Add(30*time.Minute)
	edit.StartAt, edit.EndAt = &editStart, &editEnd
	updated, err := h.updateCalendarProviderEvent(ctx, source, events[0], edit, false)
	if err != nil {
		t.Fatal(err)
	}
	event := serverEvent(updated.RemoteID)
	// Gofer writes its own events' times in UTC: 10:30 in Prague.
	if event["title"] != edit.Summary || event["start"] != "2026-12-07T09:30:00" || event["timeZone"] != "Etc/UTC" || event["keywords"] == nil {
		t.Fatalf("after editing = %+v", event)
	}
	// An edit made from the old version is refused.
	stale := events[0]
	if _, err := h.updateCalendarProviderEvent(ctx, source, stale, draft, false); err == nil {
		t.Fatal("an edit of an outdated version was accepted")
	}
	events = stored(edit.Summary)
	if len(events) != 1 {
		t.Fatalf("stored after editing = %+v", events)
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, events[0], false); err != nil {
		t.Fatal(err)
	}
	if serverEvent(updated.RemoteID) != nil {
		t.Fatal("the deleted event is still on the server")
	}

	// A weekly series: one occurrence moved, one deleted, then the series.
	series := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live series " + tag, TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		Recurrence: &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 1, Count: 3}}
	createdSeries, err := h.createCalendarProviderEvent(ctx, source, series)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(createdSeries.RemoteID)
	occurrences := stored(series.Summary)
	if len(occurrences) != 3 {
		t.Fatalf("series occurrences = %d", len(occurrences))
	}
	second := occurrences[1]
	moveStart, moveEnd := second.StartAt.Add(2*time.Hour), second.EndAt.Add(2*time.Hour)
	move := calendar.EventDraft{Summary: series.Summary + " (moved)", TimeZone: "Europe/Prague", StartAt: &moveStart, EndAt: &moveEnd}
	if _, err := h.updateCalendarProviderEvent(ctx, source, second, move, false, true); err != nil {
		t.Fatal(err)
	}
	remaining := stored(series.Summary)
	if len(remaining) != 2 || len(stored(move.Summary)) != 1 {
		t.Fatalf("after moving one occurrence: %d left, %d moved", len(remaining), len(stored(move.Summary)))
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, remaining[1], false, true); err != nil {
		t.Fatal(err)
	}
	if left := stored(series.Summary); len(left) != 1 {
		t.Fatalf("after deleting one occurrence: %d left", len(left))
	}
	overrides, _ := serverEvent(createdSeries.RemoteID)["recurrenceOverrides"].(map[string]any)
	if len(overrides) != 2 {
		t.Fatalf("server overrides = %+v", overrides)
	}
	first := stored(series.Summary)[0]
	master, err := h.readCalendarProviderSeries(ctx, source, first, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, master, true); err != nil {
		t.Fatal(err)
	}
	if serverEvent(createdSeries.RemoteID) != nil {
		t.Fatal("the deleted series is still on the server")
	}

	// A whole series edited: its title and time.
	plain := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live plain series " + tag, TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		Recurrence: &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 2}}
	createdPlain, err := h.createCalendarProviderEvent(ctx, source, plain)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(createdPlain.RemoteID)
	master, err = h.readCalendarProviderSeries(ctx, source, stored(plain.Summary)[0])
	if err != nil {
		t.Fatal(err)
	}
	whole := plain
	whole.Summary = plain.Summary + " renamed"
	laterStart, laterEnd := start.Add(time.Hour), end.Add(time.Hour)
	whole.StartAt, whole.EndAt = &laterStart, &laterEnd
	if _, err := h.updateCalendarProviderEvent(ctx, source, master, whole, true); err != nil {
		t.Fatal(err)
	}
	renamed := stored(whole.Summary)
	if len(renamed) != 2 || !renamed[0].StartAt.Equal(laterStart) {
		t.Fatalf("renamed series = %+v", renamed)
	}
}

func jmapLiveCalendarID(t *testing.T, source storage.CalendarSource) string {
	t.Helper()
	id, _, ok := jmapCalendarDAVParse(source.RemoteID)
	if !ok {
		t.Fatalf("calendar id %q", source.RemoteID)
	}
	return id
}

// jmapCalendarLive is a JMAP account on a real server with its default
// calendar discovered, for the live tests; see newJMAPCalendarLive.
type jmapCalendarLive struct {
	t          *testing.T
	ctx        context.Context
	h          *Handler
	db         *storage.DB
	user       string
	account    *models.Account
	source     storage.CalendarSource
	connection *mail.JMAPCalendarConnection
	day        time.Time
}

// newJMAPCalendarLive sets up the live tests' account, and skips unless
// GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func newJMAPCalendarLive(t *testing.T) *jmapCalendarLive {
	t.Helper()
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	ctx := context.Background()
	h, db := newAccountOwnershipTestHandler(t)
	address, _ := url.Parse(sessionURL)
	port, _ := strconv.Atoi(address.Port())
	if address.Scheme == "http" {
		if err := db.AddPlaintextTransportException(ctx, "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
		if err := db.AddPrivateTargetException(ctx, "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
	}
	account, err := h.accountStore.CreateAccount(ctx, "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: user, DisplayName: "Live", JMAPSessionURL: sessionURL, Username: user, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.discoverJMAPCalendarSources(ctx, "owner", account.ID); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListCalendarSourcesForAccount(ctx, "owner", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.IsPrimary {
			source = candidate
		}
	}
	if !calendarSourceWritable(source) {
		t.Fatalf("default calendar is not writable: %+v", source)
	}
	cfg, err := h.accountStore.GetConfig(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := mail.OpenJMAPCalendars(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	zone, _ := time.LoadLocation("Europe/Prague")
	return &jmapCalendarLive{t: t, ctx: ctx, h: h, db: db, user: user, account: account, source: source, connection: connection, day: time.Date(2026, 12, 7, 10, 0, 0, 0, zone)}
}

// serverEvent is an event as the server has it, or nil.
func (l *jmapCalendarLive) serverEvent(remoteID string) jmap.CalendarEvent {
	l.t.Helper()
	base, _, _ := strings.Cut(remoteID, "#")
	_, id, ok := jmapCalendarDAVParse(base)
	if !ok {
		l.t.Fatalf("event id %q", remoteID)
	}
	event, err := l.connection.Event(context.Background(), id)
	if err != nil {
		l.t.Fatal(err)
	}
	return event
}

// cleanup deletes an event from the server once the test is done.
func (l *jmapCalendarLive) cleanup(remoteID string) {
	l.t.Cleanup(func() {
		if event := l.serverEvent(remoteID); event != nil {
			_, _ = l.connection.Delete(context.Background(), event.ID())
		}
	})
}

// stored syncs the month from the test day and gives the cached events with
// a title.
func (l *jmapCalendarLive) stored(summary string) []storage.CalendarEvent {
	l.t.Helper()
	windowStart, windowEnd := l.day.AddDate(0, 0, -1), l.day.AddDate(0, 1, 0)
	if _, err := l.h.syncCalendarScopedWindow(l.ctx, "owner", windowStart, windowEnd, "", l.account.ID); err != nil {
		l.t.Fatal(err)
	}
	events, err := l.db.ListCalendarEvents(l.ctx, "owner", windowStart, windowEnd)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []storage.CalendarEvent
	for _, event := range events {
		if event.Summary == summary {
			out = append(out, event)
		}
	}
	return out
}

// TestJMAPCalendarLiveMeetings invites, updates and cancels a meeting, and
// answers an invitation, on a real server; Gofer sends the emails itself,
// so each one is checked in its outbox. It skips unless GOFER_JMAP_TEST_URL,
// _USER and _PASSWORD are set.
func TestJMAPCalendarLiveMeetings(t *testing.T) {
	live := newJMAPCalendarLive(t)
	ctx, h, db, source := live.ctx, live.h, live.db, live.source
	tag := time.Now().UTC().Format("150405")
	outbox := func(method string) []storage.OutgoingSend {
		t.Helper()
		rows, err := db.Read().Query(`SELECT id FROM outgoing_sends WHERE json_extract(message_json,'$.calendar_notification.Method')=? OR json_extract(message_json,'$.calendar_reply.Method')=? ORDER BY rowid`, method, method)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var sends []storage.OutgoingSend
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			send, err := db.GetOutgoingSend(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			sends = append(sends, send)
		}
		return sends
	}
	// The check made just before an invitation goes out reads the event
	// back from the server.
	sendable := func(send storage.OutgoingSend) error {
		t.Helper()
		var snapshot outgoingMessageSnapshot
		if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
			t.Fatal(err)
		}
		unlock, err := h.beforeCalendarNotificationSend(ctx, send, snapshot.outgoingMessage())
		unlock()
		return err
	}

	start := live.day.Add(48 * time.Hour)
	end := start.Add(time.Hour)
	draft := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live meeting " + tag, TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		GuestsSet: true, Guests: []calendar.GuestDraft{{Email: "ana.lopes@example.test", Name: "Ana Lopes"}}, OrganizerEmail: live.user, OrganizerName: "Live"}
	created, err := h.createCalendarProviderEvent(ctx, source, draft)
	if err != nil {
		t.Fatal(err)
	}
	live.cleanup(created.RemoteID)
	event := live.serverEvent(created.RemoteID)
	if event == nil || !strings.Contains(fmt.Sprint(event["participants"]), "mailto:ana.lopes@example.test") || event["organizerCalendarAddress"] == nil {
		t.Fatalf("meeting on the server = %+v", event)
	}
	// The invitation names the event by its creation name, which the server
	// finds by UID once its search has indexed the event.
	live.indexed(draft.RequestID + "@gofer")
	if invitations := outbox("REQUEST"); len(invitations) != 1 || sendable(invitations[0]) != nil {
		t.Fatalf("invitations = %d, sendable %v", len(invitations), sendable(invitations[0]))
	}

	meetings := live.stored(draft.Summary)
	if len(meetings) != 1 || !calendarCachedOrganizer(meetings[0]) {
		t.Fatalf("stored meeting = %+v", meetings)
	}
	moved := draft
	later, laterEnd := start.Add(time.Hour), end.Add(time.Hour)
	moved.StartAt, moved.EndAt = &later, &laterEnd
	if _, err := h.updateCalendarProviderEvent(ctx, source, meetings[0], moved, false); err != nil {
		t.Fatal(err)
	}
	if updates := outbox("REQUEST"); len(updates) != 2 || sendable(updates[1]) != nil {
		t.Fatalf("updates = %d, sendable %v", len(updates), sendable(updates[len(updates)-1]))
	}
	if sequence, _ := live.serverEvent(created.RemoteID)["sequence"].(float64); sequence < 1 {
		t.Fatalf("the update didn't raise the sequence: %v", sequence)
	}

	// Ana's acceptance arrives by mail and is written into the meeting.
	live.stored(draft.Summary)
	sequence, _ := live.serverEvent(created.RemoteID)["sequence"].(float64)
	incoming := message.CalendarIncomingReply{UID: draft.RequestID + "@gofer", Organizer: live.user, Attendee: "ana.lopes@example.test", From: "ana.lopes@example.test",
		Status: "ACCEPTED", Sequence: int(sequence), Stamp: time.Now().UTC()}
	if err := h.applyCalendarIncomingReply(ctx, storage.CalendarIncomingMessage{ID: 1, UserID: "owner", AccountID: live.account.ID, From: incoming.From}, incoming); err != nil {
		t.Fatalf("incoming reply: %v", err)
	}
	if guests := fmt.Sprint(live.serverEvent(created.RemoteID)["participants"]); !strings.Contains(guests, "participationStatus:accepted") || !strings.Contains(guests, "ana.lopes") {
		t.Fatalf("participants after Ana's reply = %s", guests)
	}

	meetings = live.stored(draft.Summary)
	if _, err := h.deleteCalendarProviderEvent(ctx, source, meetings[0], false); err != nil {
		t.Fatal(err)
	}
	if live.serverEvent(created.RemoteID) != nil {
		t.Fatal("the cancelled meeting is still on the server")
	}
	if cancellations := outbox("CANCEL"); len(cancellations) != 1 || sendable(cancellations[0]) != nil {
		t.Fatalf("cancellations = %d", len(cancellations))
	}

	// An invitation from someone else, answered.
	calendarID := jmapLiveCalendarID(t, source)
	inviteStart := start.Add(24 * time.Hour)
	invitation := jmap.CalendarEvent{"@type": "Event", "uid": "gofer-live-invitation-" + tag + "@example.test", "title": "Gofer live invitation " + tag,
		"start": inviteStart.UTC().Format("2006-01-02T15:04:05"), "timeZone": "Etc/UTC", "duration": "PT1H", "sequence": 1,
		"organizerCalendarAddress": "mailto:omar.haddad@example.test",
		"participants": map[string]any{
			"o":  map[string]any{"@type": "Participant", "name": "Omar Haddad", "calendarAddress": "mailto:omar.haddad@example.test", "roles": map[string]any{"owner": true, "attendee": true}, "participationStatus": "accepted"},
			"me": map[string]any{"@type": "Participant", "calendarAddress": "mailto:" + live.user, "roles": map[string]any{"attendee": true}, "participationStatus": "needs-action", "expectReply": true},
		}}
	invitationID, err := live.connection.Create(ctx, calendarID, invitation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = live.connection.Delete(context.Background(), invitationID) })
	invited := live.stored("Gofer live invitation " + tag)
	if len(invited) != 1 {
		t.Fatalf("stored invitation = %+v", invited)
	}
	form := url.Values{"scope": {"event"}, "response": {"accepted"}, "version": {invited[0].ETag}}
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/events/"+invited[0].ID+"/response", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", invited[0].ID)
	rec := httptest.NewRecorder()
	h.handleCalendarResponse(rec, ownerRequest(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	var answered struct {
		Delivery   string `json:"delivery"`
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answered); err != nil || answered.Delivery != "email" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	reply, err := db.GetOutgoingSend(ctx, answered.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	var replyMessage struct {
		Calendar string `json:"calendar_reply"`
	}
	_ = json.Unmarshal(reply.MessageJSON, &replyMessage)
	if !strings.Contains(replyMessage.Calendar, "METHOD:REPLY") || !strings.Contains(replyMessage.Calendar, "PARTSTAT=ACCEPTED:mailto:"+live.user) || !slices.Contains(reply.EnvelopeRecipients, "omar.haddad@example.test") {
		t.Fatalf("reply to %v:\n%s", reply.EnvelopeRecipients, replyMessage.Calendar)
	}
	// Before the reply goes out the invitation is read back; once it is
	// sent, the answer is saved in the calendar too.
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(reply.MessageJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	unlock, err := h.beforeCalendarReplySend(ctx, reply, snapshot.outgoingMessage())
	unlock()
	if err != nil {
		t.Fatalf("reply not sendable: %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status='sent' WHERE id=?`, reply.ID); err != nil {
		t.Fatal(err)
	}
	h.runCalendarReplyFollowups(ctx)
	saved, err := live.connection.Event(ctx, invitationID)
	if err != nil {
		t.Fatal(err)
	}
	participants, _ := saved["participants"].(map[string]any)
	accepted := false
	for _, raw := range participants {
		participant, _ := raw.(map[string]any)
		accepted = accepted || participant["calendarAddress"] == "mailto:"+live.user && participant["participationStatus"] == "accepted"
	}
	if !accepted {
		t.Fatalf("the answer isn't in the calendar: %+v", participants)
	}
}

// indexed waits until the server's search finds the event with a UID.
func (l *jmapCalendarLive) indexed(uid string) {
	l.t.Helper()
	calendarID := jmapLiveCalendarID(l.t, l.source)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if found, _ := l.connection.ByUID(l.ctx, calendarID, uid); found != nil {
			return
		}
	}
	l.t.Fatalf("the server never found %s by its UID", uid)
}

// TestJMAPCalendarLiveInvitations adds an invitation from an email to a
// real server's calendar, forwards it, and removes it on the cancellation.
// It skips unless GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestJMAPCalendarLiveInvitations(t *testing.T) {
	live := newJMAPCalendarLive(t)
	ctx, h := live.ctx, live.h
	owner := auth.ContextWithUser(ctx, &auth.User{ID: "owner", Username: "owner", IsAdmin: true})
	tag := time.Now().UTC().Format("150405")
	uid := "gofer-live-import-" + tag + "@example.test"
	invitationMail := func(method, sequence string) []byte {
		start := live.day.Add(72 * time.Hour).UTC()
		return []byte("From: Omar Haddad <omar.haddad@example.test>\r\nTo: " + live.user + "\r\nMIME-Version: 1.0\r\nContent-Type: text/calendar; method=" + method + "; charset=utf-8\r\n\r\n" +
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSEQUENCE:" + sequence + "\r\nDTSTAMP:20261024T000000Z\r\n" +
			"DTSTART:" + start.Format("20060102T150405Z") + "\r\nDTEND:" + start.Add(time.Hour).Format("20060102T150405Z") + "\r\nSUMMARY:Gofer live import " + tag + "\r\n" +
			"ORGANIZER;CN=Omar Haddad:mailto:omar.haddad@example.test\r\nATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:" + live.user + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	}
	invitation, err := readMailInvitation(invitationMail("REQUEST", "0"), uid, live.user, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applyMailInvitation(owner, live.account.ID, invitation, uid, live.source.ID); err != nil {
		t.Fatalf("add invitation: %v", err)
	}
	found, err := live.connection.ByUID(ctx, jmapLiveCalendarID(t, live.source), uid)
	for deadline := time.Now().Add(10 * time.Second); err == nil && found == nil && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		found, err = live.connection.ByUID(ctx, jmapLiveCalendarID(t, live.source), uid)
	}
	if err != nil || found == nil || found["title"] != "Gofer live import "+tag {
		t.Fatalf("imported invitation = %+v, %v", found, err)
	}
	t.Cleanup(func() { _, _ = live.connection.Delete(context.Background(), found.ID()) })

	imported := live.stored("Gofer live import " + tag)
	if len(imported) != 1 {
		t.Fatalf("stored invitation = %+v", imported)
	}
	form := url.Values{"recipients": {"lin.wei@example.test"}, "comment": {"You may want to join."}}
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/events/"+imported[0].ID+"/forward", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", imported[0].ID)
	rec := httptest.NewRecorder()
	h.handleCalendarForward(rec, ownerRequest(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("forward = %d %s", rec.Code, rec.Body.String())
	}
	var forwarded int
	if err := live.db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends WHERE envelope_recipients LIKE '%lin.wei@example.test%'`).Scan(&forwarded); err != nil || forwarded != 1 {
		t.Fatalf("forwarded messages = %d, %v", forwarded, err)
	}

	cancellation, err := readMailInvitation(invitationMail("CANCEL", "1"), uid, live.user, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applyMailInvitation(owner, live.account.ID, cancellation, uid, live.source.ID); err != nil {
		t.Fatalf("apply cancellation: %v", err)
	}
	if after, err := live.connection.Event(ctx, found.ID()); err != nil || after != nil && after["status"] != "cancelled" {
		t.Fatalf("after the cancellation = %+v, %v", after, err)
	}
}
