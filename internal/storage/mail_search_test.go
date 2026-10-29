package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func mailSearchTestDB(t *testing.T) *DB {
	t.Helper()
	db := newContactsTestDB(t)
	_, err := db.Write().Exec(`
		INSERT INTO users(id, username, username_normalized) VALUES('other', 'other', 'other');
		INSERT INTO accounts(id, user_id, email_address) VALUES
			('a', 'default', 'a@example.com'), ('b', 'default', 'b@example.com'), ('c', 'other', 'c@example.com');
		INSERT INTO folders(id, account_id, parent_id, name, role, discovery_state) VALUES
			('root', 'a', NULL, 'Projects', 'custom', 'active'),
			('child', 'a', 'root', 'Client', 'custom', 'active'),
			('grandchild', 'a', 'child', 'Invoices', 'custom', 'active'),
			('outside', 'a', NULL, 'Elsewhere', 'custom', 'active'),
			('trash', 'a', NULL, 'Trash', 'trash', 'active'),
			('gone', 'a', NULL, 'Missing', 'custom', 'missing'),
			('b-inbox', 'b', NULL, 'Inbox', 'inbox', 'active'),
			('private', 'c', NULL, 'Private', 'custom', 'active');
		INSERT INTO messages(id, account_id, thread_id, subject, date_received, size_bytes) VALUES
			(1, 'a', 'shared', 'needle original', '2026-10-01 12:00:00', 100),
			(2, 'a', 'shared', 'reply', '2026-10-02 12:00:00', 100),
			(3, 'a', 'outside-thread', 'needle elsewhere', '2026-10-03 12:00:00', 100),
			(4, 'b', 'shared', 'needle second account', '2026-10-04 12:00:00', 100),
			(5, 'c', 'shared', 'needle private', '2026-10-05 12:00:00', 100),
			(6, 'a', 'trash-thread', 'needle trash', '2026-10-06 12:00:00', 100),
			(7, 'a', 'missing-thread', 'needle missing', '2026-10-07 12:00:00', 100),
			(8, 'a', 'outside-thread', 'nonmatching reply', '2026-10-08 12:00:00', 100),
			(9, 'a', 'deleted-thread', 'needle deleted', '2026-10-09 12:00:00', 100);
		INSERT INTO message_folder_state(message_id, folder_id, is_read, is_starred, is_deleted) VALUES
			(1, 'root', 1, 0, 0), (1, 'child', 0, 1, 0), (2, 'grandchild', 1, 0, 0),
			(3, 'outside', 1, 0, 0), (4, 'b-inbox', 1, 0, 0), (5, 'private', 1, 0, 0),
			(6, 'trash', 1, 0, 0), (7, 'gone', 1, 0, 0), (8, 'root', 1, 0, 0), (9, 'root', 1, 0, 1);
		INSERT INTO message_search(rowid, account_id, thread_key, subject)
			SELECT id, account_id, thread_id, subject FROM messages;`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMailSearchScopes(t *testing.T) {
	db := mailSearchTestDB(t)
	for _, tt := range []struct {
		name    string
		scope   models.MailSearchScope
		filters models.EmailFilters
		ids     []string
	}{
		{"all messages", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{ByMessage: true}, []string{"8", "6", "4", "3", "2", "1"}},
		{"account", models.MailSearchScope{Kind: models.MailSearchAccount, AccountID: "a"}, models.EmailFilters{ByMessage: true}, []string{"8", "6", "3", "2", "1"}},
		{"exact folder", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root"}}, models.EmailFilters{ByMessage: true}, []string{"8", "1"}},
		{"descendants and overlapping roots", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root", "child", "root"}, IncludeDescendants: true}, models.EmailFilters{ByMessage: true}, []string{"8", "2", "1"}},
		{"multiple accounts", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"child", "b-inbox"}}, models.EmailFilters{ByMessage: true}, []string{"4", "1"}},
		{"exclude trash", models.MailSearchScope{Kind: models.MailSearchAll, ExcludeSpamTrash: true}, models.EmailFilters{ByMessage: true}, []string{"8", "4", "3", "2", "1"}},
		{"conversations separated by account", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{}, []string{"8", "6", "4", "2"}},
		{"matching messages", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{Query: "needle", ByMessage: true}, []string{"6", "4", "3", "1"}},
		{"matching reply inside scope", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"child"}, IncludeDescendants: true}, models.EmailFilters{Query: "needle"}, []string{"2"}},
		{"matching reply outside scope excluded", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root"}}, models.EmailFilters{Query: "needle"}, []string{"1"}},
		{"subject filter composes with search", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{Query: "needle", Subject: "second", ByMessage: true}, []string{"4"}},
		{"explicit folder retained", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root"}}, models.EmailFilters{ByMessage: true, Starred: true}, nil},
		{"account filter intersects scope", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{AccountID: "b", Query: "needle"}, []string{"4"}},
		{"copy flags aggregated once", models.MailSearchScope{Kind: models.MailSearchAll}, models.EmailFilters{Unread: true, Starred: true}, []string{"2"}},
		{"copies do not create conversations", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root", "child"}}, models.EmailFilters{ThreadsOnly: true}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			page, err := db.GetMailSearchPageForUser(context.Background(), "default", tt.scope, 0, 50, tt.filters)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, email := range page.Emails {
				ids = append(ids, email.ID)
				if email.AccountID == "c" || email.FolderID == "private" {
					t.Fatal("foreign result", email)
				}
			}
			if !reflect.DeepEqual(ids, tt.ids) || page.TotalCount != len(tt.ids) || page.DisplayTotalCount != len(tt.ids) {
				t.Fatalf("ids=%v total=%d display=%d, want %v", ids, page.TotalCount, page.DisplayTotalCount, tt.ids)
			}
		})
	}
}

func TestMailSearchRejectsUnavailableScopes(t *testing.T) {
	db := mailSearchTestDB(t)
	for _, scope := range []models.MailSearchScope{
		{}, {Kind: "unknown"},
		{Kind: models.MailSearchAccount},
		{Kind: models.MailSearchAccount, AccountID: "c"},
		{Kind: models.MailSearchAccount, AccountID: "absent"},
		{Kind: models.MailSearchAll, AccountID: "a"},
		{Kind: models.MailSearchAll, IncludeDescendants: true},
		{Kind: models.MailSearchFolders},
		{Kind: models.MailSearchFolders, FolderIDs: []string{" "}},
		{Kind: models.MailSearchFolders, FolderIDs: []string{"root", "private"}},
		{Kind: models.MailSearchFolders, FolderIDs: []string{"root", "absent"}},
		{Kind: models.MailSearchFolders, FolderIDs: []string{"gone"}},
		{Kind: models.MailSearchFolders, FolderIDs: make([]string, MaxMailSearchFolders+1)},
	} {
		t.Run(fmt.Sprint(scope), func(t *testing.T) {
			_, err := db.GetMailSearchPageForUser(context.Background(), "default", scope, 0, 10, models.EmailFilters{})
			if !errors.Is(err, ErrMailSearchScope) {
				t.Fatalf("error=%v, want ErrMailSearchScope", err)
			}
		})
	}
	if _, err := db.GetMailSearchPageForUser(context.Background(), "", models.MailSearchScope{Kind: models.MailSearchAll}, 0, 10, models.EmailFilters{}); !errors.Is(err, ErrMailSearchScope) {
		t.Fatalf("empty user error=%v", err)
	}
}

func TestMailSearchPaginationAndFolderContext(t *testing.T) {
	db := mailSearchTestDB(t)
	scope := models.MailSearchScope{Kind: models.MailSearchAll}
	filters := models.EmailFilters{Query: "needle", ByMessage: true, SortOrder: "asc"}
	var ids []string
	for start := 0; start < 4; start += 2 {
		page, err := db.GetMailSearchPageForUser(context.Background(), "default", scope, start, 2, filters)
		if err != nil {
			t.Fatal(err)
		}
		if page.TotalCount != 4 || page.WindowStart != start || page.WindowEnd != start+1 || page.HasMore != (start == 0) {
			t.Fatalf("unexpected page: %+v", page)
		}
		for _, email := range page.Emails {
			ids = append(ids, email.ID)
			if email.ID == "1" && (email.FolderID != "child" || email.IsRead || !email.IsStarred) {
				t.Fatalf("copy context/flags: %+v", email)
			}
		}
	}
	if !reflect.DeepEqual(ids, []string{"1", "3", "4", "6"}) {
		t.Fatal(ids)
	}
	page, err := db.GetMailSearchPageForUser(context.Background(), "default", scope, 20, 2, filters)
	if err != nil || len(page.Emails) != 0 || page.TotalCount != 4 || page.HasMore {
		t.Fatalf("empty page=%+v error=%v", page, err)
	}
}

func TestMailSearchPageAroundKeepsTheOpenMessageInView(t *testing.T) {
	db := mailSearchTestDB(t)
	all := models.MailSearchScope{Kind: models.MailSearchAll}
	for _, tt := range []struct {
		name      string
		messageID int64
		filters   models.EmailFilters
		paged     bool
		start     int
		ids       []string
	}{
		// Rows by message: 8 6 4 3 2 1.
		{"by message", 1, models.EmailFilters{ByMessage: true}, false, 4, []string{"2", "1"}},
		{"centred", 2, models.EmailFilters{ByMessage: true}, false, 3, []string{"3", "2"}},
		// Pages of 2 start at 0, 2, 4; message 2 is row 4.
		{"on its page when paginated", 2, models.EmailFilters{ByMessage: true}, true, 4, []string{"2", "1"}},
		// Rows by conversation: 8 6 4 2. Message 1 is an older part of 2's.
		{"older message of a conversation", 1, models.EmailFilters{}, false, 2, []string{"4", "2"}},
		{"another user's message", 5, models.EmailFilters{ByMessage: true}, false, 0, []string{"8", "6"}},
		{"no such message", 999, models.EmailFilters{ByMessage: true}, false, 0, []string{"8", "6"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			page, err := db.GetMailSearchPageAroundForUser(context.Background(), "default", all, tt.messageID, 2, tt.filters, tt.paged)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, email := range page.Emails {
				ids = append(ids, email.ID)
			}
			if page.WindowStart != tt.start || !reflect.DeepEqual(ids, tt.ids) {
				t.Fatalf("start=%d ids=%v, want start=%d ids=%v", page.WindowStart, ids, tt.start, tt.ids)
			}
		})
	}
}

func TestMailSearchDescendantsTerminateOnCycles(t *testing.T) {
	db := mailSearchTestDB(t)
	if _, err := db.Write().Exec(`UPDATE folders SET parent_id='grandchild' WHERE id='root'`); err != nil {
		t.Fatal(err)
	}
	page, err := db.GetMailSearchPageForUser(context.Background(), "default", models.MailSearchScope{
		Kind: models.MailSearchFolders, FolderIDs: []string{"root"}, IncludeDescendants: true,
	}, 0, 50, models.EmailFilters{ByMessage: true})
	if err != nil || page.TotalCount != 3 {
		t.Fatalf("cyclic tree page=%+v error=%v", page, err)
	}
}

func TestMailSearchDeepTreeDoesNotCrossAccounts(t *testing.T) {
	db := mailSearchTestDB(t)
	parent := "root"
	for depth := 0; depth < 30; depth++ {
		id := fmt.Sprintf("deep-%d", depth)
		if _, err := db.Write().Exec(`INSERT INTO folders(id, account_id, parent_id, name) VALUES(?, 'a', ?, ?)`, id, parent, id); err != nil {
			t.Fatal(err)
		}
		parent = id
	}
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET folder_id=? WHERE message_id=3`, parent); err != nil {
		t.Fatal(err)
	}
	// Even an inconsistent parent relation must not pull in another account.
	if _, err := db.Write().Exec(`UPDATE folders SET parent_id='root' WHERE id IN ('b-inbox', 'private')`); err != nil {
		t.Fatal(err)
	}
	page, err := db.GetMailSearchPageForUser(context.Background(), "default", models.MailSearchScope{
		Kind: models.MailSearchFolders, FolderIDs: []string{"root"}, IncludeDescendants: true,
	}, 0, 50, models.EmailFilters{ByMessage: true})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, email := range page.Emails {
		ids = append(ids, email.ID)
	}
	if !reflect.DeepEqual(ids, []string{"8", "3", "2", "1"}) || page.TotalCount != 4 {
		t.Fatalf("deep tree ids=%v count=%d", ids, page.TotalCount)
	}
}

func TestMailSearchRowIDsHaveIndependentTotalAndFolderContext(t *testing.T) {
	db := mailSearchTestDB(t)
	scope := models.MailSearchScope{Kind: models.MailSearchAll}
	ids, total, err := db.GetMailSearchRowIDsForUser(t.Context(), "default", scope, models.EmailFilters{Query: "needle", ByMessage: true}, 2)
	if err != nil || total != 4 || !reflect.DeepEqual(ids, []MailSearchRowID{
		{ID: "6", Account: "a", FolderID: "trash"}, {ID: "4", Account: "b", FolderID: "b-inbox"},
	}) {
		t.Fatalf("ids=%+v total=%d error=%v", ids, total, err)
	}
	if _, _, err := db.GetMailSearchRowIDsForUser(t.Context(), "default", scope, models.EmailFilters{}, 10002); err == nil {
		t.Fatal("unbounded ID request accepted")
	}
}
