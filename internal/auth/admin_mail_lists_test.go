package auth

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestTheAllowedMailListsChangeOneEntryAtATimeOnAnyPolicy(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	change := func(policyID, list, value string, op MailListOp) (string, error) {
		return manager.ChangeMailList(ctx, MailListChange{ActorUserID: "administrator", ActorSessionID: session.ID, PolicyID: policyID, List: list, Value: value, Op: op})
	}
	stored := func(policyID string, setting *policy.Setting) (string, bool) {
		t.Helper()
		values, err := policy.Values(ctx, manager.db.Read(), policyID)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			if value.Key == setting.Key {
				return string(value.JSON), true
			}
		}
		return "", false
	}
	if _, err := manager.ChangeMailList(ctx, MailListChange{ActorUserID: "ines", ActorSessionID: "s", List: storage.MailAllowlistServer, Value: "imap.example.test", Op: MailListAdd}); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("a webmail user changes the lists = %v", err)
	}

	// The Default policy's lists.
	if value, err := change("", storage.MailAllowlistServer, "IMAP.Example.Test", MailListAdd); err != nil || value != "imap.example.test" {
		t.Fatalf("add a server = %q, %v", value, err)
	}
	for _, step := range []struct {
		list, value string
		op          MailListOp
	}{
		{storage.MailAllowlistServer, "*.example.test", MailListAdd},
		{storage.MailAllowlistServer, "imap.example.test", MailListAdd}, // there already: nothing changes
		{storage.MailAllowlistDomain, "@example.test", MailListAdd},
		{storage.MailAllowlistProvider, "gmail", MailListAdd},
		{storage.MailAllowlistServer, "imap.example.test", MailListRemove},
		{storage.MailAllowlistServer, "imap.nowhere.test", MailListRemove}, // not there: nothing changes
		{storage.MailAllowlistServer, "", MailListSet},                     // the Default policy always sets them
	} {
		if _, err := change(policy.DefaultID, step.list, step.value, step.op); err != nil {
			t.Fatalf("%s %s %q: %v", step.op, step.list, step.value, err)
		}
	}
	if _, err := change("", storage.MailAllowlistServer, "imap.example.test:993", MailListAdd); !errors.Is(err, storage.ErrMailAllowlistEntry) {
		t.Fatalf("a server with a port = %v", err)
	}
	if _, err := change("", storage.MailAllowlistProvider, "yahoo", MailListAdd); !errors.Is(err, ErrMailListProvider) {
		t.Fatalf("a provider other than Gmail or Outlook = %v", err)
	}
	if value, ok := stored(policy.DefaultID, policy.MailAllowedServers); !ok || value != `["*.example.test"]` {
		t.Fatalf("the Default policy's servers = %s, %v", value, ok)
	}
	// Emptied, the Default policy's list is cleared: no limit either way.
	if _, err := change("", storage.MailAllowlistServer, "*.example.test", MailListRemove); err != nil {
		t.Fatal(err)
	}
	if value, ok := stored(policy.DefaultID, policy.MailAllowedServers); ok {
		t.Fatalf("an emptied Default list stays: %s", value)
	}

	// A named policy leaves a list to the others until it sets it; set and
	// emptied, it limits nothing for its people.
	field, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Field"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := change(field.ID, storage.MailAllowlistDomain, "", MailListClear); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored(field.ID, policy.MailAllowedDomains); ok {
		t.Fatal("clearing an unset list set it")
	}
	if _, err := change(field.ID, storage.MailAllowlistDomain, "", MailListSet); err != nil {
		t.Fatal(err)
	}
	if value, ok := stored(field.ID, policy.MailAllowedDomains); !ok || value != `[]` {
		t.Fatalf("a set list = %s, %v", value, ok)
	}
	if _, err := change(field.ID, storage.MailAllowlistDomain, "field.test", MailListAdd); err != nil {
		t.Fatal(err)
	}
	if _, err := change(field.ID, storage.MailAllowlistDomain, "field.test", MailListRemove); err != nil {
		t.Fatal(err)
	}
	if value, ok := stored(field.ID, policy.MailAllowedDomains); !ok || value != `[]` {
		t.Fatalf("a named policy's emptied list = %s, %v", value, ok)
	}
	// Adding to an unset list sets it to that entry alone.
	if _, err := change(field.ID, storage.MailAllowlistServer, "mail.field.test", MailListAdd); err != nil {
		t.Fatal(err)
	}
	if value, ok := stored(field.ID, policy.MailAllowedServers); !ok || value != `["mail.field.test"]` {
		t.Fatalf("a named policy's servers = %s, %v", value, ok)
	}
	if _, err := change(field.ID, storage.MailAllowlistServer, "", MailListClear); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored(field.ID, policy.MailAllowedServers); ok {
		t.Fatal("a cleared list stays set")
	}
	if _, err := change("no-such-policy", storage.MailAllowlistServer, "", MailListSet); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("a policy that doesn't exist = %v", err)
	}

	lists, err := manager.MailLists(ctx, "administrator")
	want := &MailLists{MailAllowlist: storage.MailAllowlist{Domains: []string{"example.test"}, Gmail: true}}
	if err != nil || !reflect.DeepEqual(lists, want) {
		t.Fatalf("lists = %#v, %v; want %#v", lists, err, want)
	}
	// Each change recorded once, as a policy change.
	counts := map[string]int{}
	for _, record := range policyAuditRecords(t, manager) {
		counts[record.Action]++
	}
	if want := map[string]int{"policy.settings_changed": 11, "policy.created": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("recorded = %v, want %v", counts, want)
	}
}
