package imap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type memoryIdleLimitStore struct {
	mu    sync.Mutex
	limit models.IdleConnectionLimit
	found bool
	saves int
}

func (s *memoryIdleLimitStore) LoadIdleLimit(context.Context) (models.IdleConnectionLimit, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit, s.found, nil
}

func (s *memoryIdleLimitStore) SaveIdleLimit(_ context.Context, limit models.IdleConnectionLimit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit, s.found = limit, true
	s.saves++
	return nil
}

func (s *memoryIdleLimitStore) saved() models.IdleConnectionLimit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

type idleLimitClock struct{ now time.Time }

func newTestIdleLimits(start, ceiling int) (*idleLimits, *idleLimitClock) {
	clock := &idleLimitClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	return newIdleLimits(func() time.Time { return clock.now }, func() (int, int) { return start, ceiling }), clock
}

func admitted(t *testing.T, limits *idleLimits, members ...*idleMember) []bool {
	t.Helper()
	got := make([]bool, len(members))
	for i, m := range members {
		got[i], _, _ = limits.admit(m)
	}
	return got
}

func TestIdleLimitsAdmitFoldersThatComeFirst(t *testing.T) {
	limits, _ := newTestIdleLimits(3, 15)
	store := &memoryIdleLimitStore{}
	archive := limits.join("box", 4, store, models.IdleConnectionLimit{}, false)
	sent := limits.join("box", 2, store, models.IdleConnectionLimit{}, false)
	receipts := limits.join("box", 7, store, models.IdleConnectionLimit{}, false)
	spam := limits.join("box", 9, store, models.IdleConnectionLimit{}, false)
	// The Inbox joins last and still comes first; Receipts gives way to it.
	_, _, receiptsChanged := limits.admit(receipts)
	inbox := limits.join("box", 0, store, models.IdleConnectionLimit{}, false)
	select {
	case <-receiptsChanged:
	default:
		t.Fatal("Receipts was not told it lost its place")
	}
	got := admitted(t, limits, inbox, sent, archive, receipts, spam)
	want := []bool{true, true, true, false, false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admitted = %v, want %v", got, want)
	}
	other := limits.join("other mailbox", 9, store, models.IdleConnectionLimit{}, false)
	if ok, _, _ := limits.admit(other); !ok {
		t.Fatal("another mailbox shared this one's limit")
	}
	limits.leave(sent)
	if got := admitted(t, limits, receipts, spam); !got[0] || got[1] {
		t.Fatalf("after Sent left, admitted Receipts and Spam = %v, want Receipts only", got)
	}
}

func TestIdleLimitsLowerToWhatTheServerHeldOnRefusal(t *testing.T) {
	limits, clock := newTestIdleLimits(3, 15)
	store := &memoryIdleLimitStore{}
	members := []*idleMember{
		limits.join("box", 0, store, models.IdleConnectionLimit{}, false),
		limits.join("box", 1, store, models.IdleConnectionLimit{}, false),
		limits.join("box", 2, store, models.IdleConnectionLimit{}, false),
	}
	for _, m := range members[:2] {
		limits.activate(m, true)
		limits.connected(m, true)
	}
	limits.activate(members[2], true)

	limits.refused(members[2])
	saved := store.saved()
	if saved.Limit != 2 || saved.FailedAt != 3 || !saved.FailedTime.Equal(clock.now) {
		t.Fatalf("after a refusal of the third connection, saved %+v, want limit 2 failed at 3", saved)
	}
	if got := admitted(t, limits, members...); got[2] {
		t.Fatalf("admitted = %v, want the third folder polled", got)
	}

	// Watches refused together lower the limit once.
	clock.now = clock.now.Add(30 * time.Second)
	limits.refused(members[1])
	if saved := store.saved(); saved.Limit != 2 {
		t.Fatalf("a second refusal within a minute lowered the limit to %d", saved.Limit)
	}

	// Refused while holding nothing else: fewer watches wouldn't help.
	clock.now = clock.now.Add(time.Hour)
	for _, m := range members {
		limits.connected(m, false)
		limits.activate(m, false)
	}
	limits.refused(members[0])
	if saved := store.saved(); saved.Limit != 2 || !saved.FailedTime.Equal(clock.now) {
		t.Fatalf("a refusal of the only connection saved %+v, want limit 2 and the refusal time", saved)
	}
}

func TestIdleLimitsNeverGoBelowOne(t *testing.T) {
	limits, clock := newTestIdleLimits(2, 15)
	store := &memoryIdleLimitStore{}
	first := limits.join("box", 0, store, models.IdleConnectionLimit{}, false)
	second := limits.join("box", 1, store, models.IdleConnectionLimit{}, false)
	for range 3 {
		clock.now = clock.now.Add(time.Hour)
		limits.activate(first, true)
		limits.activate(second, true)
		limits.refused(second)
	}
	if saved := store.saved(); saved.Limit != 1 {
		t.Fatalf("limit = %d, want 1", saved.Limit)
	}
}

func TestIdleLimitsClimbAfterAQuietDayAndNotSoonAfterARefusal(t *testing.T) {
	limits, clock := newTestIdleLimits(3, 15)
	store := &memoryIdleLimitStore{}
	learned := models.IdleConnectionLimit{Limit: 2, FailedAt: 3, FailedTime: clock.now.Add(-time.Hour)}
	members := []*idleMember{
		limits.join("box", 0, store, learned, true),
		limits.join("box", 1, store, learned, true),
		limits.join("box", 2, store, learned, true),
	}
	if got := admitted(t, limits, members...); got[2] {
		t.Fatalf("admitted = %v, want the learned limit of 2", got)
	}
	for _, m := range members[:2] {
		limits.activate(m, true)
		limits.connected(m, true)
	}
	if saved := store.saved(); saved.StableAt != 2 || !saved.StableSince.Equal(clock.now) {
		t.Fatalf("holding 2 saved %+v, want stable at 2 since now", saved)
	}

	clock.now = clock.now.Add(25 * time.Hour)
	if ok, _, _ := limits.admit(members[2]); ok {
		t.Fatal("climbed a day after a refusal; want to wait three days")
	}
	clock.now = clock.now.Add(48 * time.Hour)
	ok, limit, _ := limits.admit(members[2])
	if !ok || limit != 3 || store.saved().Limit != 3 {
		t.Fatalf("after a quiet day and no refusal for three days: admitted %v at %d, saved %+v; want 3", ok, limit, store.saved())
	}
}

func TestIdleLimitsOnlyClimbWhenAFolderWaits(t *testing.T) {
	limits, clock := newTestIdleLimits(2, 15)
	store := &memoryIdleLimitStore{}
	members := []*idleMember{
		limits.join("box", 0, store, models.IdleConnectionLimit{}, false),
		limits.join("box", 1, store, models.IdleConnectionLimit{}, false),
	}
	for _, m := range members {
		limits.activate(m, true)
		limits.connected(m, true)
	}
	clock.now = clock.now.Add(100 * time.Hour)
	if _, limit, _ := limits.admit(members[0]); limit != 2 {
		t.Fatalf("limit = %d with no folder waiting, want 2", limit)
	}
}

func TestIdleLimitsKeepLearnedLimitUnderTheCeiling(t *testing.T) {
	limits, _ := newTestIdleLimits(3, 4)
	store := &memoryIdleLimitStore{}
	m := limits.join("box", 0, store, models.IdleConnectionLimit{Limit: 9}, true)
	if _, limit, _ := limits.admit(m); limit != 4 {
		t.Fatalf("limit = %d, want the ceiling 4", limit)
	}
}

func TestIsConnectionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"limit", &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeLimit, Text: "Rate limit hit."}, true},
		{"unavailable", fmt.Errorf("authenticate: %w", &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeUnavailable, Text: "Server error - Please try again later"}), true},
		{"too many", errors.New("wait for greeting: Too many simultaneous connections"), true},
		{"maximum", errors.New("Maximum number of connections from user+IP exceeded"), true},
		{"cooldown held back here", &LoginCooldownError{Until: time.Now().Add(time.Minute)}, false},
		{"wrong password", &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeAuthenticationFailed, Text: "Invalid credentials"}, false},
		{"network", errors.New("connect to imap.example.com:993: i/o timeout"), false},
		{"nil", nil, false},
	} {
		if got := isConnectionRefusal(tc.err); got != tc.want {
			t.Errorf("%s: isConnectionRefusal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIdleWatcherWaitsForRoomInItsMailbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits, _ := newTestIdleLimits(1, 15)
	cfg := &models.AccountConfig{IMAPHost: "imap.example.com", IMAPPort: 993, Username: "someone@example.com"}
	inbox := limits.join(loginCooldownKey(cfg), 0, nil, models.IdleConnectionLimit{}, false)
	defer limits.leave(inbox)

	var got IdleWatcherStatus
	watcher := NewIdleWatcher(cfg, "", "Receipts", nil, func(status IdleWatcherStatus) {
		got = status
		cancel()
	}).WithMailboxLimit(5, nil)
	watcher.limits = limits
	watcher.connect = func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error) {
		t.Fatal("a folder over its mailbox's limit connected")
		return nil, nil
	}
	watcher.Run(ctx)
	if got.Healthy || got.ReasonCode != "mailbox_limit" || !strings.HasSuffix(got.Reason, "(1)") {
		t.Fatalf("status = %#v, want the mailbox limit of 1", got)
	}
}

func TestIdleWatcherLowersItsMailboxLimitWhenRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits, _ := newTestIdleLimits(3, 15)
	cfg := &models.AccountConfig{IMAPHost: "imap.example.com", IMAPPort: 993, Username: "someone@example.com"}
	store := &memoryIdleLimitStore{}
	key := loginCooldownKey(cfg)
	held := limits.join(key, 0, store, models.IdleConnectionLimit{}, false)
	defer limits.leave(held)
	limits.activate(held, true)
	limits.connected(held, true)

	watcher := NewIdleWatcher(cfg, "", "Sent", nil, func(IdleWatcherStatus) { cancel() }).WithMailboxLimit(1, store)
	watcher.limits = limits
	watcher.connect = func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error) {
		return nil, fmt.Errorf("authenticate: %w", &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeUnavailable, Text: "Server error - Please try again later"})
	}
	watcher.Run(ctx)
	if saved := store.saved(); saved.Limit != 1 || saved.FailedAt != 2 {
		t.Fatalf("saved %+v, want limit 1 after the second connection was refused", saved)
	}
}
