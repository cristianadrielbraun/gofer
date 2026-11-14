package auth

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestMailSecurityChangesTheDefaultPolicysListsAndExemptsThroughAPolicy(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	change := func(list, value string, add bool) (string, error) {
		return manager.ChangeMailList(ctx, MailListChange{ActorUserID: "administrator", ActorSessionID: session.ID, List: list, Value: value, Add: add})
	}
	if _, err := manager.ChangeMailList(ctx, MailListChange{ActorUserID: "ines", ActorSessionID: "s", List: storage.MailAllowlistServer, Value: "imap.example.test", Add: true}); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("a webmail user changes the lists = %v", err)
	}
	if value, err := change(storage.MailAllowlistServer, "IMAP.Example.Test", true); err != nil || value != "imap.example.test" {
		t.Fatalf("add a server = %q, %v", value, err)
	}
	if _, err := change(storage.MailAllowlistServer, "*.example.test", true); err != nil {
		t.Fatal(err)
	}
	// Adding one that's there changes nothing.
	if _, err := change(storage.MailAllowlistServer, "imap.example.test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := change(storage.MailAllowlistServer, "imap.example.test:993", true); !errors.Is(err, storage.ErrMailAllowlistEntry) {
		t.Fatalf("a server with a port = %v", err)
	}
	if _, err := change(storage.MailAllowlistProvider, "yahoo", true); !errors.Is(err, ErrMailListProvider) {
		t.Fatalf("a provider other than Gmail or Outlook = %v", err)
	}
	if _, err := change(storage.MailAllowlistDomain, "@example.test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := change(storage.MailAllowlistProvider, "gmail", true); err != nil {
		t.Fatal(err)
	}
	if _, err := change(storage.MailAllowlistServer, "imap.example.test", false); err != nil {
		t.Fatal(err)
	}

	// A policy made before the exemption, which the exemption goes ahead of.
	field, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: session.ID, Name: "Field"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetMailListExemption(ctx, "administrator", session.ID, "pavel", false); err != nil {
		t.Fatalf("hold someone to the lists before any exemption = %v", err)
	}
	if _, err := policy.Get(ctx, manager.db.Read(), policy.MailExemptionID); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("holding someone to the lists made the exemption policy: %v", err)
	}
	exempted, err := manager.SetMailListExemption(ctx, "administrator", session.ID, "pavel", true)
	if err != nil || len(exempted.Changed) != 1 || exempted.Changed[0].Name != "pavel" {
		t.Fatalf("exempt Pavel = %#v, %v", exempted, err)
	}
	if _, err := manager.SetMailListExemption(ctx, "administrator", session.ID, "administrator", true); !errors.Is(err, policy.ErrTargetNotFound) {
		t.Fatalf("exempt an administrator = %v", err)
	}

	lists, err := manager.MailLists(ctx, "administrator")
	want := &MailLists{MailAllowlist: storage.MailAllowlist{Servers: []string{"*.example.test"}, Domains: []string{"example.test"}, Gmail: true}, Exempt: []string{"pavel"}}
	if err != nil || !reflect.DeepEqual(lists, want) {
		t.Fatalf("lists = %#v, %v; want %#v", lists, err, want)
	}
	order, err := policy.List(ctx, manager.db.Read())
	if err != nil || len(order) != 3 || order[0].ID != policy.MailExemptionID || order[0].Priority != 1 || order[1].ID != field.ID || order[1].Priority != 2 {
		t.Fatalf("order = %#v, %v", order, err)
	}
	pavel, err := policy.MailAllowlistForUser(ctx, manager.db, "pavel")
	if err != nil || pavel.Limited() {
		t.Fatalf("Pavel's lists = %#v, %v", pavel, err)
	}
	if ines, err := policy.MailAllowlistForUser(ctx, manager.db, "ines"); err != nil || !reflect.DeepEqual(ines, want.MailAllowlist) {
		t.Fatalf("Ines's lists = %#v, %v", ines, err)
	}

	// Each change recorded once, as a policy change.
	counts := map[string]int{}
	for _, record := range policyAuditRecords(t, manager) {
		counts[record.Action]++
	}
	if want := map[string]int{"policy.settings_changed": 5, "policy.created": 2, "policy.assigned": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("recorded = %v, want %v", counts, want)
	}
	if again, err := manager.SetMailListExemption(ctx, "administrator", session.ID, "pavel", false); err != nil || len(again.Changed) != 1 {
		t.Fatalf("hold Pavel to the lists again = %#v, %v", again, err)
	}
}
