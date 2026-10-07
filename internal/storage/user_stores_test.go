package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

func newUserStoreTestSystem(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, id := range []string{"alice", "bob", "../../outside"} {
		username := id
		if id == "../../outside" {
			username = "path-user"
		}
		if _, err := db.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES (?, ?, ?)`, id, username, username); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id, username, username_normalized, is_admin, user_type)
		VALUES ('admin', 'admin', 'admin', 1, 'management')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func newUserStoreTestManager(t *testing.T, system *DB, options UserStoreOptions) *UserStores {
	t.Helper()
	m, err := NewUserStores(system, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}

func acquireUserStore(t *testing.T, m *UserStores, id string) *UserStoreLease {
	t.Helper()
	lease, err := m.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	return lease
}

func TestUserStoresSeparateRealRepositoryDataAndAuthentication(t *testing.T) {
	system := newUserStoreTestSystem(t)
	if _, err := system.Write().Exec(`INSERT INTO password_credentials(user_id, password_hash) VALUES ('alice', 'synthetic-secret')`); err != nil {
		t.Fatal(err)
	}
	m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 2})
	alice := acquireUserStore(t, m, "alice")
	bob := acquireUserStore(t, m, "bob")
	for id, lease := range map[string]*UserStoreLease{"alice": alice, "bob": bob} {
		db := lease.DB()
		if _, err := db.SaveContact(t.Context(), id, models.Contact{ID: "same-contact", Name: id, Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetUISettings(t.Context(), id, map[string]string{"theme": id}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES ('acc', ?, ?)`, id, id+"@example.com"); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertFolders(t.Context(), []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{{AccountID: "acc", FolderID: "inbox", MessageID: "<" + id + "@example.com>", Subject: id + "project", RemoteUID: 1, DateSent: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
		var subject string
		if err := db.Read().QueryRow(`SELECT subject FROM message_search WHERE message_search MATCH ?`, id+"project").Scan(&subject); err != nil || subject != id+"project" {
			t.Fatalf("search: subject=%q err=%v", subject, err)
		}
		for _, table := range centralAuthenticationTables {
			var count int
			if err := db.Read().QueryRow("SELECT COUNT(*) FROM " + quoteStoreIdentifier(table)).Scan(&count); err != nil || count != 0 {
				t.Fatalf("auth table %s: rows=%d err=%v", table, count, err)
			}
		}
		if _, err := db.Write().Exec(`INSERT INTO password_credentials(user_id, password_hash) VALUES (?, 'must-fail')`, id); err == nil {
			t.Fatal("authentication write accepted in user store")
		}
		rows, err := db.Read().Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		hasViolation, rowErr := rows.Next(), rows.Err()
		rows.Close()
		if hasViolation || rowErr != nil {
			t.Fatalf("FK check: violation=%t err=%v", hasViolation, rowErr)
		}
	}
	for id, lease := range map[string]*UserStoreLease{"alice": alice, "bob": bob} {
		var name string
		if err := lease.DB().Read().QueryRow(`SELECT display_name FROM contact_profiles WHERE id = 'same-contact'`).Scan(&name); err != nil || name != id {
			t.Fatalf("contact: name=%q err=%v", name, err)
		}
		if got := lease.DB().GetUISettings(t.Context(), id)["theme"]; got != id {
			t.Fatalf("theme=%q want %q", got, id)
		}
	}
	var centralContacts, centralMessages, credentials int
	if err := system.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM contact_profiles), (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM password_credentials)`).Scan(&centralContacts, &centralMessages, &credentials); err != nil {
		t.Fatal(err)
	}
	if centralContacts != 0 || centralMessages != 0 || credentials != 1 {
		t.Fatalf("central counts: contacts=%d messages=%d credentials=%d", centralContacts, centralMessages, credentials)
	}
}

func TestUserStoreRejectsForeignAndNullOwners(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{})
	db := acquireUserStore(t, m, "alice").DB()
	for _, query := range []string{
		`INSERT INTO users(id, username, username_normalized) VALUES ('bob', 'bob', 'bob')`,
		`INSERT INTO accounts(id, user_id, email_address) VALUES ('foreign', 'bob', 'bob@example.com')`,
		`INSERT INTO accounts(id, email_address) VALUES ('unowned', 'unowned@example.com')`,
		`INSERT INTO app_settings(user_id, key, value) VALUES ('bob', 'theme', 'foreign')`,
		`UPDATE users SET id = 'bob' WHERE id = 'alice'`,
		`UPDATE users SET user_type = 'management', is_admin = 1 WHERE id = 'alice'`,
		`DELETE FROM users WHERE id = 'alice'`,
		`UPDATE gofer_user_store SET user_id = 'bob'`,
		`DELETE FROM gofer_user_store`,
	} {
		if _, err := db.Write().Exec(query); err == nil {
			t.Fatalf("accepted foreign ownership mutation: %s", query)
		}
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES ('owned', 'alice', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE accounts SET user_id = NULL WHERE id = 'owned'`); err == nil {
		t.Fatal("account reassignment accepted")
	}
	for _, owner := range []string{"", "missing", "admin"} {
		if _, err := m.Acquire(t.Context(), owner); !errors.Is(err, ErrUserStoreOwner) {
			t.Fatalf("owner %q: %v", owner, err)
		}
	}
}

func TestUserStoresBoundCachePinLeasesAndPreserveEvictedData(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
	first := acquireUserStore(t, m, "alice")
	second := acquireUserStore(t, m, "alice")
	if first.DB() != second.DB() {
		t.Fatal("same owner acquired different stores")
	}
	if err := first.DB().SetUISettings(t.Context(), "alice", map[string]string{"theme": "retained"}); err != nil {
		t.Fatal(err)
	}
	first.Release()
	first.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx, "bob"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full cache acquisition: %v", err)
	}
	if err := second.DB().Read().Ping(); err != nil {
		t.Fatalf("leased store closed: %v", err)
	}
	second.Release()
	bob := acquireUserStore(t, m, "bob")
	if err := first.DB().Read().Ping(); err == nil {
		t.Fatal("evicted store remained open")
	}
	bob.Release()
	reopened := acquireUserStore(t, m, "alice")
	if reopened.DB() == first.DB() {
		t.Fatal("evicted handle reused")
	}
	if got := reopened.DB().GetUISettings(t.Context(), "alice")["theme"]; got != "retained" {
		t.Fatalf("reopened data: %q", got)
	}
}

func TestUserStoresConcurrentAcquisitionsShareOneStore(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
	const callers = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *UserStoreLease, callers)
	errors := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := m.Acquire(t.Context(), "alice")
			if err != nil {
				errors <- err
				return
			}
			results <- lease
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	var db *DB
	for lease := range results {
		if db != nil && db != lease.DB() {
			t.Error("multiple databases for concurrent owner")
		}
		db = lease.DB()
		lease.Release()
	}
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 1 {
		t.Fatalf("open stores=%d", count)
	}
}

func TestUserStoresCloseDrainsLeasesAndReleasesManagerLock(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m := newUserStoreTestManager(t, system, UserStoreOptions{})
	if _, err := NewUserStores(system, UserStoreOptions{}); !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
		t.Fatalf("second manager: %v", err)
	}
	lease := acquireUserStore(t, m, "alice")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close with active lease: %v", err)
	}
	if _, err := m.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoresClosed) {
		t.Fatalf("acquisition during shutdown: %v", err)
	}
	if err := lease.DB().Read().Ping(); err != nil {
		t.Fatalf("shutdown closed leased database: %v", err)
	}
	lease.Release()
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lease.DB().Read().Ping(); err == nil {
		t.Fatal("store remained open after shutdown")
	}
	if err := system.Read().Ping(); err != nil {
		t.Fatalf("system database was closed: %v", err)
	}
	newUserStoreTestManager(t, system, UserStoreOptions{})
}

func TestUserStoresIdleExpiryNeverClosesAnActiveLease(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{IdleTimeout: time.Hour})
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.now = func() time.Time { return time.Unix(0, now.Load()) }
	m.mu.Unlock()
	alice := acquireUserStore(t, m, "alice")
	bob := acquireUserStore(t, m, "bob")
	alice.Release()
	now.Add(int64(2 * time.Hour))
	m.reapIdle()
	waitUserStorePhysicalClose(t, m, "alice")
	if err := alice.DB().Read().Ping(); err == nil {
		t.Fatal("idle store remained open")
	}
	if err := bob.DB().Read().Ping(); err != nil {
		t.Fatalf("active store closed: %v", err)
	}
}

func TestUserStoresRejectUnmarkedOrWrongIdentityWithoutChangingFiles(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
	path := m.userPath("alice")
	unmarked, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := unmarked.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreIdentity) {
		t.Fatalf("unmarked database: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("unmarked database changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	alice := acquireUserStore(t, m, "alice")
	alice.Release()
	m.mu.Lock()
	m.evictOldestLocked()
	m.mu.Unlock()
	waitUserStorePhysicalClose(t, m, "alice")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.userPath("bob"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(t.Context(), "bob"); !errors.Is(err, ErrUserStoreIdentity) {
		t.Fatalf("wrong owner database: %v", err)
	}
	if err := m.WithUser(t.Context(), "alice", func(db *DB) error { return db.Read().Ping() }); err != nil {
		t.Fatal(err)
	}
}

func TestUserStoresHashOwnerPathsAndRejectSymlinks(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m := newUserStoreTestManager(t, system, UserStoreOptions{})
	lease := acquireUserStore(t, m, "../../outside")
	if filepath.Dir(lease.DB().Path()) != m.directory || strings.Contains(filepath.Base(lease.DB().Path()), "outside") {
		t.Fatalf("unsafe user path: %s", lease.DB().Path())
	}
	if err := os.Symlink(system.Path(), m.userPath("alice")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := m.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreIdentity) {
		t.Fatalf("symlink acquisition: %v", err)
	}
	link := filepath.Join(t.TempDir(), "linked-directory")
	if err := os.Symlink(m.directory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewUserStores(system, UserStoreOptions{Directory: link}); err == nil {
		t.Fatal("symlink directory accepted")
	}
}

func TestUserStoresCallbackReleasesLeaseAfterFailure(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
	failed := errors.New("operation failed")
	if err := m.WithUser(t.Context(), "alice", func(*DB) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if err := m.WithUser(t.Context(), "bob", func(db *DB) error { return db.Read().Ping() }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.system.Write().Exec(`DELETE FROM users WHERE id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(t.Context(), "bob"); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatalf("cached deleted owner: %v", err)
	}
}

func TestUserStoreKeepsMailAndSearchAtomicOnIndexFailure(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{})
	db := acquireUserStore(t, m, "alice").DB()
	if _, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES ('acc', 'alice', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(t.Context(), []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	useSearchFailureWriter(t, db)
	before := searchMutationSnapshot(t, db)
	indexFailure := errors.New("synthetic index failure")
	ctx := context.WithValue(t.Context(), searchFailureKey{}, func() error { return indexFailure })
	_, err := db.SaveDraftMessage(ctx, DraftMessageInput{AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@example.com>", Subject: "Uncommitted draft", Date: time.Now().UTC()})
	if !errors.Is(err, indexFailure) {
		t.Fatalf("draft error: %v", err)
	}
	if after := searchMutationSnapshot(t, db); after != before {
		t.Fatal("failed draft left committed mail or search changes")
	}
}
