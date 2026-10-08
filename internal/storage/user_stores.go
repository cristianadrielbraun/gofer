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
	system          *DB
	directory       string
	maxOpen         int
	idleTimeout     time.Duration
	lock            *runtimeguard.Lock
	mu              sync.Mutex
	entries         map[string]*userStoreEntry
	changed         chan struct{}
	wake            chan struct{}
	closeWake       chan struct{}
	closerDone      chan struct{}
	done            chan struct{}
	closeStore      func(*DB) error
	openStore       func(context.Context, userStoreOwner, bool) (*DB, error)
	removeStoreFile func(string) error
	closing         bool
	routing         *AccountRouting
	closeErr        error
	now             func() time.Time
}

type userStoreEntry struct {
	db        *DB
	opening   bool
	closing   bool
	refs      int
	idleSince time.Time
	closeErr  error
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
	if err := ensureUserStoreDirectory(system); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	m := &UserStores{
		system: system, directory: directory, maxOpen: options.MaxOpen,
		idleTimeout: options.IdleTimeout, lock: lock,
		entries: make(map[string]*userStoreEntry), changed: make(chan struct{}),
		wake: make(chan struct{}, 1), closeWake: make(chan struct{}, 1),
		closerDone: make(chan struct{}), done: make(chan struct{}),
		closeStore: (*DB).Close, now: time.Now,
	}
	go m.closePending()
	go m.reap()
	return m, nil
}

func (m *UserStores) Acquire(ctx context.Context, userID string) (*UserStoreLease, error) {
	return m.acquire(ctx, userID, true)
}

// AcquireExisting never substitutes a new empty file for a missing user store.
// Account routes use it once local creation has been committed.
func (m *UserStores) AcquireExisting(ctx context.Context, userID string) (*UserStoreLease, error) {
	return m.acquire(ctx, userID, false)
}

func (m *UserStores) acquire(ctx context.Context, userID string, create bool) (*UserStoreLease, error) {
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
		if entry, ok := m.entries[userID]; ok && !entry.opening && !entry.closing {
			entry.refs++
			m.mu.Unlock()
			lease := &UserStoreLease{manager: m, owner: userID, entry: entry}
			// Removal may commit after the first lookup but before this lease.
			// Register it first so a remover must wait for this recheck/release.
			if _, err := m.lookupOwner(ctx, userID); err != nil {
				lease.Release()
				return nil, err
			}
			return lease, nil
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
			// Scheduling eviction does not free capacity: its physical close
			// may still be checkpointing. Wait without holding the cache lock.
			m.evictOldestLocked()
			changed := m.changed
			m.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		entry := &userStoreEntry{opening: true, refs: 1}
		m.entries[userID] = entry
		openStore := m.openStore
		if openStore == nil {
			openStore = m.openUserStore
		}
		m.mu.Unlock()

		// The opening entry fences a concurrent removal before any file is
		// created. Recheck after registering it, including after cache waits.
		_, err = m.lookupOwner(ctx, userID)
		var db *DB
		if err == nil {
			db, err = openStore(ctx, owner, create)
		}
		if err == nil {
			err = m.recordOpenedStore(ctx, userID)
		}
		m.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && m.closing {
			err = ErrUserStoresClosed
		}
		if err != nil {
			if db != nil {
				// A canceled successful open still owns a physical handle.
				// Keep its slot and owner reserved until the closer finishes.
				entry.db, entry.opening, entry.refs = db, false, 0
				m.closeEntryLocked(userID, entry)
			} else {
				delete(m.entries, userID)
				m.signalLocked()
			}
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
		select {
		case <-m.closerDone:
			return m.closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
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

// Called with mu held. Closing entries retain their cache slot and cannot be
// leased or reopened until their physical SQLite close has completed.
func (m *UserStores) closeEntryLocked(_ string, entry *userStoreEntry) {
	if entry.opening || entry.closing || entry.refs != 0 || entry.db == nil {
		return
	}
	entry.closing = true
	m.signalLocked()
	select {
	case m.closeWake <- struct{}{}:
	default:
	}
}

// One closer bounds checkpoint work and goroutines. SQLite fsync never holds
// the cache mutex; already cached other owners remain independently usable.
func (m *UserStores) closePending() {
	defer close(m.closerDone)
	for {
		select {
		case <-m.done:
			return
		case <-m.closeWake:
		}
		for {
			m.mu.Lock()
			var owner string
			var entry *userStoreEntry
			for id, candidate := range m.entries {
				if candidate.closing {
					owner, entry = id, candidate
					break
				}
			}
			if entry == nil {
				m.mu.Unlock()
				break
			}
			closeStore := m.closeStore
			m.mu.Unlock()

			err := closeStore(entry.db)
			m.mu.Lock()
			entry.closeErr = err
			m.closeErr = errors.Join(m.closeErr, err)
			delete(m.entries, owner)
			m.signalLocked()
			m.mu.Unlock()
			m.wakeReaper()
		}
	}
}

func (m *UserStores) evictOldestLocked() bool {
	// A scheduled close will free capacity. Waiting requests must not keep
	// evicting other idle owners while that checkpoint is still running.
	for _, entry := range m.entries {
		if entry.closing {
			return false
		}
	}
	var oldest *userStoreEntry
	var owner string
	for id, entry := range m.entries {
		if entry.opening || entry.closing || entry.refs != 0 {
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
		if entry.opening || entry.closing || entry.refs != 0 {
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
		FROM users WHERE id = ? AND user_type = 'webmail' AND is_admin = 0
 AND NOT EXISTS(SELECT 1 FROM gofer_user_store_directory d WHERE d.user_id=users.id AND d.state<>'present')`, userID).
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

func (m *UserStores) openUserStore(ctx context.Context, owner userStoreOwner, create bool) (*DB, error) {
	path := m.userPath(owner.id)
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, ErrUserStoreIdentity
		}
		if err := checkUserStoreIdentity(path, owner.id); err != nil {
			return nil, err
		}
		db, err := OpenExisting(path)
		if err != nil {
			return nil, err
		}
		db.senderAvatarCache = m.system
		if err := db.requireCurrentSchema(); err != nil {
			db.Close()
			return nil, err
		}
		if err := ensureUserOAuthBoundary(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
		if err := ensureUserMailDeliverySchema(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !create {
		return nil, fmt.Errorf("existing user database is missing: %w", err)
	}
	var known bool
	if queryErr := m.system.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_user_store_directory WHERE user_id=?)`, owner.id).Scan(&known); queryErr != nil {
		return nil, queryErr
	}
	if known {
		return nil, errors.Join(ErrAccountRoute, err)
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
	if err == nil {
		err = ensureUserMailDeliverySchema(ctx, db)
	}
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
	opened, err := New(path)
	if err == nil {
		opened.userMailDelivery = true
		opened.senderAvatarCache = m.system
	}
	return opened, err
}

func checkUserStoreIdentity(path, owner string) error {
	return checkUserStoreFile(path, owner, true)
}

func checkUserStoreFile(path, owner string, currentSchema bool) error {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	// Reject incompatible schemas before any writer changes journal settings or
	// runs migrations. Managed startup upgrades owned files before activation.
	if currentSchema {
		if err := (&DB{read: db}).requireCurrentSchema(); err != nil {
			return err
		}
	}
	return checkUserStoreOwner(db, owner)
}

// checkUserStoreOwner checks the owned identity at any schema version.
func checkUserStoreOwner(db *sql.DB, owner string) error {
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
