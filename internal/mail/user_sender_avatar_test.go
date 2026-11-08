package mail

import (
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// New mail from a routed account carries the sender's cached avatar, but
// only one the owner's own mail or contacts can see.
func TestRoutedNewMailAvatarUsesOwnersVisibleCache(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	for _, email := range []string{"sender@mail.test", "stranger@mail.test"} {
		if err := f.system.SaveSenderAvatarFound(t.Context(), avatarresolver.GravatarHash(email), email, "gravatar", "image/png", "", []byte("png"), expires, "found", "missing"); err != nil {
			t.Fatal(err)
		}
	}
	o := &SyncOrchestrator{imapScope: &userIMAPScope{accounts: f.accounts, owner: "alice", id: f.ids["alice"]}}
	want := storage.SenderAvatarURL(avatarresolver.GravatarHash("sender@mail.test"), expires)
	if got := o.imapSenderAvatarURL(t.Context(), "sender@mail.test"); got == "" || got != want {
		t.Fatalf("visible sender avatar = %q, want %q", got, want)
	}
	if got := o.imapSenderAvatarURL(t.Context(), "stranger@mail.test"); got != "" {
		t.Fatalf("avatar for a sender alice never saw = %q", got)
	}
}
