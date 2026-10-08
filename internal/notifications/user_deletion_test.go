package notifications

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

type userDeletionBrowser struct {
	f       *userStorageFixture
	session *auth.Session
	http    http.Handler
}

func newUserDeletionBrowser(t *testing.T, f *userStorageFixture) *userDeletionBrowser {
	t.Helper()
	if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('deletion-admin','deletion-admin','deletion-admin','management',1)`); err != nil {
		t.Fatal(err)
	}
	session, err := f.auth.CreateAuthenticatedSession(t.Context(), "deletion-admin", "deletion browser", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE sessions SET step_up_method='totp',step_up_at=? WHERE id=?`, time.Now().UTC(), session.ID); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.base.RegisterAdministrationRoutes(mux)
	return &userDeletionBrowser{f: f, session: session, http: f.auth.Middleware(mux)}
}

func (b *userDeletionBrowser) delete(t *testing.T, owner, confirmation string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	path := "/admin/users/" + owner + "/delete"
	values := url.Values{"confirmation": {confirmation}}
	if csrf {
		proofReq := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
		proofReq.AddCookie(&http.Cookie{Name: "gofer_session", Value: b.session.Token})
		var proof string
		b.f.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proof = auth.CSRFToken(r.Context(), http.MethodPost, path)
		})).ServeHTTP(httptest.NewRecorder(), proofReq)
		if proof == "" {
			t.Fatal("administrator CSRF proof unavailable")
		}
		values.Set(auth.CSRFFormFieldName, proof)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: b.session.Token})
	w := httptest.NewRecorder()
	b.http.ServeHTTP(w, req)
	return w
}

func deletionSQLInt(t *testing.T, f *userStorageFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.system.Read().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func awaitUserDeleted(t *testing.T, f *userStorageFixture, owner string) {
	t.Helper()
	waitUserIMAPFor(t, time.Minute, func() bool {
		return deletionSQLInt(t, f, `SELECT COUNT(*) FROM users WHERE id=?`, owner) == 0
	})
}

func ownedDBPath(t *testing.T, f *userStorageFixture, owner string) string {
	t.Helper()
	var path string
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	return path
}

type heldUploadReader struct {
	reader           io.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (r *heldUploadReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.reader.Read(p)
}

func TestOwnedUserDeletionHTTPDrainsNativeWorkAndUploads(t *testing.T) {
	f := newUserStorageFixtureConfigured(t, true, &mailauth.Config{})
	b := newUserDeletionBrowser(t, f)
	s := newRoutedIMAPServer(t)
	f.useIMAPServer(t, s)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
	}
	paths := map[string]string{"alice": ownedDBPath(t, f, "alice"), "bob": ownedDBPath(t, f, "bob")}
	grants, blobs, uploads := map[string]string{}, map[string]string{}, map[string]string{}
	for _, owner := range []string{"alice", "bob"} {
		account, err := f.accountStore.CreateAccount(t.Context(), owner, providers.GmailAccountRequest(owner+"@gmail.test", owner, "bound-subject"))
		if err != nil {
			t.Fatal(err)
		}
		grants[owner] = account.ID
		expires := time.Now().Add(time.Hour)
		if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, "google", "bound-subject", owner+"-access", owner+"-refresh", "Bearer", &expires, "mail"); err != nil {
			t.Fatal(err)
		}
		blobs[owner], err = f.blobs.StoreRaw(t.Context(), account.ID, 7, []byte(owner+" private MIME"))
		if err != nil {
			t.Fatal(err)
		}
		_, uploads[owner], err = f.blobs.StoreComposeAttachment(t.Context(), owner, "retained.txt", strings.NewReader(owner+" upload"))
		if err != nil {
			t.Fatal(err)
		}
	}
	blocked, releaseBody := s.setBlock("alice", "body")
	defer close(releaseBody)
	bodyDone := make(chan error, 1)
	go func() { bodyDone <- f.imap.EnsureBody(t.Context(), "alice", 1) }()
	awaitIMAP(t, blocked)
	// A multipart request has already passed authentication and acquired its
	// file pin when its body reader enters. No database lease is retained.
	var multipartBody bytes.Buffer
	form := multipart.NewWriter(&multipartBody)
	part, err := form.CreateFormFile("attachment", "late.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "must never publish")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	reader := &heldUploadReader{reader: &multipartBody, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseUpload sync.Once
	defer releaseUpload.Do(func() { close(reader.release) })
	req := httptest.NewRequest(http.MethodPost, "/compose/attachments", reader).WithContext(t.Context())
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	uploadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); f.http.ServeHTTP(w, req); uploadDone <- w }()
	awaitIMAP(t, reader.entered)
	// Already-admitted external credential work can itself need a file pin.
	// This proves account draining precedes exclusive file admission.
	externalEntered, releaseExternal := make(chan struct{}), make(chan struct{})
	var externalOnce sync.Once
	defer externalOnce.Do(func() { close(releaseExternal) })
	externalDone := make(chan error, 1)
	go func() {
		externalDone <- f.routing.WithAccountActivityForUser(t.Context(), "alice", grants["alice"], func() error {
			close(externalEntered)
			<-releaseExternal
			pin, err := f.blobs.PinUserFiles(t.Context(), "alice")
			if err == nil {
				pin()
			}
			return err
		})
	}()
	awaitIMAP(t, externalEntered)
	if w := b.delete(t, "alice", "alice", true); w.Code != http.StatusBadRequest {
		t.Fatal("active owner deletion accepted", w.Code, w.Body.String())
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if w := b.delete(t, "alice", "alice", false); w.Code != http.StatusForbidden {
		t.Fatal("deletion without CSRF accepted", w.Code)
	}
	if w := b.delete(t, "alice", "wrong", true); w.Code != http.StatusBadRequest {
		t.Fatal("incorrect confirmation accepted", w.Code)
	}
	if n := deletionSQLInt(t, f, `SELECT deletion_pending FROM users WHERE id='alice'`); n != 0 {
		t.Fatal("rejected requests changed deletion intent", n)
	}
	if w := b.delete(t, "alice", "alice", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case err := <-bodyDone:
		if err == nil {
			t.Fatal("blocked native fetch published after deletion")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("deletion did not cancel native fetch")
	}
	externalOnce.Do(func() { close(releaseExternal) })
	select {
	case err := <-externalDone:
		if err != nil {
			t.Fatal("file removal preceded account drain", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("external account activity did not drain")
	}
	waitUserIMAP(t, func() bool {
		pin, err := f.blobs.PinUserFiles(t.Context(), "alice")
		if err == nil {
			pin()
		}
		return errors.Is(err, store.ErrUserFilesRemoving)
	})
	if _, err := os.Stat(paths["alice"]); err != nil {
		t.Fatal("database removed before HTTP file pin drained", err)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_mailbox_credentials WHERE user_id='alice'`); n != 1 {
		t.Fatal("credentials removed before file drain", n)
	}
	if w := f.request("bob", http.MethodGet, "/api/settings/ui", ""); w.Code != http.StatusOK {
		t.Fatal("other owner unavailable with one cached store", w.Code)
	}
	// Hold the running removal while real HTTP requests fill its bounded queue.
	// The overflow request must still persist intent and explain automatic retry.
	for i := 0; i < 33; i++ {
		id := fmt.Sprintf("queue-%03d", i)
		if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,status) VALUES(?,?,?,'disabled')`, id, id, id); err != nil {
			t.Fatal(err)
		}
		w := b.delete(t, id, id, true)
		if w.Code != http.StatusSeeOther {
			t.Fatal("queued deletion", i, w.Code, w.Body.String())
		}
		if i == 32 {
			location, err := url.Parse(w.Header().Get("Location"))
			if err != nil || !strings.Contains(location.Query().Get("notice"), "pending") {
				t.Fatal("queue overflow lost its pending notice", w.Header(), err)
			}
		}
	}
	if w := b.delete(t, "queue-000", "queue-000", true); w.Code != http.StatusSeeOther {
		t.Fatal("duplicate queued intent did not coalesce", w.Code)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM auth_events WHERE subject_user_id='queue-000' AND event_type=?`, auth.AuthEventUserDeletionStarted); n != 1 {
		t.Fatal("duplicate HTTP retry repeated audit", n)
	}
	releaseUpload.Do(func() { close(reader.release) })
	select {
	case w := <-uploadDone:
		if w.Code != http.StatusForbidden {
			t.Fatal("multipart read published after owner revocation", w.Code, w.Body.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not drain")
	}
	awaitUserDeleted(t, f, "alice")
	waitUserIMAPFor(t, time.Minute, func() bool {
		return deletionSQLInt(t, f, `SELECT COUNT(*) FROM users WHERE id LIKE 'queue-%'`) == 0
	})
	for _, path := range []string{paths["alice"], paths["alice"] + "-wal", paths["alice"] + "-shm", blobs["alice"], uploads["alice"]} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("owned file retained", path, err)
		}
	}
	for _, path := range []string{paths["bob"], blobs["bob"], uploads["bob"]} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("other owner file lost", err)
		}
	}
	if token, err := f.credentials.GetOAuthTokenForUser(t.Context(), "bob", grants["bob"]); err != nil || token != "bob-access" {
		t.Fatal("other owner credential damaged", err)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_mailbox_credentials WHERE user_id='alice'`); n != 0 {
		t.Fatal("owned credential retained", n)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_account_directory WHERE user_id='alice' AND state='deleted'`); n != 2 {
		t.Fatal("account tombstones missing", n)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_user_cleanup_receipts WHERE user_id='alice'`); n != 1 {
		t.Fatal("final receipt missing", n)
	}
	if w := f.request("alice", http.MethodGet, "/api/settings/ui", ""); w.Code == http.StatusOK {
		t.Fatal("deleted browser session survived")
	}
	if err := f.imap.EnsureBody(t.Context(), "bob", 1); err != nil {
		t.Fatal("other owner native receive damaged", err)
	}
}

func restartUserDeletionRuntime(t *testing.T, f *userStorageFixture) (*handler.Handler, context.CancelFunc, *mail.UserIMAP) {
	t.Helper()
	f.stopIMAP()
	f.imap.Wait()
	f.base.WaitAvatarWorkers()
	if f.credentials != nil {
		f.credentials.Wait()
	}
	closing, finish := context.WithTimeout(context.Background(), time.Minute)
	err := f.stores.Close(closing)
	finish()
	if err != nil {
		t.Fatal(err)
	}
	path := f.system.Path()
	cfg := *f.auth.Config()
	hadCredentials := f.credentials != nil
	if err := f.system.Close(); err != nil {
		t.Fatal(err)
	}
	system, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	accounts, err := config.NewUserAccountStore(routing, key)
	if err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(&cfg, system, auth.Dependencies{BucketHashKey: key})
	ctx, cancel := context.WithCancel(t.Context())
	syncer := mail.NewSyncOrchestrator(system, nil, nil, nil)
	imap, err := mail.NewUserIMAP(ctx, accounts, f.blobs, syncer.Events())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h := handler.New(system, nil, syncer, f.blobs, manager, "test-vapid")
	option := handler.UserStorageOptions{Accounts: accounts, IMAP: imap}
	var credentials *mailauth.UserCredentials
	if hadCredentials {
		credentials, err = mailauth.NewUserCredentials(ctx, nil, routing, key)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		option.Credentials = credentials
	}
	t.Cleanup(func() {
		cancel()
		imap.Wait()
		h.WaitAvatarWorkers()
		if credentials != nil {
			credentials.Wait()
		}
	})
	mux := http.NewServeMux()
	if err := h.RegisterUserStorageRoutes(ctx, mux, routing, option); err != nil {
		t.Fatal(err)
	}
	h.RegisterAuthenticationRoutes(mux)
	h.RegisterAdministrationRoutes(mux)
	f.system, f.stores, f.routing, f.accountStore, f.auth = system, stores, routing, accounts, manager
	f.base, f.imap, f.stopIMAP, f.credentials = h, imap, cancel, credentials
	f.http = manager.Middleware(mux)
	return h, cancel, imap
}

func TestOwnedUserDeletionRestartRecoversPartialCleanup(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	b := newUserDeletionBrowser(t, f)
	path := ownedDBPath(t, f, "alice")
	_, upload, err := f.blobs.StoreComposeAttachment(t.Context(), "alice", "private.txt", strings.NewReader("private"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice';
 CREATE TRIGGER interrupt_user_cleanup BEFORE UPDATE OF state ON gofer_account_directory WHEN NEW.user_id='alice' AND NEW.state='deleted' BEGIN SELECT RAISE(ABORT,'injected cleanup interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if w := b.delete(t, "alice", "alice", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	waitUserIMAP(t, func() bool {
		return deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_user_store_directory WHERE user_id='alice' AND state='removed'`) == 1
	})
	f.stopIMAP()
	f.imap.Wait()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("database receipt preceded unlink", err)
	}
	if n := deletionSQLInt(t, f, `SELECT deletion_pending FROM users WHERE id='alice'`); n != 1 {
		t.Fatal("interrupted cleanup lost identity intent", n)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_user_cleanup_receipts WHERE user_id='alice'`); n != 0 {
		t.Fatal("partial cleanup published final receipt", n)
	}
	if _, err := os.Stat(upload); err != nil {
		t.Fatal("compose namespace removed before account completion", err)
	}
	if _, err := f.system.Write().Exec(`DROP TRIGGER interrupt_user_cleanup; UPDATE users SET status='disabled' WHERE id='deletion-admin'`); err != nil {
		t.Fatal(err)
	}
	// Startup resumes already accepted intent even after the initiating admin
	// loses access; no request/session is needed and no database is recreated.
	h, cancel, imap := restartUserDeletionRuntime(t, f)
	awaitUserDeleted(t, f, "alice")
	cancel()
	imap.Wait()
	h.WaitUserDeletions()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("restart recreated removed database", err)
	}
	if _, err := os.Stat(upload); !os.IsNotExist(err) {
		t.Fatal("restart left compose files", err)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM auth_events WHERE event_type=?`, auth.AuthEventUserDeletionStarted); n != 1 {
		t.Fatal("duplicate start audit", n)
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM auth_events WHERE event_type=?`, auth.AuthEventUserDeleted); n != 1 {
		t.Fatal("completion audit missing/duplicated", n)
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error { return nil }); err != nil {
		t.Fatal("restart damaged other owner", err)
	}
}

func TestOwnedUserDeletionShutdownDrainsWorkerAndRetainsIntent(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	b := newUserDeletionBrowser(t, f)
	path := ownedDBPath(t, f, "alice")
	pin, err := f.blobs.PinUserFiles(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer pin()
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if w := b.delete(t, "alice", "alice", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	waitUserIMAP(t, func() bool {
		newPin, err := f.blobs.PinUserFiles(t.Context(), "alice")
		if err == nil {
			newPin()
		}
		return errors.Is(err, store.ErrUserFilesRemoving)
	})
	f.stopIMAP()
	joined := make(chan struct{})
	go func() { f.imap.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("root shutdown did not join canceled removal drain")
	}
	if n := deletionSQLInt(t, f, `SELECT deletion_pending FROM users WHERE id='alice'`); n != 1 {
		t.Fatal("shutdown lost pending intent", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("shutdown removed database while pin remained", err)
	}
	pin()
	if w := b.delete(t, "alice", "alice", true); w.Code != http.StatusServiceUnavailable {
		t.Fatal("stopped runtime accepted cleanup", w.Code)
	}
	restartUserDeletionRuntime(t, f)
	awaitUserDeleted(t, f, "alice")
}

func TestOwnedUserDeletionRecoveryPagesBeyondQueueCapacity(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	b := newUserDeletionBrowser(t, f)
	f.stopIMAP()
	f.imap.Wait()
	// Real confirmed auth transitions populate more intents than a queue or
	// discovery page. These never-used owners have no database/blob files.
	for i := 0; i < 65; i++ {
		id := fmt.Sprintf("recovery-%03d", i)
		if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,status) VALUES(?,?,?,'disabled')`, id, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.PrepareAdministratorUserDeletion(t.Context(), auth.PrepareAdministratorUserDeletionOptions{
			ActorUserID: b.session.UserID, ActorSessionID: b.session.ID, TargetUserID: id, Confirmation: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	restartUserDeletionRuntime(t, f)
	waitUserIMAPFor(t, time.Minute, func() bool {
		return deletionSQLInt(t, f, `SELECT COUNT(*) FROM users WHERE id LIKE 'recovery-%'`) == 0
	})
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM gofer_user_cleanup_receipts WHERE user_id LIKE 'recovery-%'`); n != 65 {
		t.Fatal("recovery skipped intents", n)
	}
	if w := f.request("bob", http.MethodGet, "/api/settings/ui", ""); w.Code != http.StatusOK {
		t.Fatal("recovery damaged retained owner", w.Code)
	}
}

func TestOwnedAccountDeletionRestartReplaysIntentForDisabledOwner(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	path := ownedDBPath(t, f, "alice")
	account := f.accounts["alice"].ID
	blob, err := f.blobs.StoreRaw(t.Context(), account, 7, []byte("owned account bytes"))
	if err != nil {
		t.Fatal(err)
	}
	_, upload, err := f.blobs.StoreComposeAttachment(t.Context(), "alice", "retained.txt", strings.NewReader("owner retained"))
	if err != nil {
		t.Fatal(err)
	}
	// Stop after the durable transition, before external/local account cleanup.
	f.stopIMAP()
	f.imap.Wait()
	if err := f.routing.RequestAccountDeletion(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	restartUserDeletionRuntime(t, f)
	waitUserAccountState(t, f, "alice", account, storage.AccountDeleted)
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatal("replayed account retained blobs", err)
	}
	for _, retained := range []string{path, upload} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatal("account cleanup removed owner namespace", retained, err)
		}
	}
	if n := deletionSQLInt(t, f, `SELECT COUNT(*) FROM users WHERE id='alice' AND status='disabled' AND deletion_pending=0`); n != 1 {
		t.Fatal("account cleanup removed/changed owner identity", n)
	}
	if w := f.request("bob", http.MethodGet, "/api/settings/ui", ""); w.Code != http.StatusOK {
		t.Fatal("account replay affected another user", w.Code)
	}
}
