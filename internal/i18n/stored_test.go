package i18n

import (
	"context"
	"testing"
)

func TestStoredWritesKnownEnglishInTheReadersLanguage(t *testing.T) {
	es := WithLocale(context.Background(), "es")
	saved := Translate(English, "jobs.transfer.stopped", "error", "NO [OVERQUOTA] mailbox full")
	if got, want := Stored(es, "jobs.", saved), Translate("es", "jobs.transfer.stopped", "error", "NO [OVERQUOTA] mailbox full"); got != want {
		t.Fatalf("Stored = %q, want %q", got, want)
	}
	if got, want := Stored(es, "jobs.", Translate(English, "jobs.account_gone")), Translate("es", "jobs.account_gone"); got != want {
		t.Fatalf("Stored = %q, want %q", got, want)
	}
	for _, text := range []string{"", "connection reset by peer", "Something a server said."} {
		if got := Stored(es, "jobs.", text); got != text {
			t.Fatalf("Stored(%q) = %q, want it as stored", text, got)
		}
	}
	if got := Stored(context.Background(), "jobs.", saved); got != saved {
		t.Fatalf("English reader: %q", got)
	}
}
