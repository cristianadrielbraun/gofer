package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newMailSearchHandler(t *testing.T) (*Handler, *storage.DB) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, nil, nil, nil)
	_, err := db.Write().Exec(`
		INSERT INTO folders(id, account_id, remote_id, name, role, parent_id) VALUES
			('root', 'victim-account', 'Projects', 'Projects', 'custom', NULL),
			('child', 'victim-account', 'Projects/Client', 'Client', 'custom', 'root'),
			('trash', 'victim-account', 'Trash', 'Trash', 'trash', NULL),
			('foreign', 'attacker-account', 'INBOX', 'Inbox', 'inbox', NULL);
		INSERT INTO messages(id, account_id, thread_id, subject, date_received) VALUES
			(1, 'victim-account', 'conversation', 'needle original', '2026-10-01 12:00:00'),
			(2, 'victim-account', 'conversation', 'needle reply', '2026-10-02 12:00:00'),
			(3, 'attacker-account', 'conversation', 'needle private', '2026-10-03 12:00:00'),
			(4, 'victim-account', 'conversation', 'needle discarded', '2026-10-04 12:00:00');
		INSERT INTO message_folder_state(message_id, folder_id, remote_uid) VALUES
			(1, 'root', 1), (2, 'child', 2), (3, 'foreign', 3), (4, 'trash', 4);
		INSERT INTO message_search(rowid, account_id, thread_key, subject)
			SELECT id, account_id, thread_id, subject FROM messages;`)
	if err != nil {
		t.Fatal(err)
	}
	return h, db
}

func TestMailSearchScopeParsing(t *testing.T) {
	for _, tt := range []struct {
		query string
		want  models.MailSearchScope
	}{
		{"", models.MailSearchScope{Kind: models.MailSearchAll}},
		{"scope=account&scope_account_id=a", models.MailSearchScope{Kind: models.MailSearchAccount, AccountID: "a"}},
		{"scope=folders&scope_folder_id=root&scope_folder_id=child&include_descendants=1&exclude_spam_trash=1", models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root", "child"}, IncludeDescendants: true, ExcludeSpamTrash: true}},
	} {
		got, err := parseMailSearchScope(httptest.NewRequest("GET", "/mail/search?"+tt.query, nil))
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: scope=%+v error=%v", tt.query, got, err)
		}
	}
	for _, query := range []string{"scope=current", "scope=account", "scope=folders", "scope_folder_id=root", "scope=folders&scope_folder_id=", "scope=all&include_descendants=1", "exclude_spam_trash=true"} {
		if _, err := parseMailSearchScope(httptest.NewRequest("GET", "/mail/search?"+query, nil)); !errors.Is(err, errInvalidMailSearch) {
			t.Fatalf("%s: error=%v", query, err)
		}
	}
}

func TestMailSearchRoutes(t *testing.T) {
	h, db := newMailSearchHandler(t)
	if err := db.MergeUISettings(t.Context(), "owner", map[string]string{"group_conversations": "false"}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, tt := range []struct {
		path   string
		htmx   bool
		status int
		want   []string
		absent []string
	}{
		{"/mail/search?q=needle", false, 200, []string{"<html", "Search results", "data-mail-search-scope=", "data-total-count=\"3\""}, []string{"needle private"}},
		{"/mail/search?q=needle", true, 200, []string{"id=\"main-content\"", "Search results"}, []string{"<html", "needle private"}},
		{"/search?q=needle", false, 200, []string{"Search results"}, []string{"needle private"}},
		{"/mail/search/items?scope=folders&scope_folder_id=root&include_descendants=1&q=needle&start=1&limit=1", false, 200, []string{"data-total-count=\"2\"", "data-window-start=\"1\"", "data-email-id=\"1\"", "data-folder-id=\"root\""}, []string{"data-email-id=\"2\"", "needle private"}},
		{"/mail/search/items?scope=account&scope_account_id=attacker-account", false, 404, nil, []string{"needle private"}},
		{"/mail/search/thread/2/subitems?scope=folders&scope_folder_id=root&include_descendants=1", false, 200, []string{"data-sub-email-id=\"1\"", "data-sub-email-id=\"2\""}, []string{"data-sub-email-id=\"3\"", "data-sub-email-id=\"4\""}},
		{"/mail/search/thread/3/subitems", false, 404, nil, []string{"needle private"}},
		{"/mail/search?scope=folders&scope_folder_id=foreign", true, 404, nil, []string{"needle private"}},
		{"/mail/search/ids?scope=account", false, 400, nil, nil},
		{"/mail/search?scope=folders", false, 400, nil, nil},
		{"/mail/search/items?scope=unknown", false, 400, nil, nil},
	} {
		t.Run(tt.path+strconv.FormatBool(tt.htmx), func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			if tt.htmx {
				req.Header.Set("HX-Request", "true")
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, ownerRequest(req))
			if rec.Code != tt.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(rec.Body.String(), absent) {
					t.Errorf("unexpected %q", absent)
				}
			}
		})
	}
}

func TestMailSearchIDsIncludeFolderContextAndGrouping(t *testing.T) {
	h, db := newMailSearchHandler(t)
	for _, grouped := range []bool{true, false} {
		if err := db.MergeUISettings(t.Context(), "owner", map[string]string{"group_conversations": strconv.FormatBool(grouped)}); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.handleMailSearchIDs(rec, ownerRequest(httptest.NewRequest("GET", "/mail/search/ids?scope=folders&scope_folder_id=root&include_descendants=1&q=needle", nil)))
		var result mailItemIDsResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		wantTotal := 2
		if grouped {
			wantTotal = 1
		}
		if result.Total != wantTotal || len(result.IDs) != wantTotal || result.Truncated || result.IDs[0].ID != "2" || result.IDs[0].FolderID != "child" || result.IDs[0].Thread != grouped {
			t.Fatalf("grouped=%t result=%+v", grouped, result)
		}
	}
}

func TestMailSearchContinuesBeyondFirstFiftyResults(t *testing.T) {
	h, db := newMailSearchHandler(t)
	if err := db.MergeUISettings(t.Context(), "owner", map[string]string{"group_conversations": "false"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		WITH RECURSIVE numbers(id) AS (VALUES(10) UNION ALL SELECT id+1 FROM numbers WHERE id<69)
		INSERT INTO messages(id, account_id, subject) SELECT id, 'victim-account', 'Extra result' FROM numbers;
		INSERT INTO message_folder_state(message_id, folder_id, remote_uid) SELECT id, 'root', id FROM messages WHERE id>=10;`); err != nil {
		t.Fatal(err)
	}
	query := "?scope=folders&scope_folder_id=root&include_descendants=1"
	rec := httptest.NewRecorder()
	h.handleMailSearchIDs(rec, ownerRequest(httptest.NewRequest("GET", "/mail/search/ids"+query, nil)))
	var result mailItemIDsResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Total != 62 || len(result.IDs) != 62 {
		t.Fatalf("IDs status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.handleMailSearchItems(rec, ownerRequest(httptest.NewRequest("GET", "/mail/search/items"+query+"&start=50&limit=50", nil)))
	rows := strings.Count(rec.Body.String(), `class="mail-list-item" data-email-id=`)
	if rec.Code != 200 || rows != 12 || !strings.Contains(rec.Body.String(), `data-total-count="62"`) {
		t.Fatalf("page status=%d rows=%d, want 200 and 12 rows", rec.Code, rows)
	}
	// In the table, the page continues the run of results from Projects that
	// the first page ended with, so no row starts a new location. Every card
	// has its own band.
	rec = httptest.NewRecorder()
	h.handleMailSearchItems(rec, ownerRequest(httptest.NewRequest("GET", "/mail/search/items"+query+"&start=50&limit=50&view=table", nil)))
	if strings.Contains(rec.Body.String(), "data-search-group") || !strings.Contains(rec.Body.String(), `data-search-location="owner@example.com · Projects"`) {
		t.Fatal("a table page in the middle of a run of one location shows its band again")
	}
	rec = httptest.NewRecorder()
	h.handleMailSearchItems(rec, ownerRequest(httptest.NewRequest("GET", "/mail/search/items"+query+"&start=50&limit=50&view=cards", nil)))
	if strings.Count(rec.Body.String(), "data-search-group") != 12 {
		t.Fatal("every search card shows its location band")
	}
}

// A search reopened with a message open, after a reload or Back, starts at
// that message's page instead of the first.
func TestMailSearchStartsAtTheOpenMessage(t *testing.T) {
	h, db := newMailSearchHandler(t)
	if err := db.MergeUISettings(t.Context(), "owner", map[string]string{"group_conversations": "false"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		WITH RECURSIVE numbers(id) AS (VALUES(10) UNION ALL SELECT id+1 FROM numbers WHERE id<69)
		INSERT INTO messages(id, account_id, subject) SELECT id, 'victim-account', 'Extra result' FROM numbers;
		INSERT INTO message_folder_state(message_id, folder_id, remote_uid) SELECT id, 'root', id FROM messages WHERE id>=10;`); err != nil {
		t.Fatal(err)
	}
	// Results run 2, 1, then 69 down to 10, so message 10 is the last of 62.
	for _, htmx := range []bool{false, true} {
		req := httptest.NewRequest("GET", "/mail/search?scope=folders&scope_folder_id=root&include_descendants=1&selected=10", nil)
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		h.handleMailSearch(rec, ownerRequest(req))
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, `data-window-start="36"`) || !strings.Contains(body, `data-email-id="10"`) || strings.Contains(body, `data-email-id="2"`) {
			t.Fatalf("htmx=%t status=%d: the page around message 10 should start at 36", htmx, rec.Code)
		}
	}
}

func TestMailSearchActionsResolveOnlyScopedMembers(t *testing.T) {
	h, _ := newMailSearchHandler(t)
	ctx := ownerRequest(httptest.NewRequest("GET", "/", nil)).Context()
	scope := &models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"root"}, IncludeDescendants: true}
	payload := messageBulkRequest{SearchScope: scope, Targets: []messageBulkTarget{{ID: "2", Thread: true, FolderID: "child"}}}
	groups, err := h.resolveMessageBulkTargets(ctx, payload, "", true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, group := range groups {
		for _, info := range group.Infos {
			got[info.MessageID] = info.FolderID
		}
	}
	if !reflect.DeepEqual(got, map[int64]string{1: "root", 2: "child"}) {
		t.Fatal(got)
	}
	for _, target := range []messageBulkTarget{{ID: "3"}, {ID: "4"}, {ID: "2", FolderID: "root"}} {
		payload.Targets = []messageBulkTarget{target}
		if _, err := h.resolveMessageBulkTargets(ctx, payload, "", false); !errors.Is(err, errMessageTargetNotFound) {
			t.Fatalf("target=%+v error=%v", target, err)
		}
	}
	// Explicit result context must not fall back to another live folder.
	if _, err := h.resolveOwnedMessageTargets(ctx, []messageBulkTarget{{ID: "2", FolderID: "root"}}, "", false); !errors.Is(err, errMessageTargetNotFound) {
		t.Fatalf("nonmember folder error=%v", err)
	}
}

func TestMailSearchDeleteUsesEachFoldersRole(t *testing.T) {
	h, db := newMailSearchHandler(t)
	req := httptest.NewRequest("POST", "/api/messages/delete", strings.NewReader(`{"search_scope":{"kind":"all"},"permanent":true,"targets":[{"id":"4","thread":true,"folder_id":"trash"}]}`))
	rec := httptest.NewRecorder()
	h.handleDeleteMessages(rec, ownerRequest(req))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, tt := range []struct {
		id   int
		kind string
	}{
		{1, storage.MessageMutationMove}, {2, storage.MessageMutationMove}, {4, storage.MessageMutationDelete},
	} {
		var kind string
		if err := db.Read().QueryRow(`SELECT kind FROM message_mutations WHERE message_id=?`, tt.id).Scan(&kind); err != nil || kind != tt.kind {
			t.Fatalf("message=%d kind=%s error=%v, want %s", tt.id, kind, err, tt.kind)
		}
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id=3`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign mutations=%d error=%v", count, err)
	}
}

func TestMailSearchUsesPerUserStorage(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		rec := httptest.NewRecorder()
		f.h.handleMailSearchIDs(rec, ownedContentRequest(owner, "/mail/search/ids", ""))
		var result mailItemIDsResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.IDs) != 1 || result.IDs[0].Account != f.accounts[owner].ID {
			t.Fatalf("owner=%s status=%d body=%s", owner, rec.Code, rec.Body.String())
		}
	}
}

func TestMailSearchDeleteKeepsTrashWithoutPermanentRequest(t *testing.T) {
	h, db := newMailSearchHandler(t)
	req := httptest.NewRequest("POST", "/api/messages/delete", strings.NewReader(`{"search_scope":{"kind":"all"},"targets":[{"id":"4","thread":true}]}`))
	rec := httptest.NewRecorder()
	h.handleDeleteMessages(rec, ownerRequest(req))
	var result struct {
		KeptTrash []messageBulkTarget `json:"kept_trash"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.KeptTrash) != 1 || result.KeptTrash[0].ID != "4" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id=4`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Trash mutations=%d error=%v", count, err)
	}
}

func TestMailSearchFlagsReturnOnlyChangedMembers(t *testing.T) {
	for _, action := range []string{"read", "star"} {
		t.Run(action, func(t *testing.T) {
			h, db := newMailSearchHandler(t)
			req := httptest.NewRequest("POST", "/api/messages/"+action, strings.NewReader(`{"search_scope":{"kind":"folders","folder_ids":["root"],"include_descendants":true},"targets":[{"id":"2","thread":true}]}`))
			rec := httptest.NewRecorder()
			if action == "read" {
				h.handleMarkMessagesRead(rec, ownerRequest(req))
			} else {
				h.handleMarkMessagesStarred(rec, ownerRequest(req))
			}
			var result struct {
				MessageIDs []string `json:"message_ids"`
			}
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			ids := map[string]bool{}
			for _, id := range result.MessageIDs {
				ids[id] = true
			}
			if len(ids) != 2 || !ids["1"] || !ids["2"] {
				t.Fatalf("changed IDs=%v", result.MessageIDs)
			}
			var read, starred int
			if err := db.Read().QueryRow(`SELECT is_read,is_starred FROM message_folder_state WHERE message_id=4`).Scan(&read, &starred); err != nil || read != 0 || starred != 0 {
				t.Fatalf("outside-scope read=%d starred=%d error=%v", read, starred, err)
			}
		})
	}
}

// Personal and managed mode serve signed-in users from the per-user routes.
func TestUserStorageRoutesServeMailSearch(t *testing.T) {
	assertUserStorageRoutes(t, []struct{ method, path string }{
		{http.MethodGet, "/mail/search"},
		{http.MethodGet, "/mail/search/items"},
		{http.MethodGet, "/mail/search/ids"},
		{http.MethodGet, "/mail/search/thread/{id}/subitems"},
	})
}
