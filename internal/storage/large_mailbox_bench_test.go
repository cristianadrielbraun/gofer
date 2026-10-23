package storage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// BenchmarkLargeMailbox times the repository paths a person hits on a very
// large mailbox: opening and scrolling a folder, filters, search, unread
// counts and single-message actions. It builds one synthetic account with
// GOFER_BENCH_MESSAGES messages (default 200000): 60% Inbox, 30% Archive,
// 10% Sent, three messages per conversation, one in seventeen unread. Each
// message has a 2.4 KB body in the search index; one in three mentions an
// invoice.
//
//	go test ./internal/storage -run '^$' -bench LargeMailbox -benchtime 10x
func BenchmarkLargeMailbox(b *testing.B) {
	messages := 200000
	if value := os.Getenv("GOFER_BENCH_MESSAGES"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1000 {
			b.Fatalf("GOFER_BENCH_MESSAGES must be a number of at least 1000")
		}
		messages = parsed
	}
	ctx := context.Background()
	db, err := New(filepath.Join(b.TempDir(), "large.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	seeded := time.Now()
	seedLargeMailbox(b, db, messages)
	b.Logf("seeded %d messages in %s", messages, time.Since(seeded).Round(time.Millisecond))

	const inbox = "bench-inbox"
	if err := db.RefreshFolderThreadState(ctx, inbox); err != nil {
		b.Fatal(err)
	}
	threads, err := db.GetFolderEmailCount(ctx, inbox)
	if err != nil {
		b.Fatal(err)
	}
	var middleID int64
	if err := db.Read().QueryRowContext(ctx, `SELECT head_message_id FROM folder_thread_state
		WHERE folder_id = ? ORDER BY last_message_at DESC, head_message_id DESC LIMIT 1 OFFSET ?`,
		inbox, threads/2).Scan(&middleID); err != nil {
		b.Fatal(err)
	}
	middle := strconv.FormatInt(middleID, 10)
	b.Logf("inbox has %d conversations", threads)

	measure := func(name string, fn func() error) {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				if err := fn(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N), "ms/op")
		})
	}
	page := func(fn func() (*models.EmailPage, error)) func() error {
		return func() error {
			result, err := fn()
			if err == nil && len(result.Emails) == 0 {
				b.Fatal("page returned no messages")
			}
			return err
		}
	}
	unread := models.EmailFilters{Unread: true}

	measure("OpenFolder", page(func() (*models.EmailPage, error) {
		return db.GetEmailsRange(ctx, inbox, 0, 50)
	}))
	measure("ScrollDeepPage", page(func() (*models.EmailPage, error) {
		return db.GetEmailsRange(ctx, inbox, threads/2, 50)
	}))
	measure("ScrollAfterCursor", page(func() (*models.EmailPage, error) {
		return db.GetEmailsAfterCursor(ctx, inbox, middle, 50, models.EmailFilters{})
	}))
	measure("OpenMessageInList", page(func() (*models.EmailPage, error) {
		return db.GetEmailsAroundEmail(ctx, inbox, middle, 50, models.EmailFilters{})
	}))
	// The same, listing each message on its own.
	byMessage := models.EmailFilters{ByMessage: true}
	inboxMessages, err := db.GetFolderEmailCountFiltered(ctx, inbox, byMessage)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("inbox has %d messages", inboxMessages)
	measure("ByMessage/OpenFolder", page(func() (*models.EmailPage, error) {
		return db.GetEmailsRangeFiltered(ctx, inbox, 0, 50, byMessage)
	}))
	measure("ByMessage/ScrollDeepPage", page(func() (*models.EmailPage, error) {
		return db.GetEmailsRangeFiltered(ctx, inbox, inboxMessages/2, 50, byMessage)
	}))
	measure("ByMessage/ScrollAfterCursor", page(func() (*models.EmailPage, error) {
		return db.GetEmailsAfterCursor(ctx, inbox, middle, 50, byMessage)
	}))
	measure("ByMessage/OpenMessageInList", page(func() (*models.EmailPage, error) {
		return db.GetEmailsAroundEmail(ctx, inbox, middle, 50, byMessage)
	}))
	measure("ByMessage/FilterUnread", page(func() (*models.EmailPage, error) {
		filters := models.EmailFilters{Unread: true, ByMessage: true}
		total, err := db.GetFolderEmailCountFiltered(ctx, inbox, filters)
		if err != nil {
			return nil, err
		}
		return db.GetEmailsRangeFilteredWithTotal(ctx, inbox, 0, 50, filters, total)
	}))
	// The list handler counts first, then loads the page.
	measure("FilterUnread", page(func() (*models.EmailPage, error) {
		total, err := db.GetFolderEmailCountFiltered(ctx, inbox, unread)
		if err != nil {
			return nil, err
		}
		return db.GetEmailsRangeFilteredWithTotal(ctx, inbox, 0, 50, unread, total)
	}))
	measure("Search", func() error {
		_, err := db.SearchMessages(ctx, "bench-user", "subject 42", 50)
		return err
	})
	measure("SearchCommonWord", func() error {
		_, err := db.SearchMessages(ctx, "bench-user", "invoice", 50)
		return err
	})
	// The search box filters the open folder: count, then the first page.
	searchBox := func(query string) func() error {
		filters := models.EmailFilters{Query: query}
		return page(func() (*models.EmailPage, error) {
			total, err := db.GetFolderEmailCountFiltered(ctx, inbox, filters)
			if err != nil {
				return nil, err
			}
			return db.GetEmailsRangeFilteredWithTotal(ctx, inbox, 0, 50, filters, total)
		})
	}
	measure("SearchBoxRareWord", searchBox("sender1234"))
	measure("SearchBoxTwoWords", searchBox("subject 42"))
	measure("SearchBoxCommonWord", searchBox("invoice"))
	measure("SearchBoxPrefix", searchBox("inv"))
	measure("UnreadCounts", func() error {
		_, err := db.GetAllFolderUnreadCounts(ctx, "bench-user")
		return err
	})
	read := false
	measure("MarkRead", func() error {
		read = !read
		return db.SetMessageRead(ctx, middleID, read)
	})
	starred := false
	measure("Star", func() error {
		starred = !starred
		return db.SetMessageStarred(ctx, middleID, starred)
	})
	// Every IMAP poll of a folder ends with this refresh.
	measure("PollRefresh", func() error {
		_, err := db.RefreshFolderUnreadCount(ctx, inbox)
		return err
	})
}

func seedLargeMailbox(b *testing.B, db *DB, messages int) {
	b.Helper()
	statements := []string{
		`INSERT INTO users(id, username, username_normalized) VALUES ('bench-user', 'bench', 'bench')`,
		`INSERT INTO accounts(id, user_id, provider, email_address, display_name, color, initials)
		 VALUES ('bench-account', 'bench-user', 'imap', 'reader@example.test', 'Reader', '#336699', 'R')`,
		`INSERT INTO folders(id, account_id, remote_id, name, role, sort_order) VALUES
		 ('bench-inbox', 'bench-account', 'INBOX', 'Inbox', 'inbox', 0),
		 ('bench-archive', 'bench-account', 'Archive', 'Archive', 'archive', 4),
		 ('bench-sent', 'bench-account', 'Sent', 'Sent', 'sent', 2)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		 INSERT INTO messages(id, account_id, internet_message_id, message_id_normalized,
		   thread_id, subject, normalized_subject, from_name, from_email, date_sent, date_received,
		   snippet, preview_text, size_bytes, has_attachments)
		 SELECT i, 'bench-account', '<m' || i || '@example.test>', 'm' || i || '@example.test',
		   'bench-thread-' || (i / 3), 'Subject ' || (i % 5000), 'subject ' || (i % 5000),
		   'Sender ' || (i % 2000), 'sender' || (i % 2000) || '@example.test',
		   datetime('2015-01-01', '+' || (i * 15) || ' minutes'), datetime('2015-01-01', '+' || (i * 15) || ' minutes'),
		   'A short preview of message ' || i, 'A short preview of message ' || i, 2000 + i % 90000, (i % 10 = 0)
		 FROM n`,
		`INSERT INTO message_folder_state(message_id, folder_id, remote_uid, is_read, is_starred, synced_at)
		 SELECT id, CASE WHEN id % 10 < 6 THEN 'bench-inbox' WHEN id % 10 < 9 THEN 'bench-archive' ELSE 'bench-sent' END,
		   id, (id % 17 <> 0), (id % 97 = 0), CURRENT_TIMESTAMP
		 FROM messages`,
		`INSERT INTO message_recipients(message_id, kind, name, email)
		 SELECT id, 'to', 'Reader', 'reader@example.test' FROM messages`,
		`INSERT INTO message_search(rowid, account_id, thread_key, subject, sender, recipients, snippet, body, attachment_names)
		 SELECT id, account_id, thread_id, subject, from_name || ' ' || from_email, 'Reader reader@example.test', snippet,
		   'Hello, this is message ' || id || CASE WHEN id % 3 = 0 THEN ' about the invoice for last month. ' ELSE '. ' END ||
		   replace(hex(zeroblob(24)), '00', 'The team met on Tuesday to review the plan, the budget and the open questions from the previous week. '),
		   ''
		 FROM messages`,
	}
	for _, statement := range statements {
		var args []any
		if statement[:4] == "WITH" {
			args = []any{messages}
		}
		if _, err := db.Write().Exec(statement, args...); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
}

// BenchmarkSyncBatch times writing one 500-message IMAP sync batch, the unit
// in which new mail and initial downloads reach the database, followed by the
// refresh at the end of a folder sync. Every third message starts a new
// conversation; the rest reply to the previous one.
//
//	go test ./internal/storage -run '^$' -bench SyncBatch -benchtime 40x
func BenchmarkSyncBatch(b *testing.B) {
	db, err := New(filepath.Join(b.TempDir(), "sync.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES ('bench-user', 'bench', 'bench');
		INSERT INTO accounts(id, user_id, email_address) VALUES ('bench-account', 'bench-user', 'reader@example.test');
		INSERT INTO folders(id, account_id, remote_id, name, role) VALUES ('bench-inbox', 'bench-account', 'INBOX', 'Inbox', 'inbox');`); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	for b.Loop() {
		batch := make([]SyncMessage, 500)
		for i := range batch {
			n++
			parent := ""
			if n%3 != 0 {
				parent = "<m" + strconv.Itoa(n-1) + "@example.test>"
			}
			batch[i] = SyncMessage{
				AccountID: "bench-account", FolderID: "bench-inbox", RemoteUID: uint32(n),
				MessageID: "<m" + strconv.Itoa(n) + "@example.test>", InReplyTo: parent, References: parent,
				Subject: "Subject " + strconv.Itoa(n/3), FromName: "Sender", FromEmail: "sender" + strconv.Itoa(n%50) + "@example.test",
				DateSent: base.Add(time.Duration(n) * time.Minute), Snippet: "A short preview", IsRead: n%7 != 0,
				ToRecipients: []Recipient{{Name: "Reader", Email: "reader@example.test"}},
			}
		}
		if err := db.UpsertSyncMessages(ctx, batch); err != nil {
			b.Fatal(err)
		}
		if _, err := db.RefreshFolderUnreadCount(ctx, "bench-inbox"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N), "ms/batch")
}
