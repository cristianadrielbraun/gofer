package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func pendingStoreOwner(t *testing.T, system *DB, owner string) {
	t.Helper()
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
}
func storeDirectoryState(t *testing.T, system *DB, owner string) string {
	t.Helper()
	var state string
	if err := system.Read().QueryRow(`SELECT state FROM gofer_user_store_directory WHERE user_id=?`, owner).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestUserStoreLifecycleMissingContactOnlyStoreIsNotReplaced(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		return db.SetUISettings(t.Context(), "alice", map[string]string{"theme": "private"})
	}); err != nil {
		t.Fatal(err)
	}
	if storeDirectoryState(t, system, "alice") != "present" {
		t.Fatal("owner file history missing")
	}
	if err := r.WithUser(t.Context(), "bob", func(*DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stores.userPath("alice")); err != nil {
		t.Fatal(err)
	}
	if err := r.WithUser(t.Context(), "alice", func(*DB) error { t.Error("replacement created"); return nil }); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := r.ReadUserDiagnostics(t.Context(), DiagnosticsActor{ID: "admin", AuthVersion: 1}, "alice", UserDiagnosticsContacts); !errors.Is(err, ErrAccountRoute) {
		t.Fatal("diagnostic hid lost contact-only file", err)
	}
	if _, err := os.Stat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing file replaced", err)
	}
}

func TestUserStoreLifecycleDrainsLeaseAndPreservesOtherOwner(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 2)
	alice, err := stores.Acquire(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Release()
	if err := r.WithUser(t.Context(), "bob", func(db *DB) error { return db.SetUISettings(t.Context(), "bob", map[string]string{"theme": "bob"}) }); err != nil {
		t.Fatal(err)
	}
	pendingStoreOwner(t, system, "alice")
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.RemovePendingUserStore(short, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pinned file was removed", err)
	}
	if storeDirectoryState(t, system, "alice") != "removing" {
		t.Fatal("drain intent lost")
	}
	if _, err := os.Stat(stores.userPath("alice")); err != nil {
		t.Fatal("file removed before lease drain", err)
	}
	if _, err := stores.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal("removing owner admitted", err)
	}
	if err := r.WithUser(t.Context(), "bob", func(db *DB) error {
		if db.GetUISettings(t.Context(), "bob")["theme"] != "bob" {
			t.Error("other owner changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	alice.Release()
	if err := r.RemovePendingUserStore(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if removed, err := r.PendingUserStoreRemoved(t.Context(), "alice"); err != nil || !removed {
		t.Fatal(removed, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(stores.userPath("alice") + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(suffix, err)
		}
	}
	if err := r.RemovePendingUserStore(t.Context(), "alice"); err != nil {
		t.Fatal("retry", err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET deletion_pending=0,status='active' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal("removed identity recreated storage", err)
	}
}

func TestUserStoreLifecycleWaitsForOpeningAndPhysicalClose(t *testing.T) {
	for _, stage := range []string{"opening", "closing"} {
		t.Run(stage, func(t *testing.T) {
			system, stores, r := newAccountRoutingTest(t, 2)
			if err := r.WithUser(t.Context(), "bob", func(*DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			unlock := sync.OnceFunc(func() { close(release) })
			defer unlock()
			var aliceDB *DB
			var opened chan error
			if stage == "opening" {
				stores.openStore = func(ctx context.Context, owner userStoreOwner, create bool) (*DB, error) {
					db, err := stores.openUserStore(ctx, owner, create)
					if owner.id == "alice" && err == nil {
						aliceDB = db
						close(entered)
						<-release
					}
					return db, err
				}
				opened = make(chan error, 1)
				go func() {
					lease, err := stores.Acquire(t.Context(), "alice")
					if lease != nil {
						lease.Release()
					}
					opened <- err
				}()
				<-entered
			} else {
				lease, err := stores.Acquire(t.Context(), "alice")
				if err != nil {
					t.Fatal(err)
				}
				aliceDB = lease.DB()
				lease.Release()
				stores.closeStore = func(db *DB) error {
					if db == aliceDB {
						close(entered)
						<-release
					}
					return db.Close()
				}
			}
			pendingStoreOwner(t, system, "alice")
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if err := r.RemovePendingUserStore(ctx, "alice"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(stage, err)
			}
			if stage == "closing" {
				<-entered
			}
			if _, err := os.Stat(stores.userPath("alice")); err != nil {
				t.Fatal("unlinked before physical handle drain", err)
			}
			bobCtx, bobCancel := context.WithTimeout(t.Context(), time.Second)
			defer bobCancel()
			if err := r.WithUser(bobCtx, "bob", func(*DB) error { return nil }); err != nil {
				t.Fatal("drain held global cache mutex", err)
			}
			unlock()
			if opened != nil {
				if err := <-opened; !errors.Is(err, ErrUserStoreOwner) {
					t.Fatal("opening was admitted after removal intent", err)
				}
			}
			stores.removeStoreFile = func(path string) error {
				if err := aliceDB.Read().Ping(); err == nil || !strings.Contains(err.Error(), "closed") {
					t.Error("unlink before database close", err)
				}
				return os.Remove(path)
			}
			if err := r.RemovePendingUserStore(t.Context(), "alice"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserStoreLifecycleInterruptedUnlinkResumesAfterRestart(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	lease, err := stores.Acquire(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	pendingStoreOwner(t, system, "alice")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := stores.userPath("alice")
	stores.removeStoreFile = func(file string) error {
		err := os.Remove(file)
		if file == path && err == nil {
			if err := os.WriteFile(path+"-wal", []byte("interrupted sidecar"), 0600); err != nil {
				t.Fatal(err)
			}
			cancel()
		}
		return err
	}
	if err := r.RemovePendingUserStore(ctx, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if storeDirectoryState(t, system, "alice") != "removing" {
		t.Fatal("interrupted removal was completed")
	}
	closeCtx, closeCancel := context.WithTimeout(t.Context(), time.Minute)
	defer closeCancel()
	if err := stores.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewUserStores(system, UserStoreOptions{Directory: stores.directory, MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := restarted.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := restarted.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal("restart reopened removed file", err)
	}
	if err := restarted.RemovePendingUserStore(t.Context(), "alice"); err != nil {
		t.Fatal("restart retry", err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if storeDirectoryState(t, system, "alice") != "removed" {
		t.Fatal("completion receipt missing")
	}
}

func TestUserStoreLifecycleRejectsWrongIdentityAndSidecar(t *testing.T) {
	for _, mode := range []string{"foreign", "symlink", "sidecar-directory", "active-owner", "management-owner"} {
		t.Run(mode, func(t *testing.T) {
			system, stores, r := newAccountRoutingTest(t, 1)
			alice, err := stores.Acquire(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			alice.Release()
			bob, err := stores.Acquire(t.Context(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			bob.Release()
			path := stores.userPath("alice")
			if mode == "active-owner" || mode == "management-owner" {
				owner := "alice"
				if mode == "management-owner" {
					owner = "admin"
				}
				if err := r.RemovePendingUserStore(t.Context(), owner); !errors.Is(err, ErrUserStoreOwner) {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("unauthorized removal", err)
				}
				return
			}
			pendingStoreOwner(t, system, "alice")
			switch mode {
			case "foreign":
				data, err := os.ReadFile(stores.userPath("bob"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(stores.userPath("bob"), path); err != nil {
					t.Fatal(err)
				}
			case "sidecar-directory":
				if err := os.Mkdir(path+"-wal", 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.RemovePendingUserStore(t.Context(), "alice"); !errors.Is(err, ErrUserStoreIdentity) {
				t.Fatal(mode, err)
			}
			if storeDirectoryState(t, system, "alice") != "removing" {
				t.Fatal("invalid file accepted as removed")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("invalid file was erased", err)
			}
			if _, err := os.Stat(stores.userPath("bob")); err != nil {
				t.Fatal("foreign file erased", err)
			}
		})
	}
}
