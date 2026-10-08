package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserRemovalQueueBoundsAndDeduplicatesAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &userRemovalWorker{ctx: ctx, queue: make(chan userRemovalJob, 32), pending: make(map[string]struct{})}
	for i := 0; i < 32; i++ {
		if err := w.enqueue(userRemovalJob{owner: fmt.Sprintf("owner-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.enqueue(userRemovalJob{owner: "owner-0"}); err != nil {
		t.Fatal("duplicate did not coalesce", err)
	}
	if err := w.enqueue(userRemovalJob{owner: "overflow"}); !errors.Is(err, errUserRemovalBusy) {
		t.Fatal("full queue admitted unbounded work", err)
	}
	if len(w.pending) != 32 || len(w.queue) != 32 {
		t.Fatal("queue or retained history exceeded bound", len(w.pending), len(w.queue))
	}
	cancel()
	if err := w.enqueue(userRemovalJob{owner: "owner-0"}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled runtime admitted duplicate work", err)
	}
}

func TestOwnedAccountCleanupIsJoinedAfterRootCancellation(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	h := f.h
	h.userStorageContext = t.Context()
	h.userDeletions = make(map[string]*userAccountDeletionJob)
	entered, canceled, permitFinish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finish sync.Once
	defer finish.Do(func() { close(permitFinish) })
	h.userAccountHooks.Cleanup = func(ctx context.Context, id string) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-permitFinish
		return ctx.Err()
	}
	id := f.accounts["alice"].ID
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/"+id, nil)
	req.SetPathValue("id", id)
	req = req.WithContext(auth.ContextWithUser(t.Context(), &auth.User{ID: "alice"}))
	w := httptest.NewRecorder()
	h.handleUserDeleteAccount(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("accepted account cleanup did not start")
	}
	f.cancel()
	select {
	case <-canceled:
	case <-time.After(30 * time.Second):
		t.Fatal("root cancellation did not reach account cleanup")
	}
	joined := make(chan struct{})
	go func() { h.userIMAP.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("runtime joined before accepted account cleanup finished")
	case <-time.After(20 * time.Millisecond):
	}
	finish.Do(func() { close(permitFinish) })
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("runtime did not join completed account cleanup")
	}
	h.accountDeleteMu.Lock()
	n := len(h.userDeletions)
	h.accountDeleteMu.Unlock()
	if n != 0 {
		t.Fatal("joined cleanup retained running jobs", n)
	}
	if state, err := h.userStorage.AccountStateForUser(t.Context(), "alice", id); err != nil || state != storage.AccountDeleting {
		t.Fatal("interrupted cleanup lost durable intent", state, err)
	}
}
