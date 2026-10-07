package mail

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserLifecycleStopCancelsWholeOwnerAndAccountWork(t *testing.T) {
	f := newUserGmailFixture(t)
	t.Cleanup(func() { f.cancel(); f.worker.Wait() })
	type work struct {
		owner   string
		service AccountService
		started chan struct{}
		done    chan error
	}
	jobs := []work{
		{owner: "alice", started: make(chan struct{}), done: make(chan error, 1)},
		{owner: "alice", service: AccountServiceContacts, started: make(chan struct{}), done: make(chan error, 1)},
		{owner: "alice", service: AccountServiceCalendar, started: make(chan struct{}), done: make(chan error, 1)},
		{owner: "bob", started: make(chan struct{}), done: make(chan error, 1)},
	}
	for _, job := range jobs {
		go func() {
			fn := func(ctx context.Context) error { close(job.started); <-ctx.Done(); return ctx.Err() }
			if job.service == 0 {
				job.done <- f.worker.RunUserServiceWork(t.Context(), job.owner, fn)
			} else {
				job.done <- f.worker.RunAccountService(t.Context(), job.owner, f.ids[job.owner], job.service, time.Minute, fn)
			}
		}()
	}
	for _, job := range jobs {
		serviceStarted(t, job.started)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	f.worker.StopUser("alice")
	for _, job := range jobs[:3] {
		if err := gmailAwait(t, job.done); !errors.Is(err, context.Canceled) {
			t.Fatal(job.service, err)
		}
	}
	select {
	case err := <-jobs[3].done:
		t.Fatal("foreign work canceled", err)
	default:
	}
	if err := f.worker.RunUserServiceWork(t.Context(), "alice", func(context.Context) error { t.Error("disabled owner entered work"); return nil }); err == nil {
		t.Fatal("disabled owner admitted")
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.RunUserServiceWork(t.Context(), "alice", func(context.Context) error { return nil }); err != nil {
		t.Fatal("new enabled work rejected", err)
	}
	f.cancel()
	f.worker.Wait()
	if err := gmailAwait(t, jobs[3].done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.worker.mu.Lock()
	remaining := len(f.worker.userRuns)
	f.worker.mu.Unlock()
	if remaining != 0 {
		t.Fatal("retained user work registrations", remaining)
	}
}

func TestUserLifecycleStopRetiresBrowserPollingGeneration(t *testing.T) {
	t.Setenv("GOFER_GMAIL_API_POLL", "on")
	f := newUserGmailFixture(t)
	t.Cleanup(func() { f.cancel(); f.worker.Wait() })
	alice, releaseOld := activeGmailJob(t, f, "alice")
	defer releaseOld()
	bob, releaseBob := activeGmailJob(t, f, "bob")
	defer releaseBob()
	f.worker.StopUser("alice")
	if alice.session.ctx.Err() == nil || bob.session.ctx.Err() != nil {
		t.Fatal("wrong polling sessions canceled")
	}
	fresh, releaseFresh := activeGmailJob(t, f, "alice")
	defer releaseFresh()
	if fresh.session == alice.session || fresh.session.ctx.Err() != nil {
		t.Fatal("stopped generation reused")
	}
	releaseOld()
	f.worker.mu.Lock()
	current := f.worker.activePollUsers["alice"]
	f.worker.mu.Unlock()
	if current != fresh.session || current.ctx.Err() != nil {
		t.Fatal("old browser release removed replacement")
	}
}
