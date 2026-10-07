package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactFindingsHTTPStoredMatchesAndAccountFilter(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	id := stagedContactSetup(t, f, "alice")
	seedHTTPContactPreview(t, f, "alice")
	seedHTTPContactPreview(t, f, "bob")
	path := "/api/contacts/" + id + "/sync-setup/findings"
	for _, suffix := range []string{"", "?mode=custom&query=alice-imported&account_id=" + f.accounts["alice"].ID} {
		response := f.request("alice", "GET", path+suffix, "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), `value="stored:same-preview-donor"`) || !strings.Contains(response.Body.String(), "alice-imported@example.com") || strings.Contains(response.Body.String(), "bob-imported@example.com") {
			t.Fatal("stored findings", response.Code, response.Body.String())
		}
	}
	if response := f.request("alice", "GET", path+"?mode=email", ""); response.Code != 200 || strings.Contains(response.Body.String(), `value="stored:same-preview-donor"`) {
		t.Fatal("email mode", response.Code, response.Body.String())
	}
	if response := f.request("alice", "GET", path+"?account_id="+f.accounts["bob"].ID, ""); response.Code != 404 {
		t.Fatal("foreign filtered account", response.Code)
	}
	if response := f.request("bob", "GET", path, ""); response.Code != 404 {
		t.Fatal("foreign findings", response.Code)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("stored discovery performed provider HTTP")
	}
}

type ownedContactSetupAPI struct {
	provider                            string
	mu                                  sync.Mutex
	calls                               int
	rejectOld, block, wrongID, throttle bool
	next                                string
	entered, release                    chan struct{}
	once, releaseOnce                   sync.Once
}

func (a *ownedContactSetupAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }
func (a *ownedContactSetupAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.calls++
	block, reject, wrong, throttle, next := a.block, a.rejectOld, a.wrongID, a.throttle, a.next
	a.mu.Unlock()
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if owner != "alice" && owner != "bob" {
		http.Error(w, "wrong owner credential", 401)
		return
	}
	if block && owner == "alice" {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-r.Context().Done():
			return
		}
	}
	if reject && strings.HasSuffix(r.Header.Get("Authorization"), "-access") {
		http.Error(w, "expired", 401)
		return
	}
	if throttle {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "busy", 429)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if a.provider == "gmail" {
		person := map[string]any{"resourceName": "people/chosen", "etag": owner + "-tag", "names": []any{map[string]string{"displayName": owner + " chosen"}}, "emailAddresses": []any{map[string]string{"value": owner + "-setup@example.com"}}, "phoneNumbers": []any{map[string]string{"value": "+1 202 555 0101"}}}
		if r.URL.Path == "/people:searchContacts" {
			if r.URL.Query().Get("pageSize") != "10" || r.URL.Query().Get("readMask") == "" || r.URL.Query().Get("query") == "" {
				http.Error(w, "wrong search query", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"person": map[string]any{"resourceName": "people/fuzzy", "names": []any{map[string]string{"displayName": "fuzzy result"}}, "emailAddresses": []any{map[string]string{"value": "fuzzy@example.com"}}}}, map[string]any{"person": person}, map[string]any{"person": person}}})
		} else if r.URL.Path == "/people/chosen" {
			if wrong {
				person["resourceName"] = "people/another"
			}
			_ = json.NewEncoder(w).Encode(person)
		} else {
			http.Error(w, "wrong Google path", 400)
		}
		return
	}
	if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "missing immutable IDs", 400)
		return
	}
	remote := map[string]any{"id": "same-chosen", "@odata.etag": owner + "-tag", "displayName": owner + " chosen", "emailAddresses": []any{map[string]string{"address": owner + "-setup@example.com"}}, "mobilePhone": "+1 202 555 0101"}
	if r.URL.Path == "/me/contacts" {
		if r.URL.Query().Get("$top") != "100" {
			http.Error(w, "wrong Graph page size", 400)
			return
		}
		result := map[string]any{"value": []any{map[string]any{"id": "unmatched", "displayName": "other person"}}}
		if r.URL.Query().Get("$skiptoken") == "two" {
			result["value"] = []any{remote, remote}
		} else {
			result["@odata.nextLink"] = "https://graph.microsoft.com/v1.0/me/contacts?$top=100&$skiptoken=two"
		}
		if next != "" {
			result["@odata.nextLink"] = next
		}
		_ = json.NewEncoder(w).Encode(result)
	} else if r.URL.Path == "/me/contacts/same-chosen" {
		if wrong {
			remote["id"] = "another"
		}
		_ = json.NewEncoder(w).Encode(remote)
	} else {
		http.Error(w, "wrong Graph path", 400)
	}
}

func newOwnedContactSetupFixture(t *testing.T, provider string) (*userStorageFixture, *ownedContactSetupAPI, *ownedContactPullAPI) {
	t.Helper()
	f, tokens, _ := newOwnedContactPullFixture(t, provider)
	api := &ownedContactSetupAPI{provider: provider, entered: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(api.unblock)
	previous := http.DefaultTransport
	target, _ := url.Parse(server.URL)
	http.DefaultTransport = ownedContactPullTransport{base: previous, target: target}
	t.Cleanup(func() { http.DefaultTransport = previous })
	return f, api, tokens
}

func stageProviderContactSetup(t *testing.T, f *userStorageFixture, owner string) string {
	t.Helper()
	form := editorForm(owner)
	form.Set("email", owner+"-setup@example.com")
	form.Set("phone", "+12025550101")
	form.Set("save_targets", "local,account:"+f.accounts[owner].ID)
	response := requestContactEditor(t.Context(), f, owner, "/api/contacts", form)
	var body struct {
		ID    string `json:"contact_id"`
		Setup string `json:"contact_sync_setup_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 || body.ID == "" || body.Setup == "" {
		t.Fatal("stage provider setup", response.Code, response.Body.String(), err)
	}
	return body.ID
}

func TestUserContactFindingsAndPreviewHTTPProviderIdentityRefreshAndRollback(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api, tokens := newOwnedContactSetupFixture(t, provider)
			id := stageProviderContactSetup(t, f, "alice")
			api.rejectOld = true
			path := "/api/contacts/" + id + "/sync-setup"
			response := f.request("alice", "GET", path+"/findings?mode=automatic", "")
			key := "gmail:people/chosen"
			if provider == "outlook" {
				key = "outlook:same-chosen"
			}
			if response.Code != 200 || strings.Count(response.Body.String(), `value="`+key+`"`) != 1 || strings.Contains(response.Body.String(), "bob-setup@example.com") {
				t.Fatal("provider findings/dedup", response.Code, response.Body.String())
			}
			if provider == "gmail" && strings.Index(response.Body.String(), `value="`+key+`"`) > strings.Index(response.Body.String(), `value="gmail:people/fuzzy"`) {
				t.Fatal("match ranking lost")
			}
			tokens.mu.Lock()
			refreshes := tokens.calls["alice|token"]
			tokens.mu.Unlock()
			if refreshes == 0 {
				t.Fatal("401 did not refresh scoped token")
			}
			api.mu.Lock()
			calls := api.calls
			api.mu.Unlock()
			if foreign := f.request("alice", "GET", path+"/findings?account_id="+f.accounts["bob"].ID, ""); foreign.Code != 404 {
				t.Fatal("foreign account searched", foreign.Code)
			}
			api.mu.Lock()
			afterForeign := api.calls
			api.mu.Unlock()
			if calls != afterForeign {
				t.Fatal("foreign filter reached provider")
			}
			form := url.Values{"candidate_" + f.accounts["alice"].ID: {key}}
			api.mu.Lock()
			api.wrongID = true
			api.mu.Unlock()
			if response := requestContactEditor(t.Context(), f, "alice", path+"/preview", form); response.Code != 502 {
				t.Fatal("wrong remote identity accepted", response.Code, response.Body.String())
			}
			api.mu.Lock()
			api.wrongID = false
			api.mu.Unlock()
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_remote_preview BEFORE INSERT ON contact_cards WHEN NEW.kind='provider' BEGIN SELECT RAISE(ABORT,'synthetic provider preview failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if response := requestContactEditor(t.Context(), f, "alice", path+"/preview", form); response.Code != 503 {
				t.Fatal("failed remote publication", response.Code, response.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var cards, fields int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&cards); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE source=?`, "synced:"+f.accounts["alice"].ID).Scan(&fields); err != nil {
					return err
				}
				if cards != 0 || fields != 0 {
					t.Fatal("failed preview partially attached", cards, fields)
				}
				_, err := db.Write().Exec(`DROP TRIGGER reject_remote_preview`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if response := requestContactEditor(t.Context(), f, "alice", path+"/preview", form); response.Code != 200 || !strings.Contains(response.Body.String(), "preferred_name") {
				t.Fatal("remote preview retry", response.Code, response.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var remote, tag string
				if err := db.Read().QueryRow(`SELECT remote_id,etag FROM contact_cards WHERE kind='provider'`).Scan(&remote, &tag); err != nil {
					return err
				}
				if remote != strings.TrimPrefix(key, provider+":") || tag != "alice-tag" {
					t.Fatal("provider card binding", remote, tag)
				}
				contact, err := db.GetContact(t.Context(), "alice", id)
				if err != nil {
					return err
				}
				if contact.GoferSyncEnabled {
					t.Fatal("preview enabled sync")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactFindingsAndPreviewHTTPBlockedProviderReleasesStoreAndRejectsLateAccount(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, preview := range []bool{false, true} {
			name := provider + "/findings"
			if preview {
				name = provider + "/preview"
			}
			t.Run(name, func(t *testing.T) {
				f, api, _ := newOwnedContactSetupFixture(t, provider)
				id := stageProviderContactSetup(t, f, "alice")
				api.block = true
				path := "/api/contacts/" + id + "/sync-setup"
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					if preview {
						key := "gmail:people/chosen"
						if provider == "outlook" {
							key = "outlook:same-chosen"
						}
						done <- requestContactEditor(t.Context(), f, "alice", path+"/preview", url.Values{"candidate_" + f.accounts["alice"].ID: {key}})
					} else {
						done <- f.request("alice", "GET", path+"/findings?mode=email", "")
					}
				}()
				select {
				case <-api.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider not reached")
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if _, err := f.accountStore.SnapshotServices(ctx, "bob", f.accounts["bob"].ID); err != nil {
					t.Fatal("provider retained store lease", err)
				}
				if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement' WHERE id=?`, f.accounts["alice"].ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				api.unblock()
				select {
				case response := <-done:
					if response.Code != 409 || strings.Contains(response.Body.String(), "alice chosen") {
						t.Fatal("stale findings/publication", response.Code, response.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("request did not drain")
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var count int
					err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&count)
					if count != 0 {
						t.Fatal("late preview published")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserContactFindingsHTTPGraphRejectsForeignAndRepeatedPages(t *testing.T) {
	for _, next := range []string{"https://outside.test/me/contacts", "https://graph.microsoft.com/v1.0/users/other/contacts", "https://graph.microsoft.com/v1.0/me/contacts?$top=100"} {
		t.Run(next, func(t *testing.T) {
			f, api, _ := newOwnedContactSetupFixture(t, "outlook")
			id := stageProviderContactSetup(t, f, "alice")
			api.next = next
			response := f.request("alice", "GET", "/api/contacts/"+id+"/sync-setup/findings?mode=email", "")
			if response.Code != 200 || !(strings.Contains(response.Body.String(), "outside the configured Graph collection") || strings.Contains(response.Body.String(), "repeated a page URL")) || strings.Contains(response.Body.String(), `value="outlook:`) {
				t.Fatal("bad page accepted", response.Code, response.Body.String())
			}
			api.mu.Lock()
			calls := api.calls
			api.mu.Unlock()
			want := 1
			if strings.Contains(next, "?$top") {
				want = 2
			}
			if calls != want {
				t.Fatal("unsafe/repeated page dispatched", calls, want)
			}
		})
	}
}

func TestUserContactFindingsHTTPProviderThrottlePersistsAndStopsRetry(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactSetupFixture(t, provider)
			id := stageProviderContactSetup(t, f, "alice")
			api.throttle = true
			path := "/api/contacts/" + id + "/sync-setup/findings?mode=email"
			if response := f.request("alice", "GET", path, ""); response.Code != 200 || !strings.Contains(response.Body.String(), "429") {
				t.Fatal("missing per-location provider failure", response.Code, response.Body.String())
			}
			until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || !until.After(time.Now()) {
				t.Fatal("throttle not durably recorded", until, err)
			}
			api.mu.Lock()
			calls := api.calls
			api.mu.Unlock()
			if response := f.request("alice", "GET", path, ""); response.Code != 200 || !strings.Contains(response.Body.String(), "retry is deferred") {
				t.Fatal("throttle retry", response.Code, response.Body.String())
			}
			api.mu.Lock()
			after := api.calls
			api.mu.Unlock()
			if after != calls {
				t.Fatal("deferred discovery contacted provider")
			}
		})
	}
}

func TestUserContactFindingsAndPreviewHTTPRootShutdownCancelsProvider(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, preview := range []bool{false, true} {
			name := provider + "/findings"
			if preview {
				name = provider + "/preview"
			}
			t.Run(name, func(t *testing.T) {
				f, api, _ := newOwnedContactSetupFixture(t, provider)
				id := stageProviderContactSetup(t, f, "alice")
				api.block = true
				path := "/api/contacts/" + id + "/sync-setup"
				done := make(chan int, 1)
				go func() {
					if preview {
						key := "gmail:people/chosen"
						if provider == "outlook" {
							key = "outlook:same-chosen"
						}
						done <- requestContactEditor(t.Context(), f, "alice", path+"/preview", url.Values{"candidate_" + f.accounts["alice"].ID: {key}}).Code
					} else {
						done <- f.request("alice", "GET", path+"/findings?mode=email", "").Code
					}
				}()
				select {
				case <-api.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider not reached")
				}
				f.stopIMAP()
				drained := make(chan struct{})
				go func() { f.imap.Wait(); close(drained) }()
				select {
				case <-drained:
				case <-time.After(5 * time.Second):
					t.Fatal("root did not drain HTTP request")
				}
				select {
				case code := <-done:
					if code == 200 {
						t.Fatal("stopped root accepted provider response")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("request did not finish")
				}
				// Neither caller completion nor root drain needs the provider release.
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var count int
					err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&count)
					if count != 0 {
						t.Fatal("late root publication")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
