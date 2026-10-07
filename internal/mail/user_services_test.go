package mail

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func serviceStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("account service did not enter")
	}
}

func TestUserAccountServiceHTTPReleasesUserCacheAndIgnoresMailReceivingFlag(t *testing.T) {
	f := newUserGmailFixture(t)
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, f.ids["alice"])
		return err
	})
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = io.WriteString(w, "synthetic DAV response")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { f.cancel(); f.worker.Wait() })
	done := gmailAsync(func() error {
		return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Minute, func(ctx context.Context) error {
			snapshot, err := f.accounts.SnapshotServices(ctx, "alice", f.ids["alice"])
			if err != nil {
				return err
			}
			if snapshot.Identity().EmailAddress != "alice@mail.test" {
				return errors.New("wrong snapshot owner")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				return err
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(io.Discard, resp.Body)
			return err
		})
	})
	serviceStarted(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := f.worker.RunAccountService(ctx, "bob", f.ids["bob"], AccountServiceCalendar, time.Second, func(ctx context.Context) error {
		_, err := f.accounts.SnapshotServices(ctx, "bob", f.ids["bob"])
		return err
	})
	close(release)
	if err != nil {
		t.Fatal("slow contact HTTP prevented other owner's calendar snapshot", err)
	}
	if err := gmailAwait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestUserAccountServiceIndependentDomainsAndPerAccountSerialization(t *testing.T) {
	f := newUserGmailFixture(t)
	first, release := make(chan struct{}), make(chan struct{})
	contact := gmailAsync(func() error {
		return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Minute, func(ctx context.Context) error {
			close(first)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	serviceStarted(t, first)
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceCalendar, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal("calendar blocked behind contacts", err)
	}
	if err := f.worker.operation(t.Context(), "alice", f.ids["alice"], 1, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal("body gate collided with service", err)
	}
	if err := f.worker.operation(t.Context(), "alice", f.ids["alice"], -1, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal("migrated negative message ID collided with service", err)
	}
	second := make(chan struct{})
	duplicate := gmailAsync(func() error {
		return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Minute, func(context.Context) error { close(second); return nil })
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		f.worker.mu.Lock()
		gate := f.worker.gates[userIMAPKey{account: f.ids["alice"], service: AccountServiceContacts}]
		waiting := gate != nil && gate.refs == 2
		f.worker.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			close(release)
			t.Fatal("duplicate service was not admitted")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-second:
		close(release)
		t.Fatal("same-account contacts ran concurrently")
	default:
	}
	close(release)
	if err := gmailAwait(t, contact); err != nil {
		t.Fatal(err)
	}
	if err := gmailAwait(t, duplicate); err != nil {
		t.Fatal(err)
	}
	serviceStarted(t, second)
}

func TestUserAccountServiceBoundsContactsWithoutBlockingCalendarOrMail(t *testing.T) {
	f := newUserGmailFixture(t)
	extra, err := f.accounts.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "extra@mail.test", IMAPHost: "imap.mail.test", SMTPHost: "smtp.mail.test", Username: "extra", Password: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var active, peak atomic.Int32
	started := make(chan struct{}, 3)
	jobs := make([]<-chan error, 0, 3)
	for _, job := range []struct{ owner, id string }{{"alice", f.ids["alice"]}, {"bob", f.ids["bob"]}, {"alice", extra.ID}} {
		jobs = append(jobs, gmailAsync(func() error {
			return f.worker.RunAccountService(t.Context(), job.owner, job.id, AccountServiceContacts, time.Minute, func(ctx context.Context) error {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}))
	}
	serviceStarted(t, started)
	serviceStarted(t, started)
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceCalendar, time.Second, func(context.Context) error { return nil }); err != nil {
		close(release)
		t.Fatal("calendar shared busy contact slots", err)
	}
	if err := f.worker.operation(t.Context(), "alice", f.ids["alice"], 0, time.Second, func(context.Context) error { return nil }); err != nil {
		close(release)
		t.Fatal("mail shared busy contact slots", err)
	}
	close(release)
	for _, done := range jobs {
		if err := gmailAwait(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("contact concurrency peak=%d", peak.Load())
	}
}

func TestUserAccountServiceDeletionCancelsActiveAndRejectsWaitingWork(t *testing.T) {
	f := newUserGmailFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	first := gmailAsync(func() error {
		return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Minute, func(ctx context.Context) error { calls.Add(1); close(started); <-ctx.Done(); return ctx.Err() })
	})
	serviceStarted(t, started)
	queued := gmailAsync(func() error {
		return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Minute, func(context.Context) error { calls.Add(1); return nil })
	})
	if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if err := gmailAwait(t, first); err == nil {
		t.Fatal("active deletion was not cancelled")
	}
	if err := gmailAwait(t, queued); err == nil {
		t.Fatal("queued deletion work ran")
	}
	if calls.Load() != 1 {
		t.Fatal("provider callback ran after deletion intent")
	}
	if err := f.worker.RunAccountService(t.Context(), "bob", f.ids["bob"], AccountServiceContacts, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal("other owner stopped", err)
	}
}

func TestUserAccountServiceAccountEditCancelsBothServiceDomains(t *testing.T) {
	f := newUserGmailFixture(t)
	started := make(chan struct{}, 2)
	var jobs []<-chan error
	for _, service := range []AccountService{AccountServiceContacts, AccountServiceCalendar} {
		jobs = append(jobs, gmailAsync(func() error {
			return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], service, time.Minute, func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); return ctx.Err() })
		}))
	}
	serviceStarted(t, started)
	serviceStarted(t, started)
	if err := f.worker.RestartAccount(t.Context(), f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := gmailAwait(t, job); !errors.Is(err, context.Canceled) {
			t.Fatal("old service session survived account edit", err)
		}
	}
}

func TestUserAccountServiceShutdownJoinsOperationsAndRejectsForeignAccess(t *testing.T) {
	f := newUserGmailFixture(t)
	started := make(chan struct{}, 4)
	var jobs []<-chan error
	for _, owner := range []string{"alice", "bob"} {
		for _, service := range []AccountService{AccountServiceContacts, AccountServiceCalendar} {
			jobs = append(jobs, gmailAsync(func() error {
				return f.worker.RunAccountService(t.Context(), owner, f.ids[owner], service, time.Minute, func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); return ctx.Err() })
			}))
		}
	}
	for range 4 {
		serviceStarted(t, started)
	}
	f.cancel()
	joined := make(chan struct{})
	go func() { f.worker.Wait(); close(joined) }()
	serviceStarted(t, joined)
	for _, job := range jobs {
		if err := gmailAwait(t, job); !errors.Is(err, context.Canceled) {
			t.Fatal("shutdown service survived", err)
		}
	}
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceCalendar, time.Second, func(context.Context) error { t.Error("callback ran after shutdown"); return nil }); err == nil {
		t.Fatal("shutdown accepted work")
	}
}

func TestUserAccountServiceRejectsInvalidAndForeignOperations(t *testing.T) {
	f := newUserGmailFixture(t)
	called := false
	fn := func(context.Context) error { called = true; return nil }
	for _, service := range []AccountService{0, AccountService(99)} {
		if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], service, time.Second, fn); err == nil {
			t.Fatal("unknown service accepted")
		}
	}
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, 0, fn); err == nil {
		t.Fatal("missing deadline accepted")
	}
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Second, nil); err == nil {
		t.Fatal("missing operation accepted")
	}
	if err := f.worker.RunAccountService(t.Context(), "bob", f.ids["alice"], AccountServiceContacts, time.Second, fn); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign account accepted", err)
	}
	if called {
		t.Fatal("invalid operation ran")
	}
}

func TestUserAccountServiceTimeoutAndDisableCancelProviderRequests(t *testing.T) {
	for _, change := range []string{"timeout", "disabled"} {
		t.Run(change, func(t *testing.T) {
			f := newUserGmailFixture(t)
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
			t.Cleanup(server.Close)
			t.Cleanup(func() { f.cancel(); f.worker.Wait() })
			timeout := time.Minute
			if change == "timeout" {
				timeout = 300 * time.Millisecond
			}
			done := gmailAsync(func() error {
				return f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceCalendar, timeout, func(ctx context.Context) error {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
					if err != nil {
						return err
					}
					response, err := server.Client().Do(req)
					if response != nil {
						response.Body.Close()
					}
					return err
				})
			})
			serviceStarted(t, started)
			if change == "disabled" {
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := gmailAwait(t, done); err == nil {
				t.Fatal("provider wait survived cancellation")
			}
			if err := f.worker.RunAccountService(t.Context(), "bob", f.ids["bob"], AccountServiceCalendar, time.Second, func(context.Context) error { return nil }); err != nil {
				t.Fatal("cancelled request retained service capacity", err)
			}
		})
	}
}

func TestUserAccountServiceCalendarCapacityIsIndependentOfContacts(t *testing.T) {
	f := newUserGmailFixture(t)
	extra, err := f.accounts.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "calendar-extra@mail.test", IMAPHost: "imap.mail.test", SMTPHost: "smtp.mail.test", Username: "calendar-extra", Password: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var active, peak atomic.Int32
	started := make(chan struct{}, 3)
	var jobs []<-chan error
	for _, job := range []struct{ owner, id string }{{"alice", f.ids["alice"]}, {"bob", f.ids["bob"]}, {"alice", extra.ID}} {
		jobs = append(jobs, gmailAsync(func() error {
			return f.worker.RunAccountService(t.Context(), job.owner, job.id, AccountServiceCalendar, time.Minute, func(ctx context.Context) error {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}))
	}
	serviceStarted(t, started)
	serviceStarted(t, started)
	if err := f.worker.RunAccountService(t.Context(), "alice", f.ids["alice"], AccountServiceContacts, time.Second, func(context.Context) error { return nil }); err != nil {
		close(release)
		t.Fatal("contacts shared busy calendar slots", err)
	}
	close(release)
	for _, job := range jobs {
		if err := gmailAwait(t, job); err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("calendar concurrency peak=%d", peak.Load())
	}
}
