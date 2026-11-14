package handler

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A change to the rules made anywhere, not only on Mail security, stops the
// mailboxes it refuses and reconnects the ones it lets in again, at the
// worker's next check; nothing is checked again while nothing changed.
func TestMailRefusalsFollowEveryChangeToTheRules(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'; UPDATE sessions SET step_up_at=CURRENT_TIMESTAMP,step_up_method='totp' WHERE user_id='administrator'`); err != nil {
		t.Fatal(err)
	}
	hosts := map[string][2]string{"alice": {"alice@example.com", "imap.mail.example.com"}, "bob": {"bob@example.com", "imap.elsewhere.test"}}
	for owner, mailbox := range hosts {
		if err := f.h.withUserDB(davTestContext(t), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET provider='imap',email_address=?,imap_host=?,imap_port=993,imap_tls_mode='tls',smtp_host=?,smtp_port=465,smtp_tls_mode='tls'`, mailbox[0], mailbox[1], mailbox[1])
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var stopped, restarted []string
	f.admin.mailRefusals.stop = func(id string) { stopped = append(stopped, id) }
	f.admin.mailRefusals.restart = func(_ context.Context, id string) error { restarted = append(restarted, id); return nil }
	check := func(wantStopped, wantRestarted []string) {
		t.Helper()
		stopped, restarted = nil, nil
		f.admin.checkMailRefusalsIfChanged(davTestContext(t))
		slices.Sort(stopped)
		slices.Sort(restarted)
		if !reflect.DeepEqual(stopped, wantStopped) || !reflect.DeepEqual(restarted, wantRestarted) {
			t.Fatalf("stopped %v and reconnected %v, want %v and %v", stopped, restarted, wantStopped, wantRestarted)
		}
	}
	alice, bob := f.accounts["alice"].ID, f.accounts["bob"].ID
	ctx := davTestContext(t)
	actor, session := "administrator", f.session.ID

	check(nil, nil)
	// The Default policy limits servers: Bob's mailbox is outside them.
	// Alice's contacts come from a CardDAV server on 127.0.0.1.
	for _, server := range []string{"*.example.com", "127.0.0.1"} {
		if _, err := f.manager.ChangeMailList(ctx, auth.MailListChange{ActorUserID: actor, ActorSessionID: session, List: storage.MailAllowlistServer, Value: server, Op: auth.MailListAdd}); err != nil {
			t.Fatal(err)
		}
	}
	check([]string{bob}, nil)
	check(nil, nil)

	// A policy for a group of Alice's limits her to another server; leaving
	// the group lets her in again.
	field, err := f.manager.CreateGroup(ctx, auth.CreateGroupOptions{ActorUserID: actor, ActorSessionID: session, Name: "Field team"})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := f.manager.CreatePolicy(ctx, auth.CreatePolicyOptions{ActorUserID: actor, ActorSessionID: session, Name: "Strict"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.SetPolicyValues(ctx, auth.SetPolicyValuesOptions{ActorUserID: actor, ActorSessionID: session, PolicyID: strict.ID, Version: strict.Version,
		Changes: []policy.ValueChange{{Key: policy.MailAllowedServers.Key, JSON: []byte(`["imap.other.test"]`)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.AssignPolicy(ctx, auth.ChangePolicyAssignmentsOptions{ActorUserID: actor, ActorSessionID: session, PolicyID: strict.ID, Targets: []policy.Target{{Kind: policy.TargetGroup, ID: field.ID}}}); err != nil {
		t.Fatal(err)
	}
	check(nil, nil)
	if _, err := f.manager.AddGroupMembers(ctx, auth.ChangeGroupMembersOptions{ActorUserID: actor, ActorSessionID: session, GroupID: field.ID, UserIDs: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	check([]string{alice}, nil)
	if _, err := f.manager.RemoveGroupMembers(ctx, auth.ChangeGroupMembersOptions{ActorUserID: actor, ActorSessionID: session, GroupID: field.ID, UserIDs: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	check(nil, []string{alice})

	// A policy setting every list to nothing lets Bob in again.
	exempt, err := f.manager.CreatePolicy(ctx, auth.CreatePolicyOptions{ActorUserID: actor, ActorSessionID: session, Name: "No limits"})
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{storage.MailAllowlistServer, storage.MailAllowlistDomain, storage.MailAllowlistProvider} {
		if _, err := f.manager.ChangeMailList(ctx, auth.MailListChange{ActorUserID: actor, ActorSessionID: session, PolicyID: exempt.ID, List: list, Op: auth.MailListSet}); err != nil {
			t.Fatal(err)
		}
	}
	bobOnly := auth.ChangePolicyAssignmentsOptions{ActorUserID: actor, ActorSessionID: session, PolicyID: exempt.ID, Targets: []policy.Target{{Kind: policy.TargetUser, ID: "bob"}}}
	if _, err := f.manager.AssignPolicy(ctx, bobOnly); err != nil {
		t.Fatal(err)
	}
	check(nil, []string{bob})

	// Mail security lists the last check, and when it was made.
	if _, err := f.manager.UnassignPolicy(ctx, bobOnly); err != nil {
		t.Fatal(err)
	}
	check([]string{bob}, nil)
	stopped, restarted = nil, nil
	page := f.request("/admin/security")
	if body := page.Body.String(); page.Code != 200 || !strings.Contains(body, "imap.elsewhere.test isn&#39;t an allowed server") || !strings.Contains(body, "data-admin-allowlist-checked") {
		i := strings.LastIndex(body, "Mailboxes outside the lists")
		t.Fatalf("Mail security = %d %q", page.Code, body[max(i, 0):min(max(i, 0)+1500, len(body))])
	}
	if len(stopped) != 0 || len(restarted) != 0 {
		t.Fatalf("the page checked again: stopped %v, reconnected %v", stopped, restarted)
	}
}
