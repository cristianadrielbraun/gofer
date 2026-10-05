package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

var (
	ErrUserStoresClosed  = errors.New("user database manager is closing")
	ErrUserStoreOwner    = errors.New("user database requires an existing webmail user")
	ErrUserStoreIdentity = errors.New("user database identity does not match its owner")
)

// UserStoreOptions bounds database handles, not registered users. Each open
// store uses the existing modernc writer/reader pools. Directory must be private
// to this installation; an exclusive lock prevents competing cache managers.
type UserStoreOptions struct {
	Directory   string
	MaxOpen     int
	IdleTimeout time.Duration
}

// UserStores owns user database handles. It borrows System, which must outlive
// the manager. Authentication remains in System; Acquire is not authorization.
// Callers must authenticate/authorize the owner before acquiring its database.
// This foundation does not select the layout for the application or split an
// existing shared database; those require the explicit migration and routing.
type UserStores struct {
	system      *DB
	directory   string
	maxOpen     int
	idleTimeout time.Duration
	lock        *runtimeguard.Lock
	mu          sync.Mutex
	entries     map[string]*userStoreEntry
	changed     chan struct{}
	wake        chan struct{}
	done        chan struct{}
	closing     bool
	closeErr    error
	now         func() time.Time
}

type userStoreEntry struct {
	db        *DB
	opening   bool
	refs      int
	idleSince time.Time
}

// UserStoreLease pins a database until Release. DB and any rows/transactions
// obtained from it must not escape that lifetime. Release is idempotent; callers
// should defer it immediately after a successful Acquire.
type UserStoreLease struct {
	manager *UserStores
	owner   string
	entry   *userStoreEntry
	once    sync.Once
}

func NewUserStores(system *DB, options UserStoreOptions) (*UserStores, error) {
	if system == nil || system.Path() == "" {
		return nil, errors.New("system database is required")
	}
	if options.MaxOpen < 0 || options.IdleTimeout < 0 {
		return nil, errors.New("user database cache limits must not be negative")
	}
	if options.MaxOpen == 0 {
		options.MaxOpen = 16
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = 2 * time.Minute
	}
	if options.Directory == "" {
		options.Directory = system.Path() + ".users"
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolve user database directory: %w", err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("user database directory must be a real directory")
	}
	lock, err := runtimeguard.Acquire(filepath.Join(directory, "manager"))
	if err != nil {
		return nil, err
	}
	m := &UserStores{
		system: system, directory: directory, maxOpen: options.MaxOpen,
		idleTimeout: options.IdleTimeout, lock: lock,
		entries: make(map[string]*userStoreEntry), changed: make(chan struct{}),
		wake: make(chan struct{}, 1), done: make(chan struct{}), now: time.Now,
	}
	go m.reap()
	return m, nil
}

func (m *UserStores) Acquire(ctx context.Context, userID string) (*UserStoreLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return nil, ErrUserStoresClosed
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner, err := m.lookupOwner(ctx, userID)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil, ErrUserStoresClosed
		}
		if entry, ok := m.entries[userID]; ok && !entry.opening {
			entry.refs++
			m.mu.Unlock()
			return &UserStoreLease{manager: m, owner: userID, entry: entry}, nil
		} else if ok {
			changed := m.changed
			m.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if len(m.entries) >= m.maxOpen {
			if !m.evictOldestLocked() {
				changed := m.changed
				m.mu.Unlock()
				select {
				case <-changed:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		entry := &userStoreEntry{opening: true, refs: 1}
		m.entries[userID] = entry
		m.mu.Unlock()

		db, err := m.openUserStore(ctx, owner)
		m.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && m.closing {
			err = ErrUserStoresClosed
		}
		if err != nil {
			if db != nil {
				m.closeErr = errors.Join(m.closeErr, db.Close())
			}
			delete(m.entries, userID)
			m.signalLocked()
			m.mu.Unlock()
			m.wakeReaper()
			return nil, err
		}
		entry.db, entry.opening = db, false
		m.signalLocked()
		m.mu.Unlock()
		return &UserStoreLease{manager: m, owner: userID, entry: entry}, nil
	}
}

func (lease *UserStoreLease) DB() *DB { return lease.entry.db }

func (lease *UserStoreLease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		m := lease.manager
		m.mu.Lock()
		lease.entry.refs--
		if lease.entry.refs == 0 {
			lease.entry.idleSince = m.now()
		}
		m.signalLocked()
		m.mu.Unlock()
		m.wakeReaper()
	})
}

func (m *UserStores) WithUser(ctx context.Context, userID string, fn func(*DB) error) error {
	lease, err := m.Acquire(ctx, userID)
	if err != nil {
		return err
	}
	defer lease.Release()
	return fn(lease.DB())
}

// Close rejects new acquisitions and drains outstanding leases. A timeout does
// not close databases in use: shutdown continues when those leases are released.
// The borrowed system database is never closed by this manager.
func (m *UserStores) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	m.signalLocked()
	m.mu.Unlock()
	m.wakeReaper()
	select {
	case <-m.done:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *UserStores) signalLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *UserStores) wakeReaper() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *UserStores) closeEntryLocked(owner string, entry *userStoreEntry) {
	m.closeErr = errors.Join(m.closeErr, entry.db.Close())
	delete(m.entries, owner)
	m.signalLocked()
}

func (m *UserStores) evictOldestLocked() bool {
	var oldest *userStoreEntry
	var owner string
	for id, entry := range m.entries {
		if entry.opening || entry.refs != 0 {
			continue
		}
		if oldest == nil || entry.idleSince.Before(oldest.idleSince) {
			owner, oldest = id, entry
		}
	}
	if oldest == nil {
		return false
	}
	m.closeEntryLocked(owner, oldest)
	return true
}

func (m *UserStores) reapIdle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for owner, entry := range m.entries {
		if entry.opening || entry.refs != 0 {
			continue
		}
		if m.closing || now.Sub(entry.idleSince) >= m.idleTimeout {
			m.closeEntryLocked(owner, entry)
		}
	}
	if m.closing && len(m.entries) == 0 {
		m.closeErr = errors.Join(m.closeErr, m.lock.Close())
		return true
	}
	return false
}

func (m *UserStores) reap() {
	interval := min(m.idleTimeout, time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(m.done)
	for {
		select {
		case <-ticker.C:
		case <-m.wake:
		}
		if m.reapIdle() {
			return
		}
	}
}

type userStoreOwner struct{ id, username, normalized, name, avatar string }

func (m *UserStores) lookupOwner(ctx context.Context, userID string) (userStoreOwner, error) {
	var owner userStoreOwner
	err := m.system.Read().QueryRowContext(ctx, `SELECT id, username, username_normalized, name, avatar_url
		FROM users WHERE id = ? AND user_type = 'webmail' AND is_admin = 0`, userID).
		Scan(&owner.id, &owner.username, &owner.normalized, &owner.name, &owner.avatar)
	if errors.Is(err, sql.ErrNoRows) {
		return owner, ErrUserStoreOwner
	}
	return owner, err
}

func (m *UserStores) userPath(userID string) string {
	hash := sha256.Sum256([]byte(userID))
	return filepath.Join(m.directory, hex.EncodeToString(hash[:])+".db")
}

func (m *UserStores) openUserStore(ctx context.Context, owner userStoreOwner) (*DB, error) {
	path := m.userPath(owner.id)
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, ErrUserStoreIdentity
		}
		if err := checkUserStoreIdentity(path, owner.id); err != nil {
			return nil, err
		}
		db, err := New(path)
		if err != nil {
			return nil, err
		}
		if err := db.requireCurrentSchema(); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Initialize privately, close/checkpoint every connection, then publish
	// without replacing an existing file. A failed creation cannot be mistaken
	// for a complete user database on the next acquisition.
	file, err := os.CreateTemp(m.directory, ".initializing-*.db")
	if err != nil {
		return nil, err
	}
	temporary := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return nil, err
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(temporary + suffix)
		}
	}()
	db, err := New(temporary)
	if err != nil {
		return nil, err
	}
	err = initializeUserStore(ctx, db, owner)
	err = errors.Join(err, db.Close())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Link(temporary, path); err != nil {
		return nil, fmt.Errorf("publish user database: %w", err)
	}
	return New(path)
}

func checkUserStoreIdentity(path, owner string) error {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	var actual string
	if err := db.QueryRow(`SELECT layout_version, user_id FROM gofer_user_store WHERE singleton = 1`).Scan(&version, &actual); err != nil {
		return fmt.Errorf("%w: %v", ErrUserStoreIdentity, err)
	}
	if version != 1 || actual != owner {
		return ErrUserStoreIdentity
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, owner).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrUserStoreIdentity
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id != ?`, owner).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrUserStoreIdentity
	}
	return nil
}
