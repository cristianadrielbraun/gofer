package notifications

import (
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestUserMailSecurityStopClosesNativeIDLEAndPreservesStores(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	server.idleSupported = true
	startBackgroundIMAP(t, f, server, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(server.idlePeers("alice")) == 1 && len(server.idlePeers("bob")) == 1 })

	f.imap.StopAccount(f.accounts["alice"].ID)
	waitUserIMAP(t, func() bool { return len(server.idlePeers("alice")) == 0 })
	if len(server.idlePeers("bob")) != 1 {
		t.Fatal("selective stop closed another owner's native IDLE")
	}
	// Both accounts use the same endpoint exception. Check selective current-
	// session cancellation while that policy remains valid; a queued startup
	// receive must not independently fail Bob's snapshot during this assertion.
	policies, err := f.system.ListMailSecurityPolicies(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		if policy.Kind == models.MailSecurityExceptionPlaintextTransport {
			if err := f.system.DeleteMailSecurityException(t.Context(), policy.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.imap.StopMailSecuritySessions()
	waitUserIMAP(t, func() bool { return len(server.idlePeers("bob")) == 0 })
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err == nil {
			t.Fatal("new native work bypassed revoked policy", owner)
		}
		if f.messageCount(t, owner) != 2 {
			t.Fatal("session cancellation deleted or replaced mailbox data", owner)
		}
		states, err := f.imap.IdleStatusesForUser(t.Context(), owner)
		if err != nil || len(states) != 0 {
			t.Fatal("stopped native IDLE still reported", owner, states, err)
		}
	}
}
