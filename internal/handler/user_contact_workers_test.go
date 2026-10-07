package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func waitUserContactWorkers(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("contact workers did not reach the expected state")
		case <-tick.C:
		}
	}
}

func TestUserContactWorkersStartupCadenceAndDurableWake(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		kind := "write"
		if r.Method == "GET" {
			kind = "preflight"
			if r.URL.Query().Get("$filter") == "" {
				kind = "pull"
			}
		}
		mu.Lock()
		counts[owner+"|"+kind]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"value":[]}`))
		} else {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "created-" + owner, "changeKey": "v1"})
		}
	})
	f := newUserContactPushFixture(t, "outlook", api)
	for _, owner := range []string{"alice", "bob"} {
		seedUserContactPush(t, f, owner, "outlook")
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := f.h.StartUserContactSync(ctx, UserContactSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.StartUserContactSync(ctx, UserContactSyncOptions{}); err == nil {
		t.Fatal("duplicate scheduling allowed")
	}
	waitUserContactWorkers(t, func() bool {
		var scheduled int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_service_schedule WHERE service='contacts' AND next_due_ms>?`, time.Now().Add(4*time.Minute).UnixMilli()).Scan(&scheduled); err != nil {
			t.Fatal(err)
		}
		var queues int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE next_due_ms>?`, time.Now().Add(30*time.Minute).UnixMilli()).Scan(&queues); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return scheduled == 2 && queues == 2 && counts["alice|write"] == 1 && counts["bob|write"] == 1
	})
	// A global wake only scans central deadlines; completed idle owners and
	// accounts must not be pulled/probed on every short scan tick.
	f.h.signalUserContacts()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if counts["alice|pull"] != 1 || counts["bob|pull"] != 1 {
		t.Fatal("cadence ignored", counts)
	}
	mu.Unlock()
	seedUserContactPush(t, f, "alice", "outlook")
	if err := f.h.WakeUserContactQueue(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	waitUserContactWorkers(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var queues int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice' AND next_due_ms>?`, time.Now().Add(30*time.Minute).UnixMilli()).Scan(&queues); err != nil {
			t.Fatal(err)
		}
		return queues == 1 && counts["alice|write"] == 2
	})
	if err := f.h.WakeUserContactAccount(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserContactWorkers(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var scheduled int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_service_schedule WHERE account_id=? AND service='contacts' AND next_due_ms>?`, f.accounts["alice"].ID, time.Now().Add(4*time.Minute).UnixMilli()).Scan(&scheduled); err != nil {
			t.Fatal(err)
		}
		return scheduled == 1 && counts["alice|pull"] == 2
	})
	cancel()
	select {
	case <-f.h.userContactWorkers.done:
	case <-time.After(10 * time.Second):
		t.Fatal("caller cancellation did not join contact workers")
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
			var started, done int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_activity_events WHERE event_type='contact_sync_started'`).Scan(&started); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE status='done'`).Scan(&done); err != nil {
				return err
			}
			want := 1
			if owner == "alice" {
				want = 2
			}
			if started != want || done != want {
				t.Fatal("wrong owned activity/jobs", owner, started, done)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserContactWorkersRootShutdownCancelsBlockedProviderAndJoins(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
	})
	f := newUserContactPushFixture(t, "gmail", api)
	if err := f.h.StartUserContactSync(t.Context(), UserContactSyncOptions{ScanInterval: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("startup provider pull did not run")
	}
	f.cancel()
	joined := make(chan struct{})
	go func() { f.h.userIMAP.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("runtime did not join contacts dispatcher/provider work")
	}
	select {
	case <-f.h.userContactWorkers.done:
	default:
		t.Fatal("runtime returned before contacts dispatcher")
	}
}

func TestUserContactWorkersQueueIsBoundedAndCoalescesOwners(t *testing.T) {
	w := &userContactWorkers{queue: make(chan userContactJob, 32), wake: make(chan struct{}, 1), pending: make(map[userContactJob]bool)}
	for i := 0; i < 32; i++ {
		if !w.enqueue(userContactJob{owner: fmt.Sprintf("owner-%d", i)}) {
			t.Fatal("queue filled early")
		}
	}
	if w.enqueue(userContactJob{owner: "overflow"}) || !w.enqueue(userContactJob{owner: "owner-0"}) || len(w.pending) != 32 {
		t.Fatal("unbounded queue or duplicate owner")
	}
	for i := 0; i < 100; i++ {
		w.signal(true)
	}
	if len(w.wake) != 1 || !w.rescan {
		t.Fatal("wake signals did not coalesce")
	}
}
