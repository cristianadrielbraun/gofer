package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/directory"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestListSettingsAreCleanedSortedAndChecked(t *testing.T) {
	raw, err := MailAllowedServers.Encode([]string{"IMAP.Example.test", "*.example.test", "imap.example.test", "192.0.2.10"})
	if err != nil || string(raw) != `["*.example.test","192.0.2.10","imap.example.test"]` {
		t.Fatalf("servers = %s, %v", raw, err)
	}
	if raw, err := MailAllowedDomains.Encode([]string{"@Example.TEST"}); err != nil || string(raw) != `["example.test"]` {
		t.Fatalf("domains = %s, %v", raw, err)
	}
	if raw, err := MailAllowedProviders.Encode([]string{"Outlook", "gmail"}); err != nil || string(raw) != `["gmail","outlook"]` {
		t.Fatalf("providers = %s, %v", raw, err)
	}
	for _, refused := range []struct {
		setting *Setting
		raw     string
	}{
		{MailAllowedServers, `null`},
		{MailAllowedServers, `"imap.example.test"`},
		{MailAllowedServers, `["not a host"]`},
		{MailAllowedDomains, `["192.0.2.10/24"]`},
		{MailAllowedProviders, `["yahoo"]`},
	} {
		if _, err := refused.setting.Decode(json.RawMessage(refused.raw)); !errors.Is(err, ErrValueInvalid) {
			t.Errorf("%s %s = %v", refused.setting.Key, refused.raw, err)
		}
	}
	// A value is stored as the setting writes it.
	db := newStore(t)
	saved, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, DefaultID, 0, []ValueChange{{Key: MailAllowedDomains.Key, JSON: json.RawMessage(`["B.example", "a.example", "b.example"]`)}}, "u-admin", monday)
	})
	if err != nil || saved.Version != 1 {
		t.Fatalf("saved = %#v, %v", saved, err)
	}
	values, err := Values(t.Context(), db.Read(), DefaultID)
	if err != nil || len(values) != 1 || string(values[0].JSON) != `["a.example","b.example"]` {
		t.Fatalf("stored = %#v, %v", values, err)
	}
}

// mailListFixture is one state of the old tables.
type mailListFixture struct {
	servers, domains, providers []string
	exempt                      bool
}

func (f mailListFixture) String() string {
	return fmt.Sprintf("servers=%v domains=%v providers=%v exempt=%v", f.servers, f.domains, f.providers, f.exempt)
}

// before is what the old tables gave someone: the lists, or nothing at
// all for a person exempted from them.
func (f mailListFixture) before(exempt bool) storage.MailAllowlist {
	var list storage.MailAllowlist
	list.Servers = append(list.Servers, f.servers...)
	list.Domains = append(list.Domains, f.domains...)
	for _, provider := range f.providers {
		list.Gmail = list.Gmail || provider == "gmail"
		list.Outlook = list.Outlook || provider == "outlook"
	}
	if list.Limited() && exempt {
		return storage.MailAllowlist{}
	}
	return list
}

// The mailboxes and connections every entry point checks: saving and
// editing accounts and identities, DAV, OAuth, and each connection.
var mailListMailboxes = []storage.MailAllowlistMailbox{
	{Provider: "gmail", Email: "ana@example.test"},
	{Provider: "gmail", Email: "ana@gmail.example"},
	{Provider: "gmail", Email: "ana@example.test", CardDAVURL: "https://dav.example.test/carddav/"},
	{Provider: "outlook", Email: "ana@outlook.example"},
	{Provider: "imap", Email: "ana@example.test", IMAPHost: "imap.example.test", SMTPHost: "smtp.example.test"},
	{Provider: "imap", Email: "ana@example.test", IMAPHost: "IMAP.Example.Test", SMTPHost: ""},
	{Provider: "imap", Email: "ana@sub.example.test", IMAPHost: "mail.sub.example.test", SMTPHost: "mail.sub.example.test"},
	{Provider: "imap", Email: "ana@other.test", IMAPHost: "imap.other.test", SMTPHost: "smtp.other.test"},
	{Provider: "imap", Email: "ana@corp.test", IMAPHost: "mail.corp.test", SMTPHost: "192.0.2.10"},
	{Provider: "jmap", Email: "ana@example.test", JMAPSessionURL: "https://jmap.example.test/.well-known/jmap"},
	{Provider: "jmap", Email: "ana@example.test", JMAPSessionURL: "not a url"},
	{Provider: "dav", Username: "ana@example.test", CardDAVURL: "https://dav.example.test/", CalDAVURL: "https://dav.example.test/cal/"},
	{Provider: "dav", Username: "plain-login", CalDAVURL: "https://dav.other.test/"},
	{Provider: "local"},
}

var mailListHosts = []string{"", "imap.example.test", "smtp.corp.test", "192.0.2.10", "dav.other.test", "deep.sub.example.test"}

// decisions is every check's answer for list, as text.
func decisions(list storage.MailAllowlist) []string {
	var answers []string
	for _, mailbox := range mailListMailboxes {
		answers = append(answers, fmt.Sprint("mailbox ", mailbox, ": ", list.CheckMailbox(mailbox)))
	}
	for _, host := range mailListHosts {
		answers = append(answers, fmt.Sprint("server ", host, ": ", list.CheckServer(host)))
	}
	for _, address := range []string{"https://dav.example.test/x", "https://dav.other.test/"} {
		answers = append(answers, fmt.Sprint("url ", address, ": ", list.CheckServerURL(address)))
	}
	for _, provider := range []string{"gmail", "outlook"} {
		answers = append(answers, fmt.Sprint("provider ", provider, ": ", list.CheckProvider(provider)))
	}
	for _, address := range []string{"ana@example.test", "ana@elsewhere.test", "plain-login"} {
		answers = append(answers, fmt.Sprint("address ", address, ": ", list.CheckAddress(address), " login: ", list.CheckLogin(address)))
	}
	return answers
}

// Every combination of the old lists and an exemption decides exactly as
// before once moved into policies, for the exempt person, someone else,
// and a connection made for nobody in particular.
func TestTheAllowedMailListsDecideTheSameAfterTheMove(t *testing.T) {
	var fixtures []mailListFixture
	for _, servers := range [][]string{nil, {"imap.example.test"}, {"*.example.test"}, {"192.0.2.10", "imap.example.test", "mail.corp.test"}} {
		for _, domains := range [][]string{nil, {"example.test"}, {"*.example.test"}} {
			for _, providers := range [][]string{nil, {"gmail"}, {"outlook"}, {"gmail", "outlook"}} {
				for _, exempt := range []bool{false, true} {
					fixtures = append(fixtures, mailListFixture{servers, domains, providers, exempt})
				}
			}
		}
	}
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users (id, username, username_normalized, user_type, created_at, updated_at) VALUES ('u-ines', 'ines', 'ines', 'webmail', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, user_type, created_at, updated_at) VALUES ('u-pavel', 'pavel', 'pavel', 'webmail', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range fixtures {
		// The old tables as they'd be, nothing moved yet; opening the
		// database moves them.
		statements := []string{
			`DELETE FROM policies`, `DELETE FROM admin_revisions`,
			`DELETE FROM mail_allowlist_entries`, `DELETE FROM mail_allowlist_exempt_users`,
		}
		for i, server := range fixture.servers {
			statements = append(statements, fmt.Sprintf(`INSERT INTO mail_allowlist_entries (id, kind, value) VALUES ('s%d', 'server', '%s')`, i, server))
		}
		for i, domain := range fixture.domains {
			statements = append(statements, fmt.Sprintf(`INSERT INTO mail_allowlist_entries (id, kind, value) VALUES ('d%d', 'domain', '%s')`, i, domain))
		}
		for i, provider := range fixture.providers {
			statements = append(statements, fmt.Sprintf(`INSERT INTO mail_allowlist_entries (id, kind, value) VALUES ('p%d', 'provider', '%s')`, i, provider))
		}
		if fixture.exempt {
			statements = append(statements, `INSERT INTO mail_allowlist_exempt_users (user_id) VALUES ('u-ines')`)
		}
		for _, statement := range statements {
			if _, err := db.Write().Exec(statement); err != nil {
				t.Fatal(statement, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if db, err = storage.New(path); err != nil {
			t.Fatal(err)
		}
		for _, person := range []struct {
			id     string
			exempt bool
		}{{"u-ines", fixture.exempt}, {"u-pavel", false}, {"", false}} {
			want := fixture.before(person.exempt)
			got, err := MailAllowlistForUser(t.Context(), db, person.id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s, person %q: lists = %#v, before %#v", fixture, person.id, got, want)
			}
			if before, after := decisions(want), decisions(got); !reflect.DeepEqual(before, after) {
				t.Fatalf("%s, person %q: decisions changed:\nbefore %s\nafter  %s", fixture, person.id, strings.Join(before, "\n"), strings.Join(after, "\n"))
			}
		}
	}
	_ = db.Close()
}

// After the move, the lists come from the policies that reach each
// person, and a change is seen at the next check.
func TestTheAllowedMailListsFollowEachPersonsPolicies(t *testing.T) {
	db := newStore(t)
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'u-ines', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if limited, err := MailListsLimitAnyone(ctx, db.Read()); err != nil || limited {
		t.Fatalf("limited with nothing set = %v, %v", limited, err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, DefaultID, 0, []ValueChange{{Key: MailAllowedServers.Key, JSON: json.RawMessage(`["imap.example.test"]`)}}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := MailAllowlistForUser(ctx, db, "u-ines"); err != nil || !reflect.DeepEqual(list.Servers, []string{"imap.example.test"}) {
		t.Fatalf("Ines with the Default policy's list = %#v, %v", list, err)
	}
	field := create(t, db, "p-field", "Field")
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		if _, err := SetValues(ctx, tx, field.ID, field.Version, []ValueChange{{Key: MailAllowedServers.Key, JSON: json.RawMessage(`["mail.field.test"]`)}}, "u-admin", monday); err != nil {
			return Policy{}, err
		}
		_, err := Assign(ctx, tx, field.ID, []Target{{Kind: TargetGroup, ID: "g-field"}}, "u-admin", monday)
		return Policy{}, err
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := MailAllowlistForUser(ctx, db, "u-ines"); err != nil || !reflect.DeepEqual(list.Servers, []string{"mail.field.test"}) {
		t.Fatalf("Ines through the field team = %#v, %v", list, err)
	}
	if list, err := MailAllowlistForUser(ctx, db, ""); err != nil || !reflect.DeepEqual(list.Servers, []string{"imap.example.test"}) {
		t.Fatalf("nobody in particular = %#v, %v", list, err)
	}
	// Leaving the group gives her the Default policy's again.
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM directory_group_members WHERE user_id = 'u-ines'`); err != nil {
			return Policy{}, err
		}
		return Policy{}, directory.Bump(ctx, tx, directory.RevisionMembership)
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := MailAllowlistForUser(ctx, db, "u-ines"); err != nil || !reflect.DeepEqual(list.Servers, []string{"imap.example.test"}) {
		t.Fatalf("Ines after leaving = %#v, %v", list, err)
	}
	if limited, err := MailListsLimitAnyone(ctx, db.Read()); err != nil || !limited {
		t.Fatalf("limited = %v, %v", limited, err)
	}
}
