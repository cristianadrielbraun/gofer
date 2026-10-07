package notifications

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type providerActionAPI struct {
	mu                             sync.Mutex
	provider                       string
	calls                          map[string]int
	read, star                     map[string]bool
	parent                         map[string]string
	blockOwner                     string
	blockPath                      string
	attachmentAPI                  bool
	badAttachment                  bool
	duplicateAttachments           bool
	lookupNextPage                 bool
	entered, release               chan struct{}
	once, releaseOnce              sync.Once
	rejectOld, throttle, missing   bool
	lookupCount                    int
	labels                         map[string]map[string]string
	appliedLabels                  map[string]map[string]bool
	badLabelCreate                 bool
	accepted                       map[string][][]byte
	acceptedAt                     map[string][]time.Time
	loseSendAck                    bool
	badSendReply                   bool
	sendStatus                     int
	drafts                         map[string]map[string]providerDraftFixtureEntry
	draftCreates, draftVersions    map[string]int
	loseDraftAck, badDraftReply    bool
	hideDrafts                     bool
	draftStatus, deleteDraftStatus int
}

func (a *providerActionAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }

func (a *providerActionAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		owner := strings.Split(r.FormValue("refresh_token"), "-")[0]
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": owner + "-fresh", "token_type": "Bearer", "expires_in": 3600, "scope": userOutlookHTTPScopes})
		return
	}
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	// Drain request bodies before waiting so net/http can observe a canceled
	// connection, including POST requests with a JSON payload.
	var payload map[string]json.RawMessage
	wire, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(wire, &payload)
	a.mu.Lock()
	a.calls[owner+"|"+r.Method+"|"+r.URL.Path]++
	block := owner == a.blockOwner && (r.Method != "GET" || r.URL.Path == a.blockPath)
	reject, throttle, missing := a.rejectOld, a.throttle, a.missing
	a.mu.Unlock()
	if block {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-r.Context().Done():
			return
		}
		a.mu.Lock()
		reject, throttle, missing = a.rejectOld, a.throttle, a.missing
		a.mu.Unlock()
	}
	if reject && strings.HasSuffix(r.Header.Get("Authorization"), "-access") {
		http.Error(w, "expired", 401)
		return
	}
	if throttle {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "throttled", 429)
		return
	}
	if missing {
		http.Error(w, "gone", 404)
		return
	}
	if a.provider == "outlook" && !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "no immutable preference", 400)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attachmentAPI && a.serveAttachmentLocked(w, r, owner) {
		return
	}
	if a.serveDraftLocked(w, r, owner, wire, payload) {
		return
	}
	switch {
	case r.Method == "POST" && (r.URL.Path == "/users/me/messages/send" || r.URL.Path == "/me/sendMail"):
		if a.sendStatus != 0 {
			http.Error(w, "synthetic send rejection", a.sendStatus)
			return
		}
		var raw []byte
		var err error
		if a.provider == "gmail" {
			var encoded string
			_ = json.Unmarshal(payload["raw"], &encoded)
			raw, err = base64.RawURLEncoding.DecodeString(encoded)
		} else {
			if r.Header.Get("Content-Type") != "text/plain" {
				http.Error(w, "invalid MIME type", 400)
				return
			}
			raw, err = base64.StdEncoding.DecodeString(string(wire))
		}
		if err != nil || len(raw) == 0 {
			http.Error(w, "invalid MIME", 400)
			return
		}
		if a.accepted == nil {
			a.accepted = make(map[string][][]byte)
		}
		a.accepted[owner] = append(a.accepted[owner], raw)
		if a.acceptedAt == nil {
			a.acceptedAt = make(map[string][]time.Time)
		}
		a.acceptedAt[owner] = append(a.acceptedAt[owner], time.Now())
		if a.loseSendAck {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if a.badSendReply {
			_, _ = fmt.Fprint(w, `{`)
			return
		}
		if a.provider == "outlook" {
			w.WriteHeader(202)
			return
		}
		id := "same-sent-provider-id"
		if len(a.accepted[owner]) > 1 {
			id = fmt.Sprintf("sent-provider-%d", len(a.accepted[owner]))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == "GET" && (r.URL.Path == "/users/me/labels" || r.URL.Path == "/me/outlook/masterCategories"):
		items := []map[string]string{}
		for name, id := range a.labels[owner] {
			items = append(items, map[string]string{"id": id, "name": name, "type": "user", "displayName": name})
		}
		key := "labels"
		if a.provider == "outlook" {
			key = "value"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{key: items})
	case r.Method == "POST" && (r.URL.Path == "/users/me/labels" || r.URL.Path == "/me/outlook/masterCategories"):
		key := "name"
		if a.provider == "outlook" {
			key = "displayName"
		}
		var name string
		_ = json.Unmarshal(payload[key], &name)
		if a.labels[owner] == nil {
			a.labels[owner] = map[string]string{}
		}
		a.labels[owner][name] = "same-label-id"
		if a.badLabelCreate {
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "same-label-id", "name": name, "displayName": name})
	case r.Method == "DELETE":
		w.WriteHeader(204)
	case r.Method == "GET" && (r.URL.Path == "/me/messages" || r.URL.Path == "/users/me/messages"):
		items := []map[string]string{}
		for i := 0; i < a.lookupCount; i++ {
			items = append(items, map[string]string{"id": "m1", "internetMessageId": "<" + owner + "@provider.test>"})
		}
		key := "value"
		if a.provider == "gmail" {
			key = "messages"
			if r.URL.Query().Get("q") != "rfc822msgid:<"+owner+"@provider.test>" || r.URL.Query().Get("includeSpamTrash") != "true" {
				http.Error(w, "incorrect identity search", 400)
				return
			}
		} else if r.URL.Query().Get("$filter") != "internetMessageId eq '<"+owner+"@provider.test>'" {
			http.Error(w, "incorrect identity filter", 400)
			return
		}
		response := map[string]any{key: items}
		if a.lookupNextPage {
			if a.provider == "gmail" {
				response["nextPageToken"] = "more"
			} else {
				response["@odata.nextLink"] = "https://graph.microsoft.com/v1.0/me/messages?more=1"
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	case r.Method == "GET":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m1", "parentFolderId": a.parent[owner], "categories": a.categoriesLocked(owner)})
	case strings.HasSuffix(r.URL.Path, "/move"):
		var destination string
		_ = json.Unmarshal(payload["destinationId"], &destination)
		a.parent[owner] = destination
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "m1"})
	case strings.HasSuffix(r.URL.Path, "/trash"):
		a.parent[owner] = "TRASH"
		_, _ = fmt.Fprint(w, `{}`)
	case strings.HasSuffix(r.URL.Path, "/untrash"):
		a.parent[owner] = "INBOX"
		_, _ = fmt.Fprint(w, `{}`)
	case a.provider == "gmail":
		var add, remove []string
		_ = json.Unmarshal(payload["addLabelIds"], &add)
		_ = json.Unmarshal(payload["removeLabelIds"], &remove)
		for _, label := range add {
			for name, id := range a.labels[owner] {
				if label == id {
					a.appliedLabels[owner][name] = true
				}
			}
			if label == "UNREAD" {
				a.read[owner] = false
			}
			if label == "STARRED" {
				a.star[owner] = true
			}
		}
		for _, label := range remove {
			for name, id := range a.labels[owner] {
				if label == id {
					a.appliedLabels[owner][name] = false
				}
			}
			if label == "UNREAD" {
				a.read[owner] = true
			}
			if label == "STARRED" {
				a.star[owner] = false
			}
		}
		_, _ = fmt.Fprint(w, `{}`)
	default:
		if raw, ok := payload["categories"]; ok {
			var categories []string
			_ = json.Unmarshal(raw, &categories)
			a.appliedLabels[owner] = map[string]bool{}
			for _, name := range categories {
				a.appliedLabels[owner][name] = true
			}
		}
		// Decode through a temporary because map elements cannot be addressed.
		if raw, ok := payload["isRead"]; ok {
			var read bool
			_ = json.Unmarshal(raw, &read)
			a.read[owner] = read
		}
		if raw, ok := payload["flag"]; ok {
			var flag map[string]string
			_ = json.Unmarshal(raw, &flag)
			a.star[owner] = flag["flagStatus"] == "flagged"
		}
		_, _ = fmt.Fprint(w, `{}`)
	}
}

func (a *providerActionAPI) categoriesLocked(owner string) []string {
	result := []string{}
	for name, applied := range a.appliedLabels[owner] {
		if applied {
			result = append(result, name)
		}
	}
	return result
}

type providerActionTransport struct{ target *url.URL }

func (t providerActionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "gmail.googleapis.com" && r.URL.Host != "graph.microsoft.com" {
		return http.DefaultTransport.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	address.Path = strings.TrimPrefix(strings.TrimPrefix(address.Path, "/gmail/v1"), "/v1.0")
	copy.URL = &address
	return http.DefaultTransport.RoundTrip(copy)
}
func newProviderActionFixture(t *testing.T, provider string) (*userStorageFixture, *providerActionAPI) {
	t.Helper()
	api := &providerActionAPI{provider: provider, calls: map[string]int{}, read: map[string]bool{}, star: map[string]bool{}, parent: map[string]string{"alice": "INBOX", "bob": "INBOX"}, entered: make(chan struct{}), release: make(chan struct{}), lookupCount: 1}
	api.labels = map[string]map[string]string{"alice": {"Existing": "Label_Existing"}, "bob": {"Existing": "Label_Existing"}}
	api.appliedLabels = map[string]map[string]bool{"alice": {"Existing": true}, "bob": {"Existing": true}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(api.unblock)
	previous := http.DefaultClient
	address, _ := url.Parse(server.URL)
	http.DefaultClient = &http.Client{Transport: providerActionTransport{address}}
	t.Cleanup(func() { http.DefaultClient = previous })
	client := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	f := newUserStorageFixtureConfigured(t, true, &mailauth.Config{GoogleClient: client, MicrosoftClient: client})
	for _, owner := range []string{"alice", "bob"} {
		req := providers.GmailAccountRequest(owner+"@provider.test", owner, "same-subject")
		oauthProvider := "google"
		if provider == "outlook" {
			req = providers.OutlookAccountRequest(owner+"@provider.test", owner, "same-subject")
			oauthProvider = "microsoft"
		}
		account, err := f.accountStore.CreateAccount(t.Context(), owner, req)
		if err != nil {
			t.Fatal(err)
		}
		f.accounts[owner] = account
		expires := time.Now().Add(time.Hour)
		if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, oauthProvider, "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expires, userOutlookHTTPScopes); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID); err != nil {
				return err
			}
			for _, folder := range []struct{ remote, role string }{{"INBOX", "inbox"}, {"ARCHIVE", "archive"}, {"TRASH", "trash"}, {"SPAM", "junk"}} {
				if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,provider_remote_id,name,role) VALUES(?,?,?,?,?,?)`, account.ID+"-"+folder.role, account.ID, folder.remote, folder.remote, folder.remote, folder.role); err != nil {
					return err
				}
			}
			if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,remote_message_id,internet_message_id,thread_id,subject,from_email) VALUES(1,?,'m1',?,'thread','private','sender@provider.test')`, account.ID, "<"+owner+"@provider.test>"); err != nil {
				return err
			}
			_, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid,is_read,is_starred,is_deleted) VALUES(1,?,1,0,0,0)`, account.ID+"-inbox")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func TestUserProviderActionsHTTPFlagsAndMoves(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			for _, action := range []struct{ owner, path string }{{"alice", "/api/messages/1/read?state=read"}, {"alice", "/api/messages/1/star"}, {"bob", "/api/messages/1/read?state=read"}} {
				if rec := f.request(action.owner, "POST", action.path, ""); rec.Code != 200 {
					t.Fatalf("flag: %d %s", rec.Code, rec.Body.String())
				}
				waitUserIMAP(t, func() bool { return f.pendingMutations(t, action.owner) == 0 })
			}
			api.mu.Lock()
			valid := api.read["alice"] && api.star["alice"] && api.read["bob"] && !api.star["bob"]
			api.mu.Unlock()
			if !valid {
				t.Fatal("flags crossed owners or did not reach provider")
			}
			id := f.accounts["alice"].ID
			for _, action := range []struct{ method, path, body string }{{"POST", "/api/messages/1/thread/archive", ""}, {"POST", "/api/messages/1/move", url.Values{"folder_id": {id + "-inbox"}}.Encode()}, {"POST", "/api/messages/1/move", url.Values{"folder_id": {id + "-junk"}}.Encode()}, {"DELETE", "/api/messages/1?folder_id=" + url.QueryEscape(id+"-junk"), ""}, {"DELETE", "/api/messages/1?folder_id=" + url.QueryEscape(id+"-trash"), ""}} {
				if rec := f.request("alice", action.method, action.path, action.body); rec.Code != 200 {
					t.Fatalf("action %s: %d %s", action.path, rec.Code, rec.Body.String())
				}
				waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
			}
			if rec := f.request("alice", "POST", "/api/messages/1/move", url.Values{"folder_id": {f.accounts["bob"].ID + "-inbox"}}.Encode()); rec.Code != 400 {
				t.Fatalf("foreign move: %d", rec.Code)
			}
		})
	}
}

func TestUserProviderActionsBlockedReplyPreservesNewFlag(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			for _, owner := range []string{"alice", "bob"} {
				state := "read"
				if owner == "alice" {
					state = "unread"
				}
				if rec := f.request(owner, "POST", "/api/messages/1/read?state="+state, ""); rec.Code != 200 {
					t.Fatal(rec.Code)
				}
			}
			waitUserIMAP(t, func() bool { return f.pendingMutations(t, "bob") == 0 })
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
			api.mu.Lock()
			valid := !api.read["alice"] && api.read["bob"]
			api.mu.Unlock()
			if !valid {
				t.Fatal("old provider reply erased newer intent")
			}
		})
	}
}

func TestUserProviderActionsRefreshAndThrottle(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.rejectOld = true
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
			token, err := f.credentials.GetOAuthTokenForUser(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || token != "alice-fresh" {
				t.Fatalf("refresh: %q %v", token, err)
			}
			api.mu.Lock()
			api.rejectOld = false
			api.throttle = true
			api.mu.Unlock()
			if rec := f.request("alice", "POST", "/api/messages/1/star", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool {
				var failed int
				_ = f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM message_mutations WHERE status='failed' AND kind='starred'`).Scan(&failed)
				})
				return failed == 1
			})
			var next time.Time
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT next_attempt_at FROM message_mutations WHERE kind='starred'`).Scan(&next)
			}); err != nil {
				t.Fatal(err)
			}
			if next.Before(time.Now().Add(time.Minute)) {
				t.Fatal("provider Retry-After lost")
			}
		})
	}
}

func TestUserProviderActionsChangedIdentityRejectsPublication(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE messages SET remote_message_id='replacement' WHERE id=1`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool {
				var failed int
				_ = f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM message_mutations WHERE status='failed'`).Scan(&failed)
				})
				return failed == 1
			})
			var id string
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE id=1`).Scan(&id)
			}); err != nil || id != "replacement" {
				t.Fatalf("stale publication changed identity: %q %v", id, err)
			}
		})
	}
}

func TestUserProviderActionsMoveRecoveryAfterPublicationFailure(t *testing.T) {
	f, api := newProviderActionFixture(t, "outlook")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_action_move BEFORE UPDATE OF remote_message_id ON messages BEGIN SELECT RAISE(ABORT,'injected move publication failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", "POST", "/api/messages/1/thread/archive", ""); rec.Code != 200 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		var failed int
		_ = f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT count(*) FROM message_mutations WHERE status='failed'`).Scan(&failed)
		})
		return failed == 1
	})
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec(`DROP TRIGGER reject_action_move`); err != nil {
			return err
		}
		_, err := db.Write().Exec(`UPDATE message_mutations SET next_attempt_at=datetime('now','-1 minute') WHERE status='failed'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	moves := api.calls["alice|POST|/me/messages/m1/move"]
	api.mu.Unlock()
	if moves != 1 || f.pendingMutations(t, "alice") != 0 {
		t.Fatalf("publication retry repeated Graph move: %d", moves)
	}
}

func TestUserProviderActionsDeletionCancelsHTTP(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if err := f.accountStore.DeleteAccount(t.Context(), "alice", f.accounts["alice"].ID, f.imap.Cleanup); err != nil {
				t.Fatal(err)
			}
			var grants int
			if err := f.system.Read().QueryRow(`SELECT count(*) FROM gofer_mailbox_credentials WHERE account_id=?`, f.accounts["alice"].ID).Scan(&grants); err != nil || grants != 0 {
				t.Fatalf("deletion retained grant: %d %v", grants, err)
			}
			if rec := f.request("bob", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool { return f.pendingMutations(t, "bob") == 0 })
		})
	}
}

func TestUserProviderActionsSpamAndBulk(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, _ := newProviderActionFixture(t, provider)
			id := f.accounts["alice"].ID
			for _, action := range []struct{ path, folder, state string }{
				{"read", "inbox", "read"}, {"star", "inbox", "starred"},
				{"spam", "inbox", ""}, {"not-spam", "junk", ""},
			} {
				body := fmt.Sprintf(`{"targets":[{"id":"1"}],"folder_id":%q,"state":%q}`, id+"-"+action.folder, action.state)
				if rec := f.request("alice", "POST", "/api/messages/"+action.path, body); rec.Code != 200 {
					t.Fatalf("%s: %d %s", action.path, rec.Code, rec.Body.String())
				}
				waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				if err := db.Read().QueryRow(`SELECT count(*) FROM message_folder_state WHERE message_id=1 AND folder_id=? AND is_deleted=0`, id+"-inbox").Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					return fmt.Errorf("not-spam did not return message to inbox")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			foreign := fmt.Sprintf(`{"targets":[{"id":"1"}],"folder_id":%q}`, f.accounts["bob"].ID+"-inbox")
			if rec := f.request("alice", "POST", "/api/messages/spam", foreign); rec.Code != 404 {
				t.Fatalf("foreign spam: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUserProviderActionsMissingIdentity(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, count := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("%s/%d", provider, count), func(t *testing.T) {
				f, api := newProviderActionFixture(t, provider)
				api.lookupCount = count
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET remote_message_id=NULL WHERE id=1`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
					t.Fatal(rec.Code)
				}
				waitUserIMAP(t, func() bool {
					var finished int
					_ = f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						return db.Read().QueryRow(`SELECT count(*) FROM message_mutations WHERE status IN ('applied','failed')`).Scan(&finished)
					})
					return finished == 1
				})
				var remote string
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE id=1`).Scan(&remote)
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				read := api.read["alice"]
				api.mu.Unlock()
				if (count == 1) != read || (count == 1 && remote != "m1") || (count != 1 && remote != "") {
					t.Fatalf("lookup %d: remote=%q read=%t", count, remote, read)
				}
			})
		}
	}
}

func TestUserProviderActionsStaleFailurePreservesNewFlag(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner, api.throttle = "alice", true
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=read", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if rec := f.request("alice", "POST", "/api/messages/1/read?state=unread", ""); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			api.unblock()
			waitUserIMAP(t, func() bool {
				api.mu.Lock()
				calls := api.calls
				total := 0
				for key, n := range calls {
					if strings.HasPrefix(key, "alice|") {
						total += n
					}
				}
				api.mu.Unlock()
				return total > 0 && f.pendingMutations(t, "alice") == 1
			})
			// Sync waits for the active pass to release its account gate.
			_ = f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var value bool
				var status string
				if err := db.Read().QueryRow(`SELECT target_value,status FROM message_mutations WHERE kind='read'`).Scan(&value, &status); err != nil {
					return err
				}
				if value || status != "pending" {
					return fmt.Errorf("old failure lost unread intent: %t %s", value, status)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			calls := 0
			for key, n := range api.calls {
				if strings.HasPrefix(key, "alice|") {
					calls += n
				}
			}
			api.mu.Unlock()
			if calls != 1 {
				t.Fatalf("new intent bypassed cooldown: %d HTTP calls", calls)
			}
		})
	}
}

type providerActionSlowBody struct {
	reader           *strings.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (b *providerActionSlowBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.reader.Read(p)
}
func (b *providerActionSlowBody) Close() error { return nil }

func TestUserProviderActionsSlowPayloadDoesNotLeaseStore(t *testing.T) {
	f, _ := newProviderActionFixture(t, "gmail")
	body := &providerActionSlowBody{reader: strings.NewReader(`{"targets":[{"id":"1"}],"state":"read"}`), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(body.release) }) }
	t.Cleanup(unblock)
	req := httptest.NewRequest("POST", "/api/messages/read", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	req.Body = body
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(rec, req) }()
	awaitIMAP(t, body.entered)
	if bob := f.request("bob", "POST", "/api/messages/1/read?state=read", ""); bob.Code != 200 {
		t.Fatal(bob.Code)
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "bob") == 0 })
	unblock()
	awaitIMAP(t, done)
	if rec.Code != 200 {
		t.Fatalf("buffered action: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	if rec := f.request("alice", "POST", "/api/messages/read", strings.Repeat(" ", 65<<10)); rec.Code != 400 {
		t.Fatalf("oversized action: %d", rec.Code)
	}
}
