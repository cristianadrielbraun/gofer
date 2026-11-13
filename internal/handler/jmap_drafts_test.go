package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Discarding a JMAP account's draft in shared mode removes it locally without
// queueing an IMAP deletion, which a JMAP account has no server for.
func TestDiscardJMAPDraftQueuesNoIMAPDelete(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mail.NewSyncOrchestrator(db, nil, nil, nil)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET provider='jmap', imap_host='', smtp_host='' WHERE id='victim-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "victim-drafts", AccountID: "victim-account", RemoteID: "mb-drafts", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveDraftMessage(ctx, storage.DraftMessageInput{AccountID: "victim-account", FolderID: "victim-drafts", InternetMessageID: "<notes-1@example.test>", Subject: "Notes for Friday", FromEmail: "owner@example.com", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"account_id": {"victim-account"}, "draft_id": {"<notes-1@example.test>"}}
	req := httptest.NewRequest(http.MethodPost, "/compose/draft/discard", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleDiscardComposeDraft(rec, ownerRequest(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("discard = %d %s", rec.Code, rec.Body.String())
	}
	var operations, drafts int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_draft_operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE internet_message_id='<notes-1@example.test>'`).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if operations != 0 || drafts != 0 {
		t.Fatalf("after discard: %d IMAP operations queued, %d drafts left", operations, drafts)
	}
}

func TestDeleteReadPaneJMAPDraftRemovesRemoteCopy(t *testing.T) {
	for _, mode := range []string{"draft", "changed", "sent", "gone", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			h.syncer = mail.NewSyncOrchestrator(db, nil, nil, nil)
			var exists atomic.Bool
			exists.Store(mode != "gone")
			var deletions, hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if user, password, ok := r.BasicAuth(); !ok || user != "owner@example.com" || password != "secret" {
					http.Error(w, "wrong login", http.StatusUnauthorized)
					return
				}
				if r.URL.Path == "/.well-known/jmap" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"capabilities":    map[string]any{jmap.CapabilityCore: map[string]any{}, jmap.CapabilityMail: map[string]any{}},
						"primaryAccounts": map[string]string{jmap.CapabilityMail: "a1"}, "apiUrl": "/jmap/",
					})
					return
				}
				var request struct {
					MethodCalls [][3]json.RawMessage `json:"methodCalls"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.MethodCalls) != 1 {
					t.Error("invalid JMAP request", err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				var name, callID string
				_ = json.Unmarshal(request.MethodCalls[0][0], &name)
				_ = json.Unmarshal(request.MethodCalls[0][2], &callID)
				result := map[string]any{"accountId": "a1"}
				switch name {
				case "Email/get":
					list := []any{}
					if exists.Load() {
						internetID := "notes-1@example.test"
						if mode == "changed" {
							internetID = "replacement@example.test"
						}
						list = append(list, map[string]any{"id": "e-draft", "messageId": []string{internetID}, "keywords": map[string]bool{jmap.KeywordDraft: mode != "sent"}})
					}
					result["list"] = list
				case "Email/set":
					var args struct {
						Destroy []string `json:"destroy"`
					}
					_ = json.Unmarshal(request.MethodCalls[0][1], &args)
					if len(args.Destroy) != 1 || args.Destroy[0] != "e-draft" {
						t.Errorf("destroyed unexpected emails: %v", args.Destroy)
					}
					deletions.Add(1)
					exists.Store(false)
					result["destroyed"] = args.Destroy
				default:
					t.Errorf("unexpected JMAP method: %s", name)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{name, result, callID}}})
			}))
			t.Cleanup(server.Close)
			endpoint, err := jmap.ParseEndpoint(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AddPlaintextTransportException(t.Context(), "http", endpoint.Host, endpoint.Port, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := db.AddPrivateTargetException(t.Context(), "http", endpoint.Host, endpoint.Port, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := h.accountStore.UpdateAccount(t.Context(), "victim-account", &models.CreateAccountRequest{
				Provider: "jmap", JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "owner@example.com", Password: "secret",
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: "victim-drafts", AccountID: "victim-account", RemoteID: "mb-drafts", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
				t.Fatal(err)
			}
			id, err := db.SaveDraftMessage(t.Context(), storage.DraftMessageInput{AccountID: "victim-account", FolderID: "victim-drafts", InternetMessageID: "<notes-1@example.test>", Subject: "Notes", FromEmail: "owner@example.com", Date: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetMessageProviderMessageID(t.Context(), id, "e-draft"); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodDelete, "/api/drafts/"+strconv.FormatInt(id, 10), nil)
			r.SetPathValue("id", strconv.FormatInt(id, 10))
			r = ownerRequest(r)
			if mode == "foreign" {
				r = attackerRequest(r)
			}
			response := httptest.NewRecorder()
			h.handleDeleteDraft(response, r)
			wantStatus, wantLocal, wantDeletes := http.StatusOK, 0, int32(0)
			if mode == "draft" {
				wantDeletes = 1
			}
			if mode == "foreign" {
				wantStatus, wantLocal = http.StatusNotFound, 1
			}
			if response.Code != wantStatus || deletions.Load() != wantDeletes {
				t.Fatalf("delete = %d %s, remote deletions %d", response.Code, response.Body.String(), deletions.Load())
			}
			if exists.Load() != (mode != "draft" && mode != "gone") {
				t.Fatal("wrong remote draft state")
			}
			if mode == "foreign" && hits.Load() != 0 {
				t.Fatal("foreign deletion reached the JMAP server")
			}
			var drafts, operations int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, id).Scan(&drafts); err != nil {
				t.Fatal(err)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM imap_draft_operations`).Scan(&operations); err != nil {
				t.Fatal(err)
			}
			if drafts != wantLocal || operations != 0 {
				t.Fatalf("local drafts %d, IMAP operations %d", drafts, operations)
			}
		})
	}
}
