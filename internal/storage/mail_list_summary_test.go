package storage

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type listedEmail struct {
	ID, Folder, Account       string
	Read, Starred, Attachment bool
	ThreadCount               int
}

func listedEmails(emails []models.Email) []listedEmail {
	out := make([]listedEmail, 0, len(emails))
	for _, e := range emails {
		out = append(out, listedEmail{e.ID, e.FolderID, e.AccountID, e.IsRead, e.IsStarred, e.HasAttachment, e.ThreadCount})
	}
	return out
}

// Lists answered from folder_thread_state must match the general query that
// groups every message, for every filter and sort the summary supports.
func TestSummaryListsMatchGeneralQuery(t *testing.T) {
	db := newSummaryListFixture(t)
	ctx := context.Background()

	var cases []models.EmailFilters
	for _, sort := range [][2]string{{"", ""}, {"date", "asc"}, {"sender", "asc"}, {"sender", "desc"}, {"subject", "asc"}} {
		for _, f := range []models.EmailFilters{
			{}, {Unread: true}, {Read: true}, {Starred: true}, {Attachments: true}, {NoAttach: true},
			{ThreadsOnly: true}, {NoThreads: true}, {Unread: true, Starred: true}, {AccountID: "second"},
			{Unread: true, Attachments: true, ThreadsOnly: true},
			// Searches keep conversations with a match in any message.
			{Query: "invoice"}, {Query: "inv"}, {Query: "subject 7"}, {Body: "budget"},
			{Query: "invoice", Body: "budget"}, {Query: "invoice", Unread: true, AccountID: "second"},
			{Query: "nothing-matches-this"}, {Query: "!!"}, {Query: "collision"},
		} {
			f.SortBy, f.SortOrder = sort[0], sort[1]
			cases = append(cases, f)
		}
	}
	compared := 0
	for _, folder := range []string{"box-inbox", "box-archive", "inbox", "archive"} {
		for _, filters := range cases {
			name := fmt.Sprintf("%s %+v", folder, filters)
			if !summaryFiltersOnly(filters) {
				t.Fatalf("%s: not served from summaries", name)
			}
			var want []models.Email
			var err error
			var page *models.EmailPage
			var count int
			if isUnifiedFolderID(folder) {
				want, err = db.listEmailsFilteredForUser(ctx, "reader", folder, 0, 1000, filters)
				if err == nil {
					page, err = db.GetEmailsRangeFilteredForUser(ctx, "reader", folder, 0, 1000, filters)
				}
				if err == nil {
					count, err = db.GetFolderEmailCountFilteredForUser(ctx, "reader", folder, filters)
				}
			} else {
				want, err = db.listEmailsFiltered(ctx, folder, 0, 1000, filters)
				if err == nil {
					page, err = db.GetEmailsRangeFiltered(ctx, folder, 0, 1000, filters)
				}
				if err == nil {
					count, err = db.GetFolderEmailCountFiltered(ctx, folder, filters)
				}
			}
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(want) > 0 {
				compared++
			}
			if !reflect.DeepEqual(listedEmails(page.Emails), listedEmails(want)) {
				t.Fatalf("%s: summary list differs\n got: %+v\nwant: %+v", name, listedEmails(page.Emails), listedEmails(want))
			}
			if count != len(want) || page.TotalCount != len(want) {
				t.Fatalf("%s: count %d, page total %d, want %d", name, count, page.TotalCount, len(want))
			}
		}
	}
	if compared < len(cases)*2 {
		t.Fatalf("only %d of %d comparisons had matching mail; the fixture is too sparse", compared, len(cases)*4)
	}
}

func TestSummaryListsLeaveOtherFiltersToGeneralQuery(t *testing.T) {
	for _, filters := range []models.EmailFilters{
		{Query: "invoice", From: "ana"}, {From: "ana"}, {Subject: "x"}, {Tag: "work"}, {After: "2024-01-01"},
		{Participant: "ana@example.test"}, {MinSizeBytes: 10}, {Unread: true, From: "ana"}, {SortBy: "size"},
	} {
		if summaryFiltersOnly(filters) {
			t.Errorf("%+v served from summaries", filters)
		}
	}
	db := newFolderThreadTestDB(t)
	for _, folder := range []string{"starred", "scheduled", "starred-acct"} {
		_, ok, err := db.summaryListScope(context.Background(), "reader", folder, models.EmailFilters{Unread: true})
		if err != nil || ok {
			t.Errorf("%s served from summaries: %v", folder, err)
		}
	}
}

// newSummaryListFixture fills two accounts with 400 messages in conversations
// of varying size, spread over folders with mixed flags.
func newSummaryListFixture(t *testing.T) *DB {
	t.Helper()
	db := newFolderThreadTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO accounts(id, user_id, email_address) VALUES ('second', 'reader', 'second@example.test');
		INSERT INTO folders(id, account_id, remote_id, name, role) VALUES ('second-inbox', 'second', 'INBOX', 'Inbox', 'box-inbox');`); err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(11))
	senders := []string{"Ana", "", "bruno", "Carla", "ana"}
	for id := 1; id <= 400; id++ {
		account, folder := "acct", []string{"box-inbox", "box-inbox", "box-archive"}[random.Intn(3)]
		if random.Intn(4) == 0 {
			account, folder = "second", "second-inbox"
		}
		thread := ""
		if random.Intn(3) > 0 {
			thread = fmt.Sprintf("%s-thread-%d", account, random.Intn(60))
		}
		if _, err := db.Write().Exec(`INSERT INTO messages(id, account_id, thread_id, subject, from_name, from_email,
				date_received, has_attachments)
			VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, datetime('2024-01-01', '+' || ? || ' minutes'), ?)`,
			id, account, thread, fmt.Sprintf("Subject %d", random.Intn(30)),
			senders[random.Intn(len(senders))], fmt.Sprintf("s%d@example.test", random.Intn(9)), random.Intn(100000), random.Intn(4) == 0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id, folder_id, is_read, is_starred, is_deleted)
			VALUES (?, ?, ?, ?, ?)`, id, folder, random.Intn(4) > 0, random.Intn(6) == 0, random.Intn(15) == 0); err != nil {
			t.Fatal(err)
		}
	}
	// The same conversation key in another account: a match there must not
	// bring the first account's conversation into a search.
	if _, err := db.Write().Exec(`INSERT INTO messages(id, account_id, thread_id, subject, from_name, from_email, date_received)
		SELECT 401, 'second', thread_id, 'Shared key', 'Dora', 'dora@example.test', datetime('2024-01-02')
		FROM messages WHERE account_id = 'acct' AND thread_id IS NOT NULL ORDER BY id LIMIT 1;
		INSERT INTO message_folder_state(message_id, folder_id, is_read) VALUES (401, 'second-inbox', 1);`); err != nil {
		t.Fatal(err)
	}
	// Index the mail for search: some bodies mention an invoice, others a
	// budget, a few both.
	if _, err := db.Write().Exec(`INSERT INTO message_search(rowid, account_id, thread_key, subject, sender, recipients, snippet, body, attachment_names)
		SELECT id, account_id, COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)), subject, from_name || ' ' || from_email, '', '',
		  CASE WHEN id = 401 THEN 'a collision of keys' WHEN id % 7 = 0 THEN 'the invoice is attached' WHEN id % 7 = 1 THEN 'about the budget'
		    WHEN id % 7 = 2 THEN 'invoice and budget' ELSE 'see you soon' END, ''
		FROM messages`); err != nil {
		t.Fatal(err)
	}
	return db
}

// Positions found through the summary index must match where the
// conversation actually appears in the list.
func TestSummaryPositionMatchesListOrder(t *testing.T) {
	db := newSummaryListFixture(t)
	ctx := context.Background()
	for _, folder := range []string{"box-inbox", "box-archive", "inbox", "archive"} {
		var page *models.EmailPage
		var err error
		if isUnifiedFolderID(folder) {
			page, err = db.GetEmailsRangeForUser(ctx, "reader", folder, 0, 1000)
		} else {
			page, err = db.GetEmailsRange(ctx, folder, 0, 1000)
		}
		if err != nil {
			t.Fatal(err)
		}
		rowOf := make(map[string]int)
		for i := len(page.Emails) - 1; i >= 0; i-- {
			key := page.Emails[i].ThreadID
			if key == "" {
				key = "msg:" + page.Emails[i].ID
			}
			rowOf[key] = i
		}
		found := 0
		for id := int64(1); id <= 400; id++ {
			var key string
			if err := db.Read().QueryRow(`SELECT COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)) FROM messages WHERE id = ?`, id).Scan(&key); err != nil {
				t.Fatal(err)
			}
			want, listed := rowOf[key]
			if listed {
				found++
			}
			emailID := fmt.Sprint(id)
			var got int
			if isUnifiedFolderID(folder) {
				got, err = db.findEmailPositionForUser(ctx, "reader", folder, emailID)
			} else {
				got, err = db.findEmailPosition(ctx, folder, emailID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s message %d: position %d, list shows it at %d (listed %v)", folder, id, got, want, listed)
			}
		}
		if found < 50 {
			t.Fatalf("%s: only %d messages listed; the fixture is too sparse", folder, found)
		}
	}
}
