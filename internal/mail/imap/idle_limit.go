package imap

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Every folder watched in real time holds its own connection to the mail
// server, and servers limit how many one mailbox may hold: Yahoo has been
// seen refusing every login, for hours, once a mailbox held a handful. No
// server says what its limit is, so Gofer learns it for each mailbox. It
// starts low and watches one more folder after a day of holding all it may
// without a refusal; a refusal brings it back to what the server held, and
// it doesn't try more again for days. Folders beyond the limit are polled
// instead, Inbox last.
const (
	// idleMailboxStart is how many folders a mailbox starts with.
	idleMailboxStart = 3
	// idleMailboxCeiling is the most Gofer learns its way up to.
	idleMailboxCeiling = 15
	// idleClimbAfter is how long a mailbox holds all its watches without a
	// refusal before it may try one more.
	idleClimbAfter = 24 * time.Hour
	// idleClimbHold is how long after a refusal it may not try more.
	idleClimbHold = 72 * time.Hour
	// idleRefusalBurst is how soon after a refusal another counts as the
	// same one.
	idleRefusalBurst = time.Minute
	// idleWaitRecheck is how often a folder waiting for room looks again,
	// so a mailbox can climb while every watch is quiet.
	idleWaitRecheck = 30 * time.Minute
)

// IdleLimitStore keeps what was learned about a mailbox's limit.
type IdleLimitStore interface {
	LoadIdleLimit(ctx context.Context) (models.IdleConnectionLimit, bool, error)
	SaveIdleLimit(ctx context.Context, limit models.IdleConnectionLimit) error
}

// errIdleSlotLost is returned by a watch that gave its connection to a
// folder that comes first.
var errIdleSlotLost = errors.New("another folder of this mailbox comes first for real-time watching")

// idleMailboxBounds returns where a mailbox starts and how far it may climb:
// GOFER_IDLE_MAX_PER_MAILBOX sets both, for administrators who know their
// server.
func idleMailboxBounds() (start, ceiling int) {
	idleBoundsOnce.Do(func() {
		idleBoundsStart, idleBoundsCeiling = idleMailboxStart, idleMailboxCeiling
		raw := strings.TrimSpace(os.Getenv("GOFER_IDLE_MAX_PER_MAILBOX"))
		if raw == "" {
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			log.Printf("GOFER_IDLE_MAX_PER_MAILBOX must be a positive integer; using the default")
			return
		}
		idleBoundsStart, idleBoundsCeiling = n, n
	})
	return idleBoundsStart, idleBoundsCeiling
}

// IdleMailboxStart is how many folders a mailbox may watch in real time
// before Gofer learned anything about its server.
func IdleMailboxStart() int {
	start, _ := idleMailboxBounds()
	return start
}

var (
	idleBoundsOnce                     sync.Once
	idleBoundsStart, idleBoundsCeiling int
)

type idleLimits struct {
	mu        sync.Mutex
	now       func() time.Time
	bounds    func() (start, ceiling int)
	mailboxes map[string]*idleMailbox
	seq       uint64
}

var defaultIdleLimits = newIdleLimits(time.Now, idleMailboxBounds)

func newIdleLimits(now func() time.Time, bounds func() (int, int)) *idleLimits {
	return &idleLimits{now: now, bounds: bounds, mailboxes: make(map[string]*idleMailbox)}
}

type idleMailbox struct {
	state   models.IdleConnectionLimit
	members []*idleMember
	// fullSince is when the mailbox began holding all the watches it may.
	fullSince time.Time
}

type idleMember struct {
	key      string
	rank     int
	seq      uint64
	store    IdleLimitStore
	admitted bool
	// active is set while the watch connects or holds its connection,
	// connected once it holds it.
	active    bool
	connected bool
	// changed is closed and replaced whenever admitted changes.
	changed chan struct{}
}

// join adds a folder's watch to its mailbox. learned is what the store had,
// used when the mailbox has no watch yet.
func (l *idleLimits) join(key string, rank int, store IdleLimitStore, learned models.IdleConnectionLimit, found bool) *idleMember {
	l.mu.Lock()
	defer l.mu.Unlock()
	start, ceiling := l.bounds()
	mb := l.mailboxes[key]
	if mb == nil {
		mb = &idleMailbox{state: models.IdleConnectionLimit{Limit: start}}
		if found {
			mb.state = learned
		}
		mb.state.Limit = min(max(mb.state.Limit, 1), ceiling)
		l.mailboxes[key] = mb
	}
	l.seq++
	m := &idleMember{key: key, rank: rank, seq: l.seq, store: store, changed: make(chan struct{})}
	mb.members = append(mb.members, m)
	l.recomputeLocked(mb)
	return m
}

func (l *idleLimits) leave(m *idleMember) {
	l.mu.Lock()
	defer l.mu.Unlock()
	mb := l.mailboxes[m.key]
	if mb == nil {
		return
	}
	for i, other := range mb.members {
		if other == m {
			mb.members = append(mb.members[:i], mb.members[i+1:]...)
			break
		}
	}
	if len(mb.members) == 0 {
		delete(l.mailboxes, m.key)
		return
	}
	l.recomputeLocked(mb)
}

// admit reports whether the watch may hold a connection now, the mailbox's
// limit, and a channel closed when that may have changed.
func (l *idleLimits) admit(m *idleMember) (bool, int, <-chan struct{}) {
	l.mu.Lock()
	mb := l.mailboxes[m.key]
	if mb == nil {
		l.mu.Unlock()
		return true, 0, m.changed
	}
	save := l.climbLocked(mb)
	l.recomputeLocked(mb)
	admitted, limit, changed := m.admitted, mb.state.Limit, m.changed
	saves := l.savesLocked(mb, save)
	l.mu.Unlock()
	saves()
	return admitted, limit, changed
}

// activate notes that the watch is opening its connection, or let it go.
func (l *idleLimits) activate(m *idleMember, active bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m.active = active
}

// connected notes that the watch holds its connection, or let it go.
func (l *idleLimits) connected(m *idleMember, connected bool) {
	l.mu.Lock()
	mb := l.mailboxes[m.key]
	if mb == nil || m.connected == connected {
		l.mu.Unlock()
		return
	}
	m.connected = connected
	save := l.recomputeLocked(mb)
	saves := l.savesLocked(mb, save)
	l.mu.Unlock()
	saves()
}

// refused notes that the server refused the watch's connection. When the
// mailbox was opening or holding others, it goes back to what it held, at
// least one fewer than before. Either way it won't try more for a while.
func (l *idleLimits) refused(m *idleMember) {
	l.mu.Lock()
	mb := l.mailboxes[m.key]
	if mb == nil {
		l.mu.Unlock()
		return
	}
	held := 0
	for _, other := range mb.members {
		if other != m && other.active {
			held++
		}
	}
	now := l.now()
	// Watches connecting together are refused together: one lowering is
	// enough for them.
	recent := !mb.state.FailedTime.IsZero() && now.Sub(mb.state.FailedTime) < idleRefusalBurst
	mb.state.FailedTime = now
	mb.fullSince = time.Time{}
	if held == 0 || recent {
		// Refused alone, something else holds the mailbox's connections and
		// fewer watches would not help; or this refusal is part of the last.
		l.recomputeLocked(mb)
		saves := l.savesLocked(mb, true)
		l.mu.Unlock()
		saves()
		return
	}
	previous := mb.state.Limit
	limit := max(min(previous-1, held), 1)
	mb.state.Limit = limit
	mb.state.FailedAt = held + 1
	if mb.state.StableAt > limit {
		mb.state.StableAt = limit
	}
	l.recomputeLocked(mb)
	saves := l.savesLocked(mb, true)
	l.mu.Unlock()
	log.Printf("idle: server refused connection %d to a mailbox; watching at most %d of its folders (was %d)", held+1, limit, previous)
	saves()
}

// climbLocked lets the mailbox watch one more folder once it has held all it
// may for a day, when a folder is waiting and no refusal came lately.
func (l *idleLimits) climbLocked(mb *idleMailbox) bool {
	_, ceiling := l.bounds()
	now := l.now()
	if mb.fullSince.IsZero() || len(mb.members) <= mb.state.Limit || mb.state.Limit >= ceiling {
		return false
	}
	if now.Sub(mb.fullSince) < idleClimbAfter {
		return false
	}
	if !mb.state.FailedTime.IsZero() && now.Sub(mb.state.FailedTime) < idleClimbHold {
		return false
	}
	mb.state.Limit++
	mb.fullSince = time.Time{}
	log.Printf("idle: a mailbox held its real-time watches for a day; trying %d", mb.state.Limit)
	return true
}

// recomputeLocked admits the watches that come first, up to the limit, and
// notes when the mailbox holds all it may. It reports whether what was
// learned changed.
func (l *idleLimits) recomputeLocked(mb *idleMailbox) bool {
	sort.SliceStable(mb.members, func(i, j int) bool {
		a, b := mb.members[i], mb.members[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.seq < b.seq
	})
	held := 0
	for i, m := range mb.members {
		admitted := i < mb.state.Limit
		if admitted != m.admitted {
			m.admitted = admitted
			close(m.changed)
			m.changed = make(chan struct{})
		}
		if m.connected {
			held++
		}
	}
	if held < mb.state.Limit {
		mb.fullSince = time.Time{}
		return false
	}
	changed := false
	if mb.fullSince.IsZero() {
		mb.fullSince = l.now()
		if mb.state.StableAt != mb.state.Limit {
			mb.state.StableAt = mb.state.Limit
			mb.state.StableSince = mb.fullSince
			changed = true
		}
	}
	return changed
}

// savesLocked returns what keeps the mailbox's state in each store its
// watches came with, to run once mu is released.
func (l *idleLimits) savesLocked(mb *idleMailbox, save bool) func() {
	if !save {
		return func() {}
	}
	state := mb.state
	var stores []IdleLimitStore
	seen := map[IdleLimitStore]bool{}
	for _, m := range mb.members {
		if m.store != nil && !seen[m.store] {
			seen[m.store] = true
			stores = append(stores, m.store)
		}
	}
	return func() {
		for _, store := range stores {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := store.SaveIdleLimit(ctx, state); err != nil {
				log.Printf("idle: save learned real-time limit: %v", err)
			}
			cancel()
		}
	}
}

// isConnectionRefusal reports whether the server turned a connection away
// for holding too many, rather than failing in some other way. A login Gofer
// held back itself during a cooldown is not one.
func isConnectionRefusal(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *imap.Error
	if errors.As(err, &statusErr) && (statusErr.Code == imap.ResponseCodeLimit || statusErr.Code == imap.ResponseCodeUnavailable) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, sign := range []string{"too many", "maximum number of connections", "connection limit"} {
		if strings.Contains(text, sign) {
			return true
		}
	}
	return false
}
