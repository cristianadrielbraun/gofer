package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Turning off conversations lists every message on its own row, and the
// filters for conversations no longer apply.
func TestEmailListSettingsFollowConversationGrouping(t *testing.T) {
	req := httptest.NewRequest("GET", "/folder/inbox?threads_only=1", nil)
	grouped := applyEmailListSettings(parseEmailFilters(req), req, map[string]string{})
	if grouped.ByMessage || !grouped.ThreadsOnly {
		t.Fatalf("by default: ByMessage %v, ThreadsOnly %v; want conversations filtered to threads", grouped.ByMessage, grouped.ThreadsOnly)
	}
	single := applyEmailListSettings(parseEmailFilters(req), req, map[string]string{"group_conversations": "false"})
	if !single.ByMessage || single.ThreadsOnly || single.NoThreads {
		t.Fatalf("without conversations: ByMessage %v, ThreadsOnly %v, NoThreads %v; want messages, unfiltered", single.ByMessage, single.ThreadsOnly, single.NoThreads)
	}
}

// The reading pane shows the whole conversation of an opened message only
// while mail is grouped into conversations.
func TestEmailPartialFollowsConversationGrouping(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, remote_id, name, role) VALUES ('owner-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox');
		INSERT INTO messages (id, account_id, thread_id, subject, from_email, snippet, date_received) VALUES
			(201, 'victim-account', 'lunch', 'Lunch on Friday?', 'pia@example.test', 'Shall we?', '2024-05-02 09:00:00'),
			(202, 'victim-account', 'lunch', 'Re: Lunch on Friday?', 'owner@example.com', 'Sounds good', '2024-05-02 10:00:00');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid) VALUES (201, 'owner-inbox', 1), (202, 'owner-inbox', 2);`); err != nil {
		t.Fatal(err)
	}
	cards := func() int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/email/202", nil)
		req.SetPathValue("id", "202")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		h.handleEmailPartial(rec, ownerRequest(req))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
		}
		return strings.Count(rec.Body.String(), "data-thread-card=")
	}
	if got := cards(); got != 2 {
		t.Fatalf("grouped: %d messages shown, want the conversation's 2", got)
	}
	if err := db.MergeUISettings(ctx, "owner", map[string]string{"group_conversations": "false"}); err != nil {
		t.Fatal(err)
	}
	if got := cards(); got > 1 {
		t.Fatalf("not grouped: %d messages shown, want only the one opened", got)
	}
}
