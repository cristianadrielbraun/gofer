package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/google/uuid"
)

var ErrAccountRoute = errors.New("account is unavailable in the requested storage scope")

type AccountRouteState string

const (
	AccountCreating AccountRouteState = "creating"
	AccountActive   AccountRouteState = "active"
	AccountDeleting AccountRouteState = "deleting"
	AccountDeleted  AccountRouteState = "deleted"
)

// AccountRoute is an ownership/lifecycle record, never a copy of mailbox
// configuration or credentials. Deleted IDs remain reserved permanently.
type AccountRoute struct {
	AccountID string
	UserID    string
	State     AccountRouteState
}

// AccountRouting is the sole routing coordinator for a UserStores manager.
// It is opt-in infrastructure, not a runtime layout switch. Callers authenticate
// and authorize users before WithUser/WithAccountForUser. WithAccount is for
// trusted workers; cleanup methods deliberately allow disabled/deleting owners.
// Callbacks must not retain databases, hold leases across provider calls, or
// invoke lifecycle methods for an account whose callback they are executing.
type AccountRouting struct {
	stores *UserStores
	mu     sync.Mutex
	scopes map[string]*accountRouteScope
}

type accountRouteScope struct {
	refs          int
	running       int
	transitioning bool
	changed       chan struct{}
}

// NewAccountRouting initializes the central ownership directory only when
// explicitly requested. Repeated calls on the same manager share the coordinator
// so deletion drains all callbacks. Main does not call this constructor yet.
func NewAccountRouting(stores *UserStores) (*AccountRouting, error) {
	if stores == nil {
		return nil, errors.New("user database manager is required")
	}
	stores.mu.Lock()
	defer stores.mu.Unlock()
	if stores.closing {
		return nil, ErrUserStoresClosed
	}
	if stores.routing != nil {
		return stores.routing, nil
	}
	tx, err := stores.system.Write().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var isUserStore int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name = 'gofer_user_store'`).Scan(&isUserStore); err != nil {
		return nil, err
	}
	if isUserStore != 0 {
		return nil, ErrUserStoreIdentity
	}
	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS gofer_account_directory (
			account_id TEXT PRIMARY KEY CHECK (account_id != ''),
			user_id TEXT NOT NULL CHECK (user_id != ''),
			state TEXT NOT NULL CHECK (state IN ('creating', 'active', 'deleting', 'deleted')),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS gofer_account_directory_state ON gofer_account_directory(state, account_id);
		CREATE INDEX IF NOT EXISTS gofer_account_directory_owner ON gofer_account_directory(user_id, state, account_id);
		CREATE TABLE IF NOT EXISTS gofer_avatar_interests (
			email_hash TEXT NOT NULL REFERENCES sender_avatars(email_hash) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			PRIMARY KEY(email_hash,user_id)
		);
		CREATE TABLE IF NOT EXISTS gofer_account_poll_schedule (
			account_id TEXT PRIMARY KEY REFERENCES gofer_account_directory(account_id),
			next_due_ms INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS gofer_account_poll_due ON gofer_account_poll_schedule(next_due_ms, account_id);
		CREATE TABLE IF NOT EXISTS gofer_account_provider_retry (
			account_id TEXT PRIMARY KEY REFERENCES gofer_account_directory(account_id),
			retry_until_ms INTEGER NOT NULL
		);

        CREATE TABLE IF NOT EXISTS gofer_account_active_poll (
            account_id TEXT PRIMARY KEY REFERENCES gofer_account_directory(account_id),
            next_due_ms INTEGER NOT NULL DEFAULT 0,
            last_attempt_ns INTEGER NOT NULL DEFAULT 0,
            failures INTEGER NOT NULL DEFAULT 0 CHECK(failures BETWEEN 0 AND 4),
            revision INTEGER NOT NULL DEFAULT 0
        );
		CREATE INDEX IF NOT EXISTS gofer_account_active_poll_due ON gofer_account_active_poll(next_due_ms,account_id);
		CREATE TABLE IF NOT EXISTS gofer_account_service_schedule (
			account_id TEXT NOT NULL REFERENCES gofer_account_directory(account_id),
			service TEXT NOT NULL CHECK(service IN ('contacts','calendar')),
			next_due_ms INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(account_id,service)
		);
		CREATE INDEX IF NOT EXISTS gofer_account_service_due ON gofer_account_service_schedule(service,next_due_ms,account_id);
		CREATE TABLE IF NOT EXISTS gofer_contact_queue_schedule (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			next_due_ms INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS gofer_contact_queue_due ON gofer_contact_queue_schedule(next_due_ms,user_id);
		CREATE TABLE IF NOT EXISTS gofer_user_cleanup_receipts (
			user_id TEXT PRIMARY KEY CHECK(user_id<>''),
			completed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TRIGGER IF NOT EXISTS gofer_account_directory_identity BEFORE UPDATE OF account_id, user_id ON gofer_account_directory
		WHEN NEW.account_id IS NOT OLD.account_id OR NEW.user_id IS NOT OLD.user_id
		BEGIN SELECT RAISE(ABORT, 'account ownership is immutable'); END;
	`)
	if err != nil {
		return nil, fmt.Errorf("initialize account ownership directory: %w", err)
	}
	// Tombstone owner IDs deliberately outlive centrally deleted user rows.
	// Future user deletion must finish every account before deleting its owner.
	// Older opt-in layouts may predate queue hints. Seed existing mailbox owners
	// centrally, without opening every store or rewriting established deadlines.
	if _, err := tx.Exec(`INSERT INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision)
 SELECT DISTINCT d.user_id,0,0 FROM gofer_account_directory d JOIN users u ON u.id=d.user_id
 WHERE d.state='active' AND u.user_type='webmail' AND u.is_admin=0
 ON CONFLICT(user_id) DO NOTHING`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r := &AccountRouting{stores: stores, scopes: make(map[string]*accountRouteScope)}
	stores.routing = r
	return r, nil
}

// System is for central authentication and instance policy, not local mail data.
func (r *AccountRouting) System() *DB { return r.stores.system }

// ValidateUser checks current owner lifecycle without opening a user database.
func (r *AccountRouting) ValidateUser(ctx context.Context, userID string) error {
	return r.requireActiveOwner(ctx, userID)
}

// AccountStateForUser includes durable deletion tombstones without consulting
// a potentially removed user file. Unknown/foreign IDs do not disclose state.
func (r *AccountRouting) AccountStateForUser(ctx context.Context, userID, accountID string) (AccountRouteState, error) {
	if userID == "" || accountID == "" {
		return "", ErrAccountRoute
	}
	var state AccountRouteState
	err := r.System().Read().QueryRowContext(ctx, `SELECT state FROM gofer_account_directory WHERE account_id = ? AND user_id = ?`, accountID, userID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAccountRoute
	}
	return state, err
}

func (r *AccountRouting) requireActiveOwner(ctx context.Context, userID string) error {
	var present int
	err := r.System().Read().QueryRowContext(ctx, `SELECT 1 FROM users
		WHERE id = ? AND user_type = 'webmail' AND is_admin = 0
		AND status = 'active' AND deletion_pending = 0`, userID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserStoreOwner
	}
	return err
}

func (r *AccountRouting) WithUser(ctx context.Context, userID string, fn func(*DB) error) error {
	return r.withUser(ctx, userID, false, fn)
}

// WithExistingUser authorizes local maintenance without creating an unused
// owner's store. Missing files remain errors, including for owners with accounts.
func (r *AccountRouting) WithExistingUser(ctx context.Context, userID string, fn func(*DB) error) error {
	if err := r.requireActiveOwner(ctx, userID); err != nil {
		return err
	}
	// An hourly sweep may encounter many registered owners with no store yet.
	// Do not evict a useful cache entry just to attempt opening a missing file.
	if _, err := os.Lstat(r.stores.userPath(userID)); err != nil {
		return fmt.Errorf("%w: %w", ErrAccountRoute, err)
	}
	return r.withUser(ctx, userID, true, fn)
}

func (r *AccountRouting) withUser(ctx context.Context, userID string, existing bool, fn func(*DB) error) error {
	if err := r.requireActiveOwner(ctx, userID); err != nil {
		return err
	}
	return r.withUserStore(ctx, userID, existing, func(db *DB) error {
		// Acquiring a store may wait for cache capacity. Recheck lifecycle after it.
		if err := r.requireActiveOwner(ctx, userID); err != nil {
			return err
		}
		return fn(db)
	})
}

// withUserStore selects the storage lease without changing the caller's
// authorization policy. Deletion maintenance may run for disabled owners;
// request routing separately requires an active owner through withUser.
func (r *AccountRouting) withUserStore(ctx context.Context, userID string, existing bool, fn func(*DB) error) error {
	if !existing {
		// A durable historical account means this owner already had storage.
		// Deleting/deleted accounts must not make a lost store look unused.
		if err := r.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id = ? AND state IN ('active','deleting','deleted'))`, userID).Scan(&existing); err != nil {
			return err
		}
	}
	acquire := r.stores.Acquire
	if existing {
		acquire = r.stores.AcquireExisting
	}
	lease, err := acquire(ctx, userID)
	if err != nil {
		if existing && errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrAccountRoute, err)
		}
		return err
	}
	defer lease.Release()
	return fn(lease.DB())
}

func (r *AccountRouting) route(ctx context.Context, accountID, userID string, state AccountRouteState) (AccountRoute, error) {
	var entry AccountRoute
	err := r.System().Read().QueryRowContext(ctx, `SELECT account_id, user_id, state
		FROM gofer_account_directory WHERE account_id = ?`, accountID).Scan(&entry.AccountID, &entry.UserID, &entry.State)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (entry.State != state || (userID != "" && entry.UserID != userID))) {
		return AccountRoute{}, ErrAccountRoute
	}
	return entry, err
}

func (r *AccountRouting) WithAccountForUser(ctx context.Context, userID, accountID string, fn func(*DB) error) error {
	if userID == "" {
		return ErrAccountRoute
	}
	return r.withAccount(ctx, userID, accountID, func(db *DB, _ string) error { return fn(db) })
}

func (r *AccountRouting) WithAccount(ctx context.Context, accountID string, fn func(*DB, string) error) error {
	return r.withAccount(ctx, "", accountID, fn)
}

// WithAccountActivityForUser protects an already persisted account's external
// work from deletion cleanup without opening or leasing its database. It shares
// the account callback drain. The callback must not delete its own account.
func (r *AccountRouting) WithAccountActivityForUser(ctx context.Context, userID, accountID string, fn func() error) error {
	if userID == "" {
		return ErrAccountRoute
	}
	entry, release, err := r.startAccountActivity(ctx, userID, accountID)
	if err != nil {
		return err
	}
	defer release()
	if err := r.requireActiveOwner(ctx, entry.UserID); err != nil {
		return err
	}
	return fn()
}

func (r *AccountRouting) startAccountActivity(ctx context.Context, userID, accountID string) (AccountRoute, func(), error) {
	r.mu.Lock()
	entry, err := r.route(ctx, accountID, userID, AccountActive)
	if err != nil {
		r.mu.Unlock()
		return AccountRoute{}, nil, err
	}
	scope := r.scopeLocked(accountID)
	if scope.transitioning {
		r.mu.Unlock()
		return AccountRoute{}, nil, ErrAccountRoute
	}
	scope.refs++
	scope.running++
	r.mu.Unlock()
	return entry, func() {
		r.mu.Lock()
		scope.running--
		r.releaseScopeLocked(accountID, scope)
		r.mu.Unlock()
	}, nil
}

func (r *AccountRouting) withAccount(ctx context.Context, userID, accountID string, fn func(*DB, string) error) error {
	entry, release, err := r.startAccountActivity(ctx, userID, accountID)
	if err != nil {
		return err
	}
	defer release()
	return r.withUser(ctx, entry.UserID, true, func(db *DB) error {
		if _, err := r.route(ctx, accountID, entry.UserID, AccountActive); err != nil {
			return err
		}
		var found int
		if err := db.Read().QueryRowContext(ctx, `SELECT 1 FROM accounts
			WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, entry.UserID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAccountRoute
			}
			return err
		}
		return fn(db, entry.UserID)
	})
}

// ReserveAccount commits a recoverable intent before any mailbox rows are
// written. New IDs are opaque UUIDs, globally unique even for the same email.
func (r *AccountRouting) ReserveAccount(ctx context.Context, userID string) (AccountRoute, error) {
	entry := AccountRoute{AccountID: uuid.NewString(), UserID: userID, State: AccountCreating}
	result, err := r.System().Write().ExecContext(ctx, `INSERT INTO gofer_account_directory(account_id, user_id, state)
		SELECT ?, id, 'creating' FROM users WHERE id = ? AND user_type = 'webmail' AND is_admin = 0
		AND status = 'active' AND deletion_pending = 0`, entry.AccountID, userID)
	if err != nil {
		return AccountRoute{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return AccountRoute{}, err
		}
		return AccountRoute{}, ErrUserStoreOwner
	}
	return entry, nil
}

// CompleteAccountCreation serializes local creation with recovery/cancellation.
// If a previous attempt already committed its account row, the callback is
// skipped. Failures leave the central intent pending and never delete local data.
func (r *AccountRouting) CompleteAccountCreation(ctx context.Context, userID, accountID string, create func(*DB) error) error {
	if userID == "" {
		return ErrAccountRoute
	}
	return r.transition(ctx, accountID, func(_ *accountRouteScope) error {
		if _, err := r.route(ctx, accountID, userID, AccountActive); err == nil {
			return r.withUser(ctx, userID, true, func(db *DB) error {
				var present int
				if err := db.Read().QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID).Scan(&present); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return ErrAccountRoute
					}
					return err
				}
				return nil
			})
		}
		if _, err := r.route(ctx, accountID, userID, AccountCreating); err != nil {
			return err
		}
		return r.withUser(ctx, userID, create == nil, func(db *DB) error {
			var deleting int
			err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(is_deleting, 0) FROM accounts WHERE id = ? AND user_id = ?`, accountID, userID).Scan(&deleting)
			if errors.Is(err, sql.ErrNoRows) && create != nil {
				if err := create(db); err != nil {
					return err
				}
				err = db.Read().QueryRowContext(ctx, `SELECT COALESCE(is_deleting, 0) FROM accounts WHERE id = ? AND user_id = ?`, accountID, userID).Scan(&deleting)
			}
			if errors.Is(err, sql.ErrNoRows) || (err == nil && deleting != 0) {
				return ErrAccountRoute
			}
			if err != nil {
				return err
			}
			if err := r.requireActiveOwner(ctx, userID); err != nil {
				return err
			}
			tx, err := r.System().Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			// A successful account activation guarantees a durable owner queue
			// hint before any producer can accept local contact work. If its first
			// postcommit wake fails, removing the last account cannot hide that work.
			if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision)
 SELECT id,0,0 FROM users WHERE id=? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0
 ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE gofer_account_directory
 SET state='active',updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND state='creating'
 AND EXISTS(SELECT 1 FROM users u WHERE u.id=? AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)`, accountID, userID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrUserStoreOwner
			}
			return tx.Commit()
		})
	})
}

// CancelAccountCreation can discard an empty reservation only. A committed
// local row must be recovered or deleted explicitly, never silently discarded.
func (r *AccountRouting) CancelAccountCreation(ctx context.Context, userID, accountID string) error {
	if userID == "" {
		return ErrAccountRoute
	}
	return r.transition(ctx, accountID, func(_ *accountRouteScope) error {
		if _, err := r.route(ctx, accountID, userID, AccountCreating); err != nil {
			return err
		}
		return r.withUserStore(ctx, userID, false, func(db *DB) error {
			var count int
			if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = ?`, accountID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrAccountRoute
			}
			_, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_directory SET state = 'deleted', updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND state = 'creating'`, accountID)
			return err
		})
	})
}

// RequestAccountDeletion commits an authenticated owner's deletion intent
// without waiting for running callbacks. BeginAccountDeletion must drain them
// before any external or local cleanup occurs.
func (r *AccountRouting) RequestAccountDeletion(ctx context.Context, userID, accountID string) error {
	if err := r.requireActiveOwner(ctx, userID); err != nil {
		return err
	}
	return r.transition(ctx, accountID, func(*accountRouteScope) error {
		result, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_directory
			SET state = CASE WHEN state = 'deleted' THEN 'deleted' ELSE 'deleting' END,
			updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND user_id = ?`, accountID, userID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrAccountRoute
		}
		return nil
	})
}

// BeginAccountDeletion blocks new routes before waiting for current callbacks.
// A timeout leaves 'deleting' durable; retrying resumes the drain. User-wide
// WithUser callbacks are not account-scoped and must be stopped separately by
// the future application lifecycle integration.
func (r *AccountRouting) BeginAccountDeletion(ctx context.Context, userID, accountID string) error {
	return r.transition(ctx, accountID, func(scope *accountRouteScope) error {
		var owner string
		var state AccountRouteState
		if err := r.System().Read().QueryRowContext(ctx, `SELECT user_id, state FROM gofer_account_directory WHERE account_id = ?`, accountID).Scan(&owner, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAccountRoute
			}
			return err
		}
		if userID == "" || owner != userID {
			return ErrAccountRoute
		}
		if state == AccountDeleted {
			return nil
		}
		if _, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_directory SET state = 'deleting', updated_at = CURRENT_TIMESTAMP WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		r.mu.Lock()
		for scope.running != 0 {
			changed := scope.changed
			r.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
			r.mu.Lock()
		}
		r.mu.Unlock()
		// Hide the mailbox from owner-wide local listings before external cleanup.
		// This runs after account callbacks drain and holds no lease over a provider
		// call. A failed local mark leaves the durable central deletion retryable.
		return r.withUserStore(ctx, userID, true, func(db *DB) error {
			_, err := db.Write().ExecContext(ctx, `UPDATE accounts SET is_deleting = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, accountID, userID)
			return err
		})
	})
}

// CompleteAccountDeletion serializes cleanup and only tombstones the directory
// after the local account is absent. The caller must finish external cleanup
// before invoking it; the callback performs local database work only.
func (r *AccountRouting) CompleteAccountDeletion(ctx context.Context, userID, accountID string, cleanup func(*DB) error) error {
	if userID == "" {
		return ErrAccountRoute
	}
	return r.transition(ctx, accountID, func(scope *accountRouteScope) error {
		if _, err := r.route(ctx, accountID, userID, AccountDeleted); err == nil {
			return nil
		}
		if _, err := r.route(ctx, accountID, userID, AccountDeleting); err != nil {
			return err
		}
		r.mu.Lock()
		busy := scope.running != 0
		r.mu.Unlock()
		if busy {
			return ErrAccountRoute
		}
		return r.withUserStore(ctx, userID, true, func(db *DB) error {
			if cleanup != nil {
				if err := cleanup(db); err != nil {
					return err
				}
			}
			var count int
			if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = ?`, accountID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrAccountRoute
			}
			_, err := r.System().Write().ExecContext(ctx, `UPDATE gofer_account_directory SET state = 'deleted', updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND state = 'deleting'`, accountID)
			return err
		})
	})
}

// ListAccounts pages directory metadata without opening any user databases.
// An empty owner is an explicit instance-wide worker/operator query. Active
// entries exclude inactive owners; pending cleanup includes disabled owners.
func (r *AccountRouting) ListAccounts(ctx context.Context, userID string, state AccountRouteState, after string, limit int) ([]AccountRoute, error) {
	if state != AccountCreating && state != AccountActive && state != AccountDeleting && state != AccountDeleted {
		return nil, ErrAccountRoute
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("account page size must be between 1 and 1000")
	}
	rows, err := r.System().Read().QueryContext(ctx, `SELECT d.account_id, d.user_id, d.state
		FROM gofer_account_directory d LEFT JOIN users u ON u.id = d.user_id
		WHERE d.state = ? AND d.account_id > ? AND (? = '' OR d.user_id = ?)
		AND (? != 'active' OR (u.status = 'active' AND u.deletion_pending = 0 AND u.user_type = 'webmail' AND u.is_admin = 0))
		ORDER BY d.account_id LIMIT ?`, state, after, userID, userID, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AccountRoute
	for rows.Next() {
		var entry AccountRoute
		if err := rows.Scan(&entry.AccountID, &entry.UserID, &entry.State); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (r *AccountRouting) scopeLocked(id string) *accountRouteScope {
	scope := r.scopes[id]
	if scope == nil {
		scope = &accountRouteScope{changed: make(chan struct{})}
		r.scopes[id] = scope
	}
	return scope
}

func (r *AccountRouting) releaseScopeLocked(id string, scope *accountRouteScope) {
	scope.refs--
	close(scope.changed)
	scope.changed = make(chan struct{})
	if scope.refs == 0 {
		delete(r.scopes, id)
	}
}

func (r *AccountRouting) transition(ctx context.Context, id string, fn func(*accountRouteScope) error) error {
	r.mu.Lock()
	scope := r.scopeLocked(id)
	scope.refs++
	for scope.transitioning {
		changed := scope.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			r.mu.Lock()
			r.releaseScopeLocked(id, scope)
			r.mu.Unlock()
			return ctx.Err()
		}
		r.mu.Lock()
	}
	scope.transitioning = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		scope.transitioning = false
		r.releaseScopeLocked(id, scope)
		r.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(scope)
}
