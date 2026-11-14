package storage

import (
	"path/filepath"
	"strings"
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

// The old lists move into policies once: the Default policy's values, and
// an exemption policy first in the order for the people exempted. A
// database copied from an older one moves them when it's opened.
func TestTheAllowedMailListsMoveIntoPoliciesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users (id, username, username_normalized, user_type, created_at, updated_at) VALUES ('ana', 'ana', 'ana', 'webmail', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, user_type, created_at, updated_at) VALUES ('ben', 'ben', 'ben', 'webmail', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, user_type, deletion_pending, created_at, updated_at) VALUES ('cal', 'cal', 'cal', 'webmail', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, user_type, is_admin, created_at, updated_at) VALUES ('root', 'root', 'root', 'management', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		// A policy made before the move, which the exemption goes ahead of.
		`INSERT INTO policies (id, name, name_normalized, is_default, priority, created_at, updated_at) VALUES ('p-field', 'Field', 'field', 0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES ('e1', 'server', 'imap.example.test', 'root')`,
		`INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES ('e2', 'server', '*.example.test', 'root')`,
		`INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES ('e3', 'domain', 'example.test', 'root')`,
		`INSERT INTO mail_allowlist_entries (id, kind, value, created_by) VALUES ('e4', 'provider', 'outlook', 'root')`,
		`INSERT INTO mail_allowlist_exempt_users (user_id, created_by) VALUES ('ben', 'root')`,
		`INSERT INTO mail_allowlist_exempt_users (user_id, created_by) VALUES ('cal', 'root')`,
		`INSERT INTO mail_allowlist_exempt_users (user_id, created_by) VALUES ('root', 'root')`,
		`DELETE FROM schema_version WHERE version = 152`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen := func() *DB {
		t.Helper()
		db, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db = reopen()
	values := func() string {
		t.Helper()
		rows, err := db.Read().Query(`SELECT policy_id, setting_key, value_json FROM policy_values ORDER BY policy_id, setting_key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var policyID, key, value string
			if err := rows.Scan(&policyID, &key, &value); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, policyID+" "+key+"="+value)
		}
		return strings.Join(lines, "\n")
	}
	want := strings.Join([]string{
		`default mail.allowed_address_domains=["example.test"]`,
		`default mail.allowed_providers=["outlook"]`,
		`default mail.allowed_servers=["*.example.test","imap.example.test"]`,
		`mail-exemption mail.allowed_address_domains=[]`,
		`mail-exemption mail.allowed_providers=[]`,
		`mail-exemption mail.allowed_servers=[]`,
	}, "\n")
	if got := values(); got != want {
		t.Fatalf("moved values:\n%s\nwant:\n%s", got, want)
	}
	var order, assigned string
	if err := db.Read().QueryRow(`SELECT group_concat(id || ':' || COALESCE(priority, 0), ' ') FROM (SELECT id, priority FROM policies ORDER BY is_default, priority)`).Scan(&order); err != nil || order != "mail-exemption:1 p-field:2 default:0" {
		t.Fatalf("policies = %q, %v", order, err)
	}
	// Only Ben: Cal is being deleted, and an administrator has no mail.
	if err := db.Read().QueryRow(`SELECT group_concat(target_kind || ':' || target_id || ':' || created_by, ' ') FROM policy_assignments`).Scan(&assigned); err != nil || assigned != "user:ben:root" {
		t.Fatalf("assignments = %q, %v", assigned, err)
	}
	var revision int
	if err := db.Read().QueryRow(`SELECT revision FROM admin_revisions WHERE name = 'policy'`).Scan(&revision); err != nil || revision < 1 {
		t.Fatalf("policy revision = %d, %v", revision, err)
	}
	// The old rows stay, untouched.
	var oldRows int
	if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM mail_allowlist_entries) + (SELECT COUNT(*) FROM mail_allowlist_exempt_users)`).Scan(&oldRows); err != nil || oldRows != 7 {
		t.Fatalf("old rows = %d, %v", oldRows, err)
	}

	// Cleared since, the lists don't come back when the database opens
	// again.
	if _, err := db.Write().Exec(`DELETE FROM policy_values WHERE policy_id = 'default'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	if got := values(); strings.Contains(got, "default ") {
		t.Fatalf("the move ran again:\n%s", got)
	}

	// A database made at 152 that then gets old rows, as a split copies
	// them from an older one, moves them when it's opened.
	copied := filepath.Join(t.TempDir(), "copied.db")
	fresh, err := New(copied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Write().Exec(`INSERT INTO mail_allowlist_entries (id, kind, value) VALUES ('e1', 'domain', 'example.test')`); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if fresh, err = New(copied); err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	var moved string
	if err := fresh.Read().QueryRow(`SELECT value_json FROM policy_values WHERE policy_id = 'default' AND setting_key = 'mail.allowed_address_domains'`).Scan(&moved); err != nil || moved != `["example.test"]` {
		t.Fatalf("copied lists = %q, %v", moved, err)
	}
	var policies int
	if err := fresh.Read().QueryRow(`SELECT COUNT(*) FROM policies`).Scan(&policies); err != nil || policies != 1 {
		t.Fatalf("policies made for a copy with no exemptions = %d, %v", policies, err)
	}
}
