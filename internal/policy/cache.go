package policy

import (
	"container/list"
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/directory"
)

// DefaultCacheSize is how many people's settings a Cache keeps.
const DefaultCacheSize = 1000

// revisions are the counters a resolution depends on: any change to
// policies, assignments, the settings that live elsewhere, or who is in
// which group moves one of them on.
type revisions struct {
	policy     int64
	membership int64
}

// Cache keeps people's resolved settings until the policies or group
// membership change, and up to a number of people, the least recently
// asked for going first. It's safe to use from many goroutines.
type Cache struct {
	q    Querier
	size int

	mu      sync.Mutex
	order   *list.List               // of *cacheEntry, most recent first
	entries map[string]*list.Element // by user ID; "" is the instance
	// logged is the revision whose skipped values were last logged, so a
	// value that doesn't check is reported once per change, not per read.
	logged revisions
	// Logf reports skipped values; log.Printf unless replaced.
	Logf func(format string, args ...any)
}

type cacheEntry struct {
	userID    string
	revisions revisions
	effective *Effective
}

// NewCache returns a cache reading through q, keeping up to size people.
func NewCache(q Querier, size int) *Cache {
	if size < 1 {
		size = DefaultCacheSize
	}
	return &Cache{q: q, size: size, order: list.New(), entries: map[string]*list.Element{}, logged: revisions{-1, -1}, Logf: log.Printf}
}

// ForUser returns the person's settings, worked out again when anything
// they depend on changed.
func (cache *Cache) ForUser(ctx context.Context, userID string) (*Effective, error) {
	return cache.get(ctx, userID, func() (*Effective, error) { return ForUser(ctx, cache.q, userID) })
}

// ForInstance returns the whole server's settings.
func (cache *Cache) ForInstance(ctx context.Context) (*Effective, error) {
	return cache.get(ctx, "", func() (*Effective, error) { return ForInstance(ctx, cache.q) })
}

func (cache *Cache) get(ctx context.Context, userID string, resolveNow func() (*Effective, error)) (*Effective, error) {
	current, err := readRevisions(ctx, cache.q)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if element, ok := cache.entries[userID]; ok {
		entry := element.Value.(*cacheEntry)
		if entry.revisions == current {
			cache.order.MoveToFront(element)
			cache.mu.Unlock()
			return entry.effective, nil
		}
	}
	cache.mu.Unlock()

	effective, err := resolveNow()
	if err != nil {
		return nil, err
	}
	// The revisions may have moved on while resolving; the entry keeps
	// the ones read before, so the next read resolves again.
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(effective.Ignored) > 0 && cache.logged != current {
		cache.logged = current
		for _, ignored := range effective.Ignored {
			cache.Logf("policy: skipping %s on policy %s: %v", ignored.Key, ignored.PolicyID, ignored.Err)
		}
	}
	if element, ok := cache.entries[userID]; ok {
		element.Value = &cacheEntry{userID: userID, revisions: current, effective: effective}
		cache.order.MoveToFront(element)
		return effective, nil
	}
	cache.entries[userID] = cache.order.PushFront(&cacheEntry{userID: userID, revisions: current, effective: effective})
	for cache.order.Len() > cache.size {
		oldest := cache.order.Back()
		cache.order.Remove(oldest)
		delete(cache.entries, oldest.Value.(*cacheEntry).userID)
	}
	return effective, nil
}

// readRevisions reads both counters in one query, as every read through
// the cache does.
func readRevisions(ctx context.Context, q Querier) (revisions, error) {
	var current revisions
	rows, err := q.QueryContext(ctx, `SELECT name, revision FROM admin_revisions WHERE name IN (?, ?)`,
		directory.RevisionPolicy, directory.RevisionMembership)
	if err != nil {
		return current, fmt.Errorf("read the policy revisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var revision int64
		if err := rows.Scan(&name, &revision); err != nil {
			return current, fmt.Errorf("read a policy revision: %w", err)
		}
		switch name {
		case directory.RevisionPolicy:
			current.policy = revision
		case directory.RevisionMembership:
			current.membership = revision
		}
	}
	return current, rows.Err()
}

// CurrentRevisions reads the policy and membership revisions, which move
// on with every change to what a person's settings could be.
func CurrentRevisions(ctx context.Context, q Querier) (policyRevision, membershipRevision int64, err error) {
	current, err := readRevisions(ctx, q)
	return current.policy, current.membership, err
}
