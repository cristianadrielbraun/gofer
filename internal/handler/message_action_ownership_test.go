package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/translation"
)

type countingTranslationTransport struct {
	calls atomic.Int32
}

func (transport *countingTranslationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("translation must not be called")),
		Request:    req,
	}, nil
}

type messageActionOwnershipFixture struct {
	handler          *Handler
	db               *storage.DB
	victimMessageID  int64
	foreignMessageID int64
	bodyPath         string
	remoteCalls      *atomic.Int32
	translationCalls *countingTranslationTransport
}

func newMessageActionOwnershipFixture(t *testing.T) messageActionOwnershipFixture {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, nil, nil, nil)

	translationTransport := &countingTranslationTransport{}
	h.googleTranslator = translation.NewGoogleWebConnector(&http.Client{Transport: translationTransport})

	fixture := insertVictimReadableMessage(t, h, db)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{
		{ID: "victim-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "victim-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "victim-spam", AccountID: "victim-account", RemoteID: "Spam", Name: "Spam", Role: "spam", Selectable: true},
		{ID: "attacker-inbox", AccountID: "attacker-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "attacker-archive", AccountID: "attacker-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "attacker-trash", AccountID: "attacker-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "attacker-spam", AccountID: "attacker-account", RemoteID: "Spam", Name: "Spam", Role: "spam", Selectable: true},
	}); err != nil {
		t.Fatalf("seed action folders: %v", err)
	}

	const foreignMessageID = int64(102)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO messages (
			id, account_id, internet_message_id, thread_id, subject, from_email, snippet
		) VALUES (
			?, 'attacker-account', '<attacker-action@example.com>', 'attacker-thread',
			'Attacker message', 'attacker-sender@example.com', 'attacker preview'
		);
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		VALUES (?, 'attacker-inbox', 102)`,
		foreignMessageID, foreignMessageID,
	); err != nil {
		t.Fatalf("seed attacker message: %v", err)
	}

	var bodyPath string
	if err := db.Read().QueryRowContext(t.Context(),
		`SELECT body_html_path FROM messages WHERE id = ?`,
		fixture.messageID,
	).Scan(&bodyPath); err != nil {
		t.Fatalf("query victim body path: %v", err)
	}

	result := messageActionOwnershipFixture{
		handler:          h,
		db:               db,
		victimMessageID:  fixture.messageID,
		foreignMessageID: foreignMessageID,
		bodyPath:         bodyPath,
		remoteCalls:      &atomic.Int32{},
		translationCalls: translationTransport,
	}
	h.remoteResourceDownloader = func(string) ([]byte, error) {
		result.remoteCalls.Add(1)
		return []byte("remote image"), nil
	}
	if err := os.WriteFile(bodyPath, []byte(`<p>victim body</p><img src="" data-remote-src="https://remote.example/pixel.png">`), 0o600); err != nil {
		t.Fatalf("write remote-content body: %v", err)
	}
	return result
}

func (fixture messageActionOwnershipFixture) assertVictimUnchanged(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	var isRead, isStarred, isDeleted int
	if err := fixture.db.Read().QueryRowContext(ctx, `
		SELECT is_read, is_starred, is_deleted
		FROM message_folder_state
		WHERE message_id = ? AND folder_id = 'victim-inbox'`,
		fixture.victimMessageID,
	).Scan(&isRead, &isStarred, &isDeleted); err != nil {
		t.Fatalf("query victim folder state: %v", err)
	}

	var bodyPath string
	var attachments, mutations, labels, messageAllows, senderAllows, senderMarkers int
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT body_html_path FROM messages WHERE id = ?`,
		fixture.victimMessageID,
	).Scan(&bodyPath); err != nil {
		t.Fatalf("query victim body path: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`,
		fixture.victimMessageID,
	).Scan(&attachments); err != nil {
		t.Fatalf("query victim attachments: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_mutations WHERE message_id = ?`,
		fixture.victimMessageID,
	).Scan(&mutations); err != nil {
		t.Fatalf("query victim mutations: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_labels WHERE message_id = ?`,
		fixture.victimMessageID,
	).Scan(&labels); err != nil {
		t.Fatalf("query victim labels: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_content_messages WHERE message_id = ?`,
		fixture.victimMessageID,
	).Scan(&messageAllows); err != nil {
		t.Fatalf("query victim remote-content allow: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_content_senders`,
	).Scan(&senderAllows); err != nil {
		t.Fatalf("query sender remote-content allows: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM app_settings
		WHERE key LIKE 'remote_content_sender_allow_%'`,
	).Scan(&senderMarkers); err != nil {
		t.Fatalf("query sender remote-content markers: %v", err)
	}

	if isRead != 0 || isStarred != 0 || isDeleted != 0 ||
		bodyPath != fixture.bodyPath || attachments != 1 || mutations != 0 ||
		labels != 0 || messageAllows != 0 || senderAllows != 0 || senderMarkers != 0 {
		t.Fatalf(
			"victim state changed read=%d starred=%d deleted=%d body=%q attachments=%d mutations=%d labels=%d message_allows=%d sender_allows=%d sender_markers=%d",
			isRead, isStarred, isDeleted, bodyPath, attachments, mutations, labels, messageAllows, senderAllows, senderMarkers,
		)
	}
	if calls := fixture.remoteCalls.Load(); calls != 0 {
		t.Fatalf("foreign action made %d remote-content request(s)", calls)
	}
	if calls := fixture.translationCalls.calls.Load(); calls != 0 {
		t.Fatalf("foreign action made %d translation request(s)", calls)
	}
}

type messageActionRequest struct {
	name        string
	method      string
	path        string
	pathValues  map[string]string
	body        string
	contentType string
	handle      http.HandlerFunc
}

func foreignMessageActionRequests(fixture messageActionOwnershipFixture) []messageActionRequest {
	id := strconv.FormatInt(fixture.victimMessageID, 10)
	bulk := func(extra string) string {
		return `{"targets":[{"id":"` + id + `"}]` + extra + `}`
	}
	return []messageActionRequest{
		{name: "toggle read", method: http.MethodPost, path: "/api/messages/" + id + "/read", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleToggleRead},
		{name: "toggle star", method: http.MethodPost, path: "/api/messages/" + id + "/star", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleToggleStar},
		{name: "toggle thread read", method: http.MethodPost, path: "/api/messages/" + id + "/thread/read", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleToggleThreadRead},
		{name: "archive thread", method: http.MethodPost, path: "/api/messages/" + id + "/thread/archive", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleArchiveThread},
		{name: "delete thread", method: http.MethodDelete, path: "/api/messages/" + id + "/thread", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleDeleteThread},
		{name: "delete message", method: http.MethodDelete, path: "/api/messages/" + id, pathValues: map[string]string{"id": id}, handle: fixture.handler.handleDeleteMessage},
		{name: "move message", method: http.MethodPost, path: "/api/messages/" + id + "/move", pathValues: map[string]string{"id": id}, body: url.Values{"folder_id": {"attacker-archive"}}.Encode(), contentType: "application/x-www-form-urlencoded", handle: fixture.handler.handleMoveMessage},
		{name: "prefetch body", method: http.MethodPost, path: "/api/messages/" + id + "/prefetch-body", pathValues: map[string]string{"id": id}, handle: fixture.handler.handlePrefetchBody},
		{name: "refetch body", method: http.MethodPost, path: "/api/messages/" + id + "/refetch", pathValues: map[string]string{"id": id}, handle: fixture.handler.handleRefetchBody},
		{name: "translate", method: http.MethodPost, path: "/api/messages/" + id + "/translate", pathValues: map[string]string{"id": id}, body: `{}`, contentType: "application/json", handle: fixture.handler.handleTranslateMessage},
		{name: "allow remote content", method: http.MethodPost, path: "/api/remote-content/" + id + "/allow", pathValues: map[string]string{"id": id}, body: `{"mode":"sender"}`, contentType: "application/json", handle: fixture.handler.handleAllowRemoteContent},
		{name: "label message", method: http.MethodPost, path: "/api/messages/" + id + "/label", pathValues: map[string]string{"id": id}, body: url.Values{"label": {"Projects"}}.Encode(), contentType: "application/x-www-form-urlencoded", handle: fixture.handler.handleLabelMessage},
		{name: "unlabel message", method: http.MethodPost, path: "/api/messages/" + id + "/unlabel", pathValues: map[string]string{"id": id}, body: url.Values{"label": {"Projects"}}.Encode(), contentType: "application/x-www-form-urlencoded", handle: fixture.handler.handleUnlabelMessage},
		{name: "bulk read", method: http.MethodPost, path: "/api/messages/read", body: bulk(""), contentType: "application/json", handle: fixture.handler.handleMarkMessagesRead},
		{name: "bulk star", method: http.MethodPost, path: "/api/messages/star", body: bulk(`,"state":"starred"`), contentType: "application/json", handle: fixture.handler.handleMarkMessagesStarred},
		{name: "bulk archive", method: http.MethodPost, path: "/api/messages/archive", body: bulk(""), contentType: "application/json", handle: fixture.handler.handleArchiveMessages},
		{name: "bulk delete", method: http.MethodPost, path: "/api/messages/delete", body: bulk(""), contentType: "application/json", handle: fixture.handler.handleDeleteMessages},
		{name: "bulk spam", method: http.MethodPost, path: "/api/messages/spam", body: bulk(""), contentType: "application/json", handle: fixture.handler.handleMarkMessagesSpam},
		{name: "bulk not spam", method: http.MethodPost, path: "/api/messages/not-spam", body: bulk(""), contentType: "application/json", handle: fixture.handler.handleMarkMessagesNotSpam},
		{name: "bulk label", method: http.MethodPost, path: "/api/messages/label", body: bulk(`,"label":"Projects"`), contentType: "application/json", handle: fixture.handler.handleLabelMessages},
		{name: "bulk unlabel", method: http.MethodPost, path: "/api/messages/unlabel", body: bulk(`,"label":"Projects"`), contentType: "application/json", handle: fixture.handler.handleUnlabelMessages},
		{name: "bulk move", method: http.MethodPost, path: "/api/messages/move", body: bulk(`,"folder_id":"attacker-archive"`), contentType: "application/json", handle: fixture.handler.handleMoveMessages},
	}
}

func executeMessageActionRequest(t *testing.T, request messageActionRequest, asUser func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
	if request.contentType != "" {
		req.Header.Set("Content-Type", request.contentType)
	}
	for key, value := range request.pathValues {
		req.SetPathValue(key, value)
	}
	rec := httptest.NewRecorder()
	request.handle(rec, asUser(req))
	return rec
}

func TestPrivateMessageActionsRejectForeignUserAndAdminWithoutSideEffects(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		role := "user"
		if isAdmin {
			role = "admin"
		}
		t.Run(role, func(t *testing.T) {
			template := newMessageActionOwnershipFixture(t)
			for _, requestTemplate := range foreignMessageActionRequests(template) {
				t.Run(requestTemplate.name, func(t *testing.T) {
					fixture := newMessageActionOwnershipFixture(t)
					request := foreignMessageActionRequests(fixture)
					var action messageActionRequest
					for _, candidate := range request {
						if candidate.name == requestTemplate.name {
							action = candidate
							break
						}
					}
					rec := executeMessageActionRequest(t, action, func(req *http.Request) *http.Request {
						return attackerRequestWithAdmin(req, isAdmin)
					})
					if rec.Code != http.StatusNotFound {
						t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
					}
					fixture.assertVictimUnchanged(t)
				})
			}
		})
	}
}

func TestBulkMessageActionsRejectMixedOwnershipBeforeAnyMutation(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   func(messageActionOwnershipFixture) string
		handle func(*Handler) http.HandlerFunc
	}{
		{name: "read", path: "/api/messages/read", body: mixedMessageActionBody, handle: func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesRead }},
		{name: "star", path: "/api/messages/star", body: func(f messageActionOwnershipFixture) string {
			return mixedMessageActionBody(f)[:len(mixedMessageActionBody(f))-1] + `,"state":"starred"}`
		}, handle: func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesStarred }},
		{name: "archive", path: "/api/messages/archive", body: mixedMessageActionBody, handle: func(h *Handler) http.HandlerFunc { return h.handleArchiveMessages }},
		{name: "delete", path: "/api/messages/delete", body: mixedMessageActionBody, handle: func(h *Handler) http.HandlerFunc { return h.handleDeleteMessages }},
		{name: "spam", path: "/api/messages/spam", body: mixedMessageActionBody, handle: func(h *Handler) http.HandlerFunc { return h.handleMarkMessagesSpam }},
		{name: "label", path: "/api/messages/label", body: func(f messageActionOwnershipFixture) string {
			return mixedMessageActionBody(f)[:len(mixedMessageActionBody(f))-1] + `,"label":"Projects"}`
		}, handle: func(h *Handler) http.HandlerFunc { return h.handleLabelMessages }},
		{name: "unlabel", path: "/api/messages/unlabel", body: func(f messageActionOwnershipFixture) string {
			return mixedMessageActionBody(f)[:len(mixedMessageActionBody(f))-1] + `,"label":"Projects"}`
		}, handle: func(h *Handler) http.HandlerFunc { return h.handleUnlabelMessages }},
		{name: "move", path: "/api/messages/move", body: func(f messageActionOwnershipFixture) string {
			return mixedMessageActionBody(f)[:len(mixedMessageActionBody(f))-1] + `,"folder_id":"victim-archive"}`
		}, handle: func(h *Handler) http.HandlerFunc { return h.handleMoveMessages }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newMessageActionOwnershipFixture(t)
			request := messageActionRequest{
				method:      http.MethodPost,
				path:        tt.path,
				body:        tt.body(fixture),
				contentType: "application/json",
				handle:      tt.handle(fixture.handler),
			}
			rec := executeMessageActionRequest(t, request, ownerRequest)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}
			fixture.assertVictimUnchanged(t)
		})
	}
}

func mixedMessageActionBody(fixture messageActionOwnershipFixture) string {
	return `{"targets":[{"id":"` + strconv.FormatInt(fixture.victimMessageID, 10) +
		`"},{"id":"` + strconv.FormatInt(fixture.foreignMessageID, 10) + `"}]}`
}

func TestForeignAndMissingMessageActionResponsesMatch(t *testing.T) {
	fixture := newMessageActionOwnershipFixture(t)
	request := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/messages/"+id+"/read", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		fixture.handler.handleToggleRead(rec, attackerRequest(req))
		return rec
	}

	foreign := request(strconv.FormatInt(fixture.victimMessageID, 10))
	missing := request("999999")
	if foreign.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("foreign=(%d,%q) missing=(%d,%q), want matching 404 responses",
			foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
	}
	fixture.assertVictimUnchanged(t)
}

func TestOwnedMessageActionQueuesMutation(t *testing.T) {
	fixture := newMessageActionOwnershipFixture(t)
	id := strconv.FormatInt(fixture.victimMessageID, 10)
	req := httptest.NewRequest(http.MethodPost, "/api/messages/"+id+"/read?state=read", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()

	fixture.handler.handleToggleRead(rec, ownerRequest(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200", rec.Code, rec.Body.String())
	}
	var isRead, mutations int
	if err := fixture.db.Read().QueryRowContext(t.Context(), `
		SELECT is_read
		FROM message_folder_state
		WHERE message_id = ? AND folder_id = 'victim-inbox'`,
		fixture.victimMessageID,
	).Scan(&isRead); err != nil {
		t.Fatalf("query read state: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM message_mutations
		WHERE message_id = ? AND kind = 'read'`,
		fixture.victimMessageID,
	).Scan(&mutations); err != nil {
		t.Fatalf("query read mutation: %v", err)
	}
	if isRead != 1 || mutations != 1 {
		t.Fatalf("owned action state is_read=%d mutations=%d, want 1/1", isRead, mutations)
	}
}

func TestOwnedRemoteContentApprovalFetchesAndPersists(t *testing.T) {
	fixture := newMessageActionOwnershipFixture(t)
	id := strconv.FormatInt(fixture.victimMessageID, 10)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/remote-content/"+id+"/allow",
		strings.NewReader(`{"mode":"sender"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()

	fixture.handler.handleAllowRemoteContent(rec, ownerRequest(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200", rec.Code, rec.Body.String())
	}
	if calls := fixture.remoteCalls.Load(); calls != 1 {
		t.Fatalf("remote-content requests = %d, want 1", calls)
	}
	var bodyPath string
	var allowed, senderMarkers int
	if err := fixture.db.Read().QueryRowContext(t.Context(),
		`SELECT body_html_path FROM messages WHERE id = ?`,
		fixture.victimMessageID,
	).Scan(&bodyPath); err != nil {
		t.Fatalf("query rewritten body path: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM remote_content_messages WHERE message_id = ?`,
		fixture.victimMessageID,
	).Scan(&allowed); err != nil {
		t.Fatalf("query remote-content allow: %v", err)
	}
	if err := fixture.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM app_settings
		WHERE user_id = 'owner' AND key LIKE 'remote_content_sender_allow_%' AND value = '1'`,
	).Scan(&senderMarkers); err != nil {
		t.Fatalf("query sender remote-content marker: %v", err)
	}
	if bodyPath == fixture.bodyPath || allowed != 1 || senderMarkers != 1 {
		t.Fatalf("remote content body=%q original=%q allowed=%d sender_markers=%d",
			bodyPath, fixture.bodyPath, allowed, senderMarkers)
	}
}
