package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

func waitUserStorePhysicalClose(t *testing.T, m *UserStores, owner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		m.mu.Lock()
		_, present := m.entries[owner]
		changed := m.changed
		m.mu.Unlock()
		if !present {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("physical store close did not finish", owner, ctx.Err())
		}
	}
}

func assertCachedOwnerUsableWhileClosing(t *testing.T, m *UserStores, owner string, unblock func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.WithUser(ctx, owner, func(db *DB) error { return db.Read().PingContext(ctx) })
	}()
	select {
	case err := <-done:
		if err != nil {
			unblock()
			t.Fatal("other cached owner could not use its SQLite connection", err)
		}
	case <-ctx.Done():
		unblock()
		t.Fatal("physical close held the global cache lock", ctx.Err())
	}
}

func TestUserStoresBlockedPhysicalClosePreservesCapacityOwnerAndRuntimeLock(t *testing.T) {
	for _, mode := range []string{"eviction", "idle"} {
		t.Run(mode, func(t *testing.T) {
			system := newUserStoreTestSystem(t)
			m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 2, IdleTimeout: time.Hour})
			alice, bob := acquireUserStore(t, m, "alice"), acquireUserStore(t, m, "bob")
			aliceDB, bobDB := alice.DB(), bob.DB()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var active, maximum, aliceCloses atomic.Int32
			m.mu.Lock()
			m.closeStore = func(db *DB) error {
				n := active.Add(1)
				for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
				}
				defer active.Add(-1)
				if db == aliceDB {
					aliceCloses.Add(1)
					close(entered)
					<-release
				}
				return db.Close()
			}
			m.mu.Unlock()
			alice.Release()
			if mode == "eviction" {
				m.mu.Lock()
				scheduled := m.evictOldestLocked()
				m.mu.Unlock()
				if !scheduled {
					t.Fatal("idle eviction not scheduled")
				}
			} else {
				m.mu.Lock()
				now := time.Now().Add(2 * time.Hour)
				m.now = func() time.Time { return now }
				m.mu.Unlock()
				m.reapIdle()
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("physical close did not begin")
			}
			assertCachedOwnerUsableWhileClosing(t, m, "bob", unblock)
			for _, owner := range []string{"alice", "../../outside"} {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
				lease, err := m.Acquire(ctx, owner)
				cancel()
				if lease != nil {
					lease.Release()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("closing owner reopened or closing handle stopped counting toward capacity", owner, err)
				}
			}
			m.mu.Lock()
			count, closing := len(m.entries), m.entries["alice"].closing
			m.mu.Unlock()
			if count != 2 || !closing || aliceDB.Read().Ping() != nil {
				t.Fatal("closing database was forgotten before its real close", count, closing)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal("shutdown could not be canceled during physical close", err)
			}
			bob.Release()
			if next, err := NewUserStores(system, UserStoreOptions{MaxOpen: 2}); !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
				if next != nil {
					_ = next.Close(t.Context())
				}
				t.Fatal("runtime lock released while a database was still physically open", err)
			}
			unblock()
			ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := m.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if aliceDB.Read().Ping() == nil || bobDB.Read().Ping() == nil || aliceCloses.Load() != 1 || maximum.Load() != 1 {
				t.Fatal("shutdown failed to close or serialize real SQL pools", aliceCloses.Load(), maximum.Load())
			}
			newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 2})
		})
	}
}

func TestUserStoresCanceledSuccessfulOpeningKeepsPhysicalHandleAccounted(t *testing.T) {
	for _, mode := range []string{"request-cancel", "runtime-close"} {
		t.Run(mode, func(t *testing.T) {
			system := newUserStoreTestSystem(t)
			m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 2})
			bob := acquireUserStore(t, m, "bob")
			opened := make(chan *DB, 1)
			finishOpening, finishClose := make(chan struct{}), make(chan struct{})
			closeEntered := make(chan struct{})
			var openOnce, closeOnce sync.Once
			unblockOpening := func() { openOnce.Do(func() { close(finishOpening) }) }
			unblockClose := func() { closeOnce.Do(func() { close(finishClose) }) }
			defer unblockOpening()
			defer unblockClose()
			var aliceDB *DB
			m.mu.Lock()
			m.openStore = func(ctx context.Context, owner userStoreOwner, create bool) (*DB, error) {
				db, err := m.openUserStore(ctx, owner, create)
				if err == nil && owner.id == "alice" {
					opened <- db
					<-finishOpening
				}
				return db, err
			}
			m.closeStore = func(db *DB) error {
				if db == aliceDB {
					close(closeEntered)
					<-finishClose
				}
				return db.Close()
			}
			m.mu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				lease, err := m.Acquire(ctx, "alice")
				if lease != nil {
					lease.Release()
				}
				done <- err
			}()
			select {
			case aliceDB = <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("native SQLite opening did not finish")
			}
			want := ErrUserStoresClosed
			if mode == "request-cancel" {
				cancel()
				want = context.Canceled
			} else {
				closing, stop := context.WithCancel(t.Context())
				stop()
				if err := m.Close(closing); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			unblockOpening()
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatal("canceled successful open returned an authorized lease", err)
				}
			case <-time.After(time.Second):
				unblockClose()
				t.Fatal("opening cleanup waited for fsync instead of scheduling close")
			}
			select {
			case <-closeEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("canceled opened handle did not reach the physical closer")
			}
			m.mu.Lock()
			entry := m.entries["alice"]
			count := len(m.entries)
			m.mu.Unlock()
			if entry == nil || !entry.closing || entry.opening || entry.refs != 0 || count != 2 {
				t.Fatal("canceled open lost physical capacity accounting", entry, count)
			}
			if mode == "request-cancel" {
				assertCachedOwnerUsableWhileClosing(t, m, "bob", unblockClose)
			}
			unblockClose()
			waitUserStorePhysicalClose(t, m, "alice")
			if aliceDB.Read().Ping() == nil {
				t.Fatal("canceled opened SQL pools remained usable")
			}
			bob.Release()
			closing, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			if err := m.Close(closing); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserStoresPhysicalCloseErrorStillDrainsAndReleasesLock(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m, err := NewUserStores(system, UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected physical close error")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); !errors.Is(err, failure) {
			t.Error("lost original close error", err)
		}
	})
	lease := acquireUserStore(t, m, "alice")
	db := lease.DB()
	m.mu.Lock()
	m.closeStore = func(db *DB) error { return errors.Join(db.Close(), failure) }
	m.mu.Unlock()
	lease.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, failure) || db.Read().Ping() == nil {
		t.Fatal("physical close error dropped or database leaked", err)
	}
	if err := system.Read().Ping(); err != nil {
		t.Fatal("manager closed its borrowed system database", err)
	}
	newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
}

func TestUserStoresWaitingForCloseDoesNotEvictOtherIdleOwner(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 2, IdleTimeout: time.Hour})
	alice, bob := acquireUserStore(t, m, "alice"), acquireUserStore(t, m, "bob")
	aliceDB, bobDB := alice.DB(), bob.DB()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m.mu.Lock()
	m.closeStore = func(db *DB) error {
		if db == aliceDB {
			close(entered)
			<-release
		}
		return db.Close()
	}
	m.mu.Unlock()
	alice.Release()
	bob.Release()
	m.mu.Lock()
	scheduled := m.evictOldestLocked()
	m.mu.Unlock()
	if !scheduled {
		t.Fatal("oldest idle store was not scheduled for close")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("oldest store close did not begin")
	}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		lease, err := m.Acquire(ctx, "../../outside")
		cancel()
		if lease != nil {
			lease.Release()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("waiting acquisition bypassed physical close capacity", err)
		}
		m.mu.Lock()
		entry := m.entries["bob"]
		retained := entry != nil && !entry.closing && entry.db == bobDB
		m.mu.Unlock()
		if !retained {
			t.Fatal("waiting requests evicted the other idle owner")
		}
		assertCachedOwnerUsableWhileClosing(t, m, "bob", unblock)
	}
	unblock()
	waitUserStorePhysicalClose(t, m, "alice")
	lease := acquireUserStore(t, m, "../../outside")
	if err := lease.DB().Read().Ping(); err != nil {
		t.Fatal("freed capacity could not open the waiting owner", err)
	}
}
