package storage

import (
	"fmt"
	"testing"
	"time"
)

func setSubjectThreading(t *testing.T, db *DB, enabled bool, windowDays, maxMessages string) {
	t.Helper()
	if err := db.MergeUISettings(t.Context(), "reader", map[string]string{
		"subject_threading":              fmt.Sprint(enabled),
		"subject_threading_window_days":  windowDays,
		"subject_threading_max_messages": maxMessages,
	}); err != nil {
		t.Fatal(err)
	}
}

func notice(uid uint32, from, subject string, sent time.Time) SyncMessage {
	return SyncMessage{AccountID: "acct", FolderID: "box-inbox", RemoteUID: uid,
		MessageID: fmt.Sprintf("<notice-%d@example.test>", uid), Subject: subject, FromEmail: from,
		ToRecipients: []Recipient{{Email: "reader@example.test"}}, DateSent: sent}
}

func syncNotices(t *testing.T, db *DB, messages ...SyncMessage) {
	t.Helper()
	for _, m := range messages {
		if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{m}); err != nil {
			t.Fatal(err)
		}
	}
}

func threadsOf(t *testing.T, db *DB, messages ...SyncMessage) []string {
	t.Helper()
	ids := make([]string, len(messages))
	for i, m := range messages {
		ids[i] = messageThreadID(t, db, m.MessageID)
	}
	return ids
}

// assertGroups checks which messages share a conversation: equal letters in
// want mean the same conversation.
func assertGroups(t *testing.T, db *DB, want string, messages ...SyncMessage) {
	t.Helper()
	got := threadsOf(t, db, messages...)
	for i := range messages {
		for j := i + 1; j < len(messages); j++ {
			if (want[i] == want[j]) != (got[i] == got[j]) {
				t.Fatalf("conversations %v, want grouping %q", got, want)
			}
		}
	}
}

func regroup(t *testing.T, db *DB) {
	t.Helper()
	ids, err := db.SubjectRegroupMessageIDs(t.Context(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegroupSubjectThreads(t.Context(), "reader", ids); err != nil {
		t.Fatal(err)
	}
}

var noticeDay = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

// Off by default: mail without reply headers starts its own conversation,
// replies by subject included.
func TestSubjectThreadingOffByDefault(t *testing.T) {
	db := newFolderThreadTestDB(t)
	a := notice(1, "alerts@bank.example.test", "Deposit received", noticeDay)
	b := notice(2, "alerts@bank.example.test", "Deposit received", noticeDay.Add(24*time.Hour))
	c := notice(3, "alerts@bank.example.test", "Re: Deposit received", noticeDay.Add(48*time.Hour))
	syncNotices(t, db, a, b, c)
	assertGroups(t, db, "abc", a, b, c)
}

func TestSubjectThreadingGroupsSameSenderInWindow(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "100")
	a := notice(1, "alerts@bank.example.test", "Deposit received", noticeDay)
	b := notice(2, "Alerts@Bank.example.test", "Deposit received", noticeDay.Add(6*24*time.Hour))
	other := notice(3, "news@shop.example.test", "Deposit received", noticeDay.Add(24*time.Hour))
	late := notice(4, "alerts@bank.example.test", "Deposit received", noticeDay.Add(20*24*time.Hour))
	syncNotices(t, db, a, b, other, late)
	assertGroups(t, db, "aabc", a, b, other, late)
}

// The window follows the conversation: each notice within a week of the
// one before keeps joining, however old the first one is.
func TestSubjectThreadingWindowFollowsConversation(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "0")
	var notices []SyncMessage
	for i := range 6 {
		notices = append(notices, notice(uint32(i+1), "alerts@bank.example.test", "Weekly summary", noticeDay.Add(time.Duration(i*6*24)*time.Hour)))
	}
	syncNotices(t, db, notices...)
	assertGroups(t, db, "aaaaaa", notices...)
}

// Mail synced newest first still groups.
func TestSubjectThreadingGroupsOutOfOrderSync(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "100")
	a := notice(1, "alerts@bank.example.test", "Deposit received", noticeDay)
	b := notice(2, "alerts@bank.example.test", "Deposit received", noticeDay.Add(24*time.Hour))
	syncNotices(t, db, b, a)
	assertGroups(t, db, "aa", a, b)
}

func TestSubjectThreadingReplyNeedsSharedParticipant(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "100")
	first := notice(1, "lena@example.test", "Garden club", noticeDay)
	reply := notice(2, "omar@example.test", "Re: Garden club", noticeDay.Add(time.Hour))
	reply.CCRecipients = []Recipient{{Email: "lena@example.test"}}
	stranger := notice(3, "ines@example.test", "Re: Garden club", noticeDay.Add(2*time.Hour))
	syncNotices(t, db, first, reply, stranger)
	assertGroups(t, db, "aab", first, reply, stranger)
}

// The account's own mail with one subject to different people stays apart.
func TestSubjectThreadingOwnMailNeedsSharedRecipient(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "100")
	toLena := notice(1, "reader@example.test", "Invoice", noticeDay)
	toLena.ToRecipients = []Recipient{{Email: "lena@example.test"}}
	toOmar := notice(2, "reader@example.test", "Invoice", noticeDay.Add(time.Hour))
	toOmar.ToRecipients = []Recipient{{Email: "omar@example.test"}}
	toLenaAgain := notice(3, "reader@example.test", "Invoice", noticeDay.Add(2*time.Hour))
	toLenaAgain.ToRecipients = []Recipient{{Email: "lena@example.test"}}
	syncNotices(t, db, toLena, toOmar, toLenaAgain)
	assertGroups(t, db, "aba", toLena, toOmar, toLenaAgain)
}

// A full conversation is left alone and the next notice starts another,
// which the ones after it join.
func TestSubjectThreadingSizeLimit(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "3")
	var notices []SyncMessage
	for i := range 5 {
		notices = append(notices, notice(uint32(i+1), "alerts@bank.example.test", "Deposit received", noticeDay.Add(time.Duration(i)*time.Hour)))
	}
	syncNotices(t, db, notices...)
	assertGroups(t, db, "aaabb", notices...)
}

// A reply joins the conversation it answers however long ago that was,
// unless the user turns that off; other mail still keeps to the window.
func TestSubjectThreadingLateReplies(t *testing.T) {
	for _, tc := range []struct {
		lateReplies string
		want        string
	}{
		{"", "aaab"},
		{"true", "aaab"},
		{"false", "abcd"},
	} {
		t.Run("late_replies="+tc.lateReplies, func(t *testing.T) {
			db := newFolderThreadTestDB(t)
			settings := map[string]string{"subject_threading": "true", "subject_threading_window_days": "7", "subject_threading_max_messages": "100"}
			if tc.lateReplies != "" {
				settings["subject_threading_late_replies"] = tc.lateReplies
			}
			if err := db.MergeUISettings(t.Context(), "reader", settings); err != nil {
				t.Fatal(err)
			}
			first := notice(1, "lena@example.test", "Garden club", noticeDay)
			reply := notice(2, "omar@example.test", "Re: Garden club", noticeDay.Add(40*24*time.Hour))
			reply.CCRecipients = []Recipient{{Email: "lena@example.test"}}
			again := notice(3, "lena@example.test", "AW: Garden club", noticeDay.Add(200*24*time.Hour))
			again.ToRecipients = []Recipient{{Email: "omar@example.test"}}
			later := notice(4, "lena@example.test", "Garden club", noticeDay.Add(300*24*time.Hour))
			syncNotices(t, db, first, reply, again, later)
			assertGroups(t, db, tc.want, first, reply, again, later)
		})
	}
}

func TestRegroupSubjectThreads(t *testing.T) {
	db := newFolderThreadTestDB(t)
	// A chain where each notice is within the window only of its
	// neighbours, and a reply by headers that moves with its parent.
	a := notice(1, "alerts@bank.example.test", "Deposit received", noticeDay)
	b := notice(2, "alerts@bank.example.test", "Deposit received", noticeDay.Add(4*24*time.Hour))
	c := notice(3, "alerts@bank.example.test", "Deposit received", noticeDay.Add(8*24*time.Hour))
	reply := notice(4, "reader@example.test", "Re: Deposit received", noticeDay.Add(9*24*time.Hour))
	reply.ToRecipients = []Recipient{{Email: "alerts@bank.example.test"}}
	reply.InReplyTo, reply.References = c.MessageID, c.MessageID
	syncNotices(t, db, a, b, c, reply)
	assertGroups(t, db, "abcc", a, b, c, reply)

	setSubjectThreading(t, db, true, "7", "100")
	regroup(t, db)
	assertGroups(t, db, "aaaa", a, b, c, reply)
	if n := threadRowCount(t, db); n != 1 {
		t.Fatalf("%d thread rows after grouping, want 1", n)
	}

	setSubjectThreading(t, db, false, "7", "100")
	regroup(t, db)
	assertGroups(t, db, "abcc", a, b, c, reply)
	if n := threadRowCount(t, db); n != 3 {
		t.Fatalf("%d thread rows after splitting, want 3", n)
	}
}

func TestParseSubjectThreading(t *testing.T) {
	for _, tc := range []struct {
		enabled, window, max, late string
		want                       SubjectThreading
	}{
		{"", "", "", "", SubjectThreading{false, 7, 100, true}},
		{"true", "30", "0", "false", SubjectThreading{true, 30, 0, false}},
		{"true", "0", "1", "true", SubjectThreading{true, 1, 2, true}},
		{"true", "9999", "99999", "", SubjectThreading{true, 365, 10000, true}},
		{"true", "week", "-5", "maybe", SubjectThreading{true, 7, 100, true}},
	} {
		if got := ParseSubjectThreading(tc.enabled, tc.window, tc.max, tc.late); got != tc.want {
			t.Errorf("ParseSubjectThreading(%q, %q, %q, %q) = %+v, want %+v", tc.enabled, tc.window, tc.max, tc.late, got, tc.want)
		}
	}
}

// replyToMissing is a notice whose reply headers point only to a message
// the mailbox does not have, a different one for each notice.
func replyToMissing(uid uint32, from, subject string, sent time.Time) SyncMessage {
	m := notice(uid, from, subject, sent)
	m.InReplyTo = fmt.Sprintf("<missing-%d@example.test>", uid)
	m.References = m.InReplyTo
	return m
}

// Mail whose reply headers lead nowhere is grouped by subject like mail
// without them, and only when the user groups by subject.
func TestSubjectThreadingGroupsMailWithMissingReferences(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		want    string
	}{
		{true, "aab"},
		{false, "abc"},
	} {
		db := newFolderThreadTestDB(t)
		setSubjectThreading(t, db, tc.enabled, "7", "100")
		a := replyToMissing(1, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay)
		b := replyToMissing(2, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay.Add(24*time.Hour))
		other := replyToMissing(3, "news@shop.example.test", "Ticket 42: Printer jams", noticeDay.Add(48*time.Hour))
		syncNotices(t, db, a, b, other)
		assertGroups(t, db, tc.want, a, b, other)
	}
}

// Reply headers that lead to saved mail still win over the subject.
func TestSubjectThreadingKeepsReferencesToSavedMail(t *testing.T) {
	db := newFolderThreadTestDB(t)
	setSubjectThreading(t, db, true, "7", "100")
	first := notice(1, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay)
	saved := notice(2, "lena@example.test", "Toner order", noticeDay.Add(time.Hour))
	answer := notice(3, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay.Add(2*time.Hour))
	answer.InReplyTo, answer.References = saved.MessageID, saved.MessageID
	syncNotices(t, db, first, saved, answer)
	assertGroups(t, db, "abb", first, saved, answer)
}

// Regroup applies grouping by subject to saved mail whose reply headers
// lead nowhere, and splits it again.
func TestRegroupSubjectThreadsWithMissingReferences(t *testing.T) {
	db := newFolderThreadTestDB(t)
	a := replyToMissing(1, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay)
	b := replyToMissing(2, "desk@support.example.test", "Ticket 42: Printer jams", noticeDay.Add(24*time.Hour))
	syncNotices(t, db, a, b)
	assertGroups(t, db, "ab", a, b)

	setSubjectThreading(t, db, true, "7", "100")
	regroup(t, db)
	assertGroups(t, db, "aa", a, b)

	setSubjectThreading(t, db, false, "7", "100")
	regroup(t, db)
	assertGroups(t, db, "ab", a, b)
	if n := threadRowCount(t, db); n != 2 {
		t.Fatalf("%d thread rows after splitting, want 2", n)
	}
}
