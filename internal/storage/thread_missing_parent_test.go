package storage

import (
	"testing"
	"time"
)

const trackerIssue = "<ISSUE-7.root@tracker.example.test>"

// trackerNotice is a notification that replies to an issue message the
// tracker never sends, each with its own extra reference, as Jira's are.
func trackerNotice(uid uint32, subject, extraRef string, sent time.Time) SyncMessage {
	m := notice(uid, "tracker@example.test", subject, sent)
	m.InReplyTo = trackerIssue
	m.References = trackerIssue + " " + extraRef
	return m
}

// Mail replying to a message the mailbox does not have is one conversation,
// whichever order it arrives in.
func TestRepliesToMissingMessageShareConversation(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		db := newFolderThreadTestDB(t)
		a := trackerNotice(1, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.1@node-a.example.test>", noticeDay)
		b := trackerNotice(2, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.2@node-b.example.test>", noticeDay.Add(5*24*time.Hour))
		// The message both reply to: the subject can differ.
		c := trackerNotice(3, "[Tracker] Commented: (ISSUE-7) Printer jams", "<ISSUE-7.3@node-a.example.test>", noticeDay.Add(6*24*time.Hour))
		if reverse {
			syncNotices(t, db, c, b, a)
		} else {
			syncNotices(t, db, a, b, c)
		}
		assertGroups(t, db, "aaa", a, b, c)
		if n := threadRowCount(t, db); n != 1 {
			t.Fatalf("reverse=%v: %d thread rows, want 1", reverse, n)
		}
	}
}

// Sharing only an older missing reference, mail joins only with the same
// subject, as with references to saved mail.
func TestOlderMissingReferenceNeedsSameSubject(t *testing.T) {
	db := newFolderThreadTestDB(t)
	first := trackerNotice(1, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.1@node-a.example.test>", noticeDay)
	other := notice(2, "tracker@example.test", "Weekly digest", noticeDay.Add(time.Hour))
	other.InReplyTo = "<digest-9@tracker.example.test>"
	other.References = trackerIssue + " <digest-9@tracker.example.test>"
	same := notice(3, "tracker@example.test", "Re: [Tracker] (ISSUE-7) Printer jams", noticeDay.Add(2*time.Hour))
	same.InReplyTo = "<ISSUE-7.9@node-c.example.test>"
	same.References = trackerIssue + " <ISSUE-7.9@node-c.example.test>"
	syncNotices(t, db, first, other, same)
	assertGroups(t, db, "aba", first, other, same)
}

// When the missing message arrives, the conversation becomes its replies.
func TestMissingMessageArrivingJoinsItsReplies(t *testing.T) {
	db := newFolderThreadTestDB(t)
	a := trackerNotice(1, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.1@node-a.example.test>", noticeDay.Add(time.Hour))
	b := trackerNotice(2, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.2@node-b.example.test>", noticeDay.Add(2*time.Hour))
	issue := notice(3, "tracker@example.test", "[Tracker] (ISSUE-7) Printer jams", noticeDay)
	issue.MessageID = trackerIssue
	syncNotices(t, db, a, b, issue)
	assertGroups(t, db, "aaa", a, b, issue)
	if n := threadRowCount(t, db); n != 1 {
		t.Fatalf("%d thread rows, want 1", n)
	}
}

// Regroup joins mail that refers to the same missing message, saved apart
// before Gofer grouped it, even without grouping by subject.
func TestRegroupJoinsRepliesToMissingMessage(t *testing.T) {
	db := newFolderThreadTestDB(t)
	a := trackerNotice(1, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.1@node-a.example.test>", noticeDay)
	b := trackerNotice(2, "[Tracker] (ISSUE-7) Printer jams", "<ISSUE-7.2@node-b.example.test>", noticeDay.Add(5*24*time.Hour))
	syncNotices(t, db, a, b)
	// As older versions saved them: each in a conversation of its own.
	if _, err := db.Write().Exec(`INSERT INTO threads (id, account_id, subject, normalized_subject) SELECT 'apart', account_id, subject, normalized_subject FROM messages WHERE internet_message_id = ?`, b.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE messages SET thread_id = 'apart' WHERE internet_message_id = ?`, b.MessageID); err != nil {
		t.Fatal(err)
	}
	assertGroups(t, db, "ab", a, b)

	regroup(t, db)
	assertGroups(t, db, "aa", a, b)
	if n := threadRowCount(t, db); n != 1 {
		t.Fatalf("%d thread rows after regrouping, want 1", n)
	}
}
