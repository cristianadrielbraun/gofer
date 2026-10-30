package storage

import (
	"path/filepath"
	"testing"
)

// Entries match exactly, or below a "*." pattern's domain but not the domain
// itself; empty lists limit nothing.
func TestMailAllowlistMatching(t *testing.T) {
	var none MailAllowlist
	if none.Limited() || none.CheckServer("imap.anywhere.test") != nil || none.CheckAddress("ana@anywhere.test") != nil || none.CheckProvider("gmail") != nil {
		t.Fatal("empty lists limited something")
	}
	list := MailAllowlist{Servers: []string{"imap.example.test", "*.mail.example.test", "192.0.2.10"}, Domains: []string{"example.test", "*.branch.example.test"}}
	for host, allowed := range map[string]bool{
		"imap.example.test": true, "IMAP.Example.Test.": true, "smtp.example.test": false,
		"a.mail.example.test": true, "b.a.mail.example.test": true, "mail.example.test": false,
		"evilmail.example.test": false, "192.0.2.10": true, "[192.0.2.10]": true, "192.0.2.11": false,
	} {
		if got := list.CheckServer(host) == nil; got != allowed {
			t.Errorf("server %q allowed = %v, want %v", host, got, allowed)
		}
	}
	for address, allowed := range map[string]bool{
		"ana@example.test": true, "ana@EXAMPLE.test": true, "ana@sub.example.test": false,
		"ben@north.branch.example.test": true, "ben@branch.example.test": false, "nobody": false,
	} {
		if got := list.CheckAddress(address) == nil; got != allowed {
			t.Errorf("address %q allowed = %v, want %v", address, got, allowed)
		}
	}
	if err := list.CheckServerURL("https://dav.example.test/carddav/"); err == nil {
		t.Error("DAV server not in the list was allowed")
	}
	if err := list.CheckServerURL("https://x.mail.example.test:8443/"); err != nil {
		t.Error("DAV server under a pattern was refused:", err)
	}
	if err := list.CheckProvider("gmail"); err == nil || err.Error() != "Your Gofer administrator doesn't allow Gmail mailboxes." {
		t.Error("Gmail not refused with its rule:", err)
	}
	list.Outlook = true
	if list.CheckProvider("outlook") != nil || list.CheckProvider("imap") != nil {
		t.Error("allowed provider refused")
	}
	// Gmail mailboxes are checked by provider, not by their Google hosts.
	gmail := MailAllowlist{Gmail: true}
	if err := gmail.CheckMailbox(MailAllowlistMailbox{Provider: "gmail", Email: "ana@gmail.com", IMAPHost: "imap.gmail.com"}); err != nil {
		t.Error("allowed Gmail mailbox refused:", err)
	}
	if err := gmail.CheckMailbox(MailAllowlistMailbox{Provider: "imap", Email: "ana@example.test", IMAPHost: "imap.example.test"}); !IsMailAllowlistError(err) {
		t.Error("only Gmail allowed, but an IMAP server was let in:", err)
	}
	// An account with no mail has no address for the domain list, only a
	// login, which is checked when it is an address. Its DAV server is still
	// checked.
	domains := MailAllowlist{Domains: []string{"example.test"}, Servers: []string{"dav.example.test"}}
	if err := domains.CheckMailbox(MailAllowlistMailbox{Provider: "dav", Username: "ana", CalDAVURL: "https://dav.example.test/"}); err != nil {
		t.Error("account with no mail refused for its plain username:", err)
	}
	if err := domains.CheckMailbox(MailAllowlistMailbox{Provider: "dav", Username: "ana@example.test", CalDAVURL: "https://dav.example.test/"}); err != nil {
		t.Error("account with no mail refused for a login at an allowed domain:", err)
	}
	if err := domains.CheckMailbox(MailAllowlistMailbox{Provider: "dav", Username: "ana@example.org", CardDAVURL: "https://dav.example.test/"}); !IsMailAllowlistError(err) {
		t.Error("account with no mail let in with a login at a domain not allowed:", err)
	}
	if err := (MailAllowlist{Servers: []string{"dav.example.test"}}).CheckMailbox(MailAllowlistMailbox{Provider: "dav", Username: "ana@example.org", CalDAVURL: "https://dav.example.test/"}); err != nil {
		t.Error("login checked against an empty domain list:", err)
	}
	if err := domains.CheckMailbox(MailAllowlistMailbox{Provider: "dav", CalDAVURL: "https://other.example.test/"}); !IsMailAllowlistError(err) {
		t.Error("account with no mail let in to a server not allowed:", err)
	}
	if err := domains.CheckMailbox(MailAllowlistMailbox{Provider: "imap", IMAPHost: "dav.example.test"}); !IsMailAllowlistError(err) {
		t.Error("mailbox with no address let in past the domain list:", err)
	}
}

func TestNormalizeMailAllowlistEntry(t *testing.T) {
	for _, test := range []struct{ kind, in, want string }{
		{MailAllowlistServer, " IMAP.Example.Test. ", "imap.example.test"},
		{MailAllowlistServer, "*.Example.Test", "*.example.test"},
		{MailAllowlistServer, "[2001:db8::1]", "2001:db8::1"},
		{MailAllowlistDomain, "@Example.Test", "example.test"},
	} {
		if got, err := NormalizeMailAllowlistEntry(test.kind, test.in); err != nil || got != test.want {
			t.Errorf("%s %q = %q, %v; want %q", test.kind, test.in, got, err, test.want)
		}
	}
	for _, bad := range []string{"", "*", "*.", "imap.example.test:993", "imap.example.test/path", "a..test", "*.*.test", "ana@example.test"} {
		if _, err := NormalizeMailAllowlistEntry(MailAllowlistServer, bad); err == nil {
			t.Errorf("server entry %q accepted", bad)
		}
	}
	if _, err := NormalizeMailAllowlistEntry(MailAllowlistDomain, "192.0.2.10"); err != nil {
		// A dotted number is a valid name; domains are not checked further.
		t.Error(err)
	}
}

// The lists are stored once each, providers only through their switch, and
// an exempted person gets no limits.
func TestMailAllowlistStorage(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "allowlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('ana','ana','ana'),('ben','ben','ben')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.AddMailAllowlistEntry(ctx, MailAllowlistServer, "IMAP.example.test", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AddMailAllowlistEntry(ctx, MailAllowlistDomain, "example.test", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMailAllowlistEntry(ctx, MailAllowlistProvider, "gmail", "admin"); err == nil {
		t.Fatal("provider added as an entry")
	}
	if err := db.SetMailAllowlistProvider(ctx, "gmail", true, "admin"); err != nil {
		t.Fatal(err)
	}
	list, err := db.GetMailAllowlist(ctx)
	if err != nil || len(list.Servers) != 1 || list.Servers[0] != "imap.example.test" || len(list.Domains) != 1 || !list.Gmail || list.Outlook {
		t.Fatalf("lists = %+v, %v", list, err)
	}
	if err := db.SetMailAllowlistExempt(ctx, "ben", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := db.MailAllowlistForUser(ctx, "ben"); err != nil || got.Limited() {
		t.Fatalf("exempt person limited: %+v, %v", got, err)
	}
	if got, err := db.MailAllowlistForUser(ctx, "ana"); err != nil || !got.Limited() {
		t.Fatalf("person not limited: %+v, %v", got, err)
	}
	entries, err := db.ListMailAllowlistEntries(ctx)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if err := db.DeleteMailAllowlistEntry(ctx, entry.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The provider switch is not removed with the entries.
	if list, err = db.GetMailAllowlist(ctx); err != nil || len(list.Servers) != 0 || len(list.Domains) != 0 || !list.Gmail {
		t.Fatalf("after deleting entries = %+v, %v", list, err)
	}
	if err := db.SetMailAllowlistExempt(ctx, "ben", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if exempt, err := db.IsMailAllowlistExempt(ctx, "ben"); err != nil || exempt {
		t.Fatal("exemption not removed", err)
	}
}
