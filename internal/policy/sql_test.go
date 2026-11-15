package policy

import (
	"fmt"
	"math/rand"
	"testing"
)

// On random policies, assignments and groups, the queries name exactly the
// people the resolver gives a named policy's "on" for the setting.
func TestTheQueriesAgreeWithTheResolver(t *testing.T) {
	people := []string{"u-ana", "u-ben", "u-cleo", "u-dev", "u-eli", "u-fay"}
	groups := []string{"g-a", "g-b", "g-c"}
	for round := 0; round < 60; round++ {
		r := rand.New(rand.NewSource(int64(round)))
		db := newStore(t)
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := db.Write().Exec(query, args...); err != nil {
				t.Fatal(query, err)
			}
		}
		for _, id := range people {
			exec(`INSERT INTO users (id, username, username_normalized, created_at, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, id, id)
		}
		for _, id := range groups {
			exec(`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, id, id)
			for _, person := range people {
				if r.Intn(3) == 0 {
					exec(`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, id, person)
				}
			}
		}
		count := 1 + r.Intn(5)
		for _, priority := range r.Perm(count) {
			id := fmt.Sprintf("p-%d", priority)
			exec(`INSERT INTO policies (id, name, name_normalized, is_default, priority, created_at, updated_at) VALUES (?, ?, ?, 0, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, id, id, priority+1)
			// Unset, on, off, or a value that doesn't check, which both skip.
			if r.Intn(2) == 0 {
				exec(`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, id, MFAPasskeyCounts.Key, []string{"true", "false"}[r.Intn(2)])
			}
			if choice := r.Intn(4); choice > 0 {
				exec(`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, id, OwnProxies.Key, []string{"true", "false", `"maybe"`}[choice-1])
			}
			switch r.Intn(4) {
			case 1:
				exec(`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES (?, ?, 'true', CURRENT_TIMESTAMP)`, id, MFARequired.Key)
			case 2:
				exec(`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES (?, ?, 'false', CURRENT_TIMESTAMP)`, id, MFARequired.Key)
			case 3:
				exec(`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES (?, ?, '"yes"', CURRENT_TIMESTAMP)`, id, MFARequired.Key)
			}
			for _, person := range people {
				if r.Intn(4) == 0 {
					exec(`INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at) VALUES (?, 'user', ?, CURRENT_TIMESTAMP)`, id, person)
				}
			}
			for _, group := range groups {
				if r.Intn(3) == 0 {
					exec(`INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at) VALUES (?, 'group', ?, CURRENT_TIMESTAMP)`, id, group)
				}
			}
		}
		// The Default policy's own-proxy switch, either way, and proxies of
		// their own for some.
		exec(`INSERT INTO outbound_proxy_policy (singleton, people_add) VALUES (1, ?) ON CONFLICT (singleton) DO UPDATE SET people_add = excluded.people_add`, r.Intn(2))
		withProxies := map[string]bool{}
		for _, person := range people {
			for n := r.Intn(3); n > 0; n-- {
				exec(`INSERT INTO outbound_proxies (id, name, type, host, port, owner_user_id) VALUES (?, ?, 'socks5', 'proxy.example.test', 1080, ?)`,
					fmt.Sprintf("%s-proxy-%d", person, n), fmt.Sprintf("proxy %d", n), person)
				withProxies[person] = true
			}
		}
		owners, err := OwnProxyOwners(t.Context(), db.Read())
		if err != nil {
			t.Fatal(err)
		}
		if len(owners) != len(withProxies) {
			t.Fatalf("round %d: %d people with proxies of their own, the query found %d: %v", round, len(withProxies), len(owners), owners)
		}
		named := map[string]bool{}
		deciding := map[string]string{}
		passkeys := map[string]bool{}
		rows, err := db.Read().Query(TwoFactorPoliciesSQL())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, policyID, policyName string
			var passkey bool
			if err := rows.Scan(&id, &policyID, &policyName, &passkey); err != nil {
				t.Fatal(err)
			}
			if policyName != policyID {
				t.Fatalf("round %d: %s's policy is called %q", round, policyID, policyName)
			}
			named[id], deciding[id], passkeys[id] = true, policyID, passkey
		}
		rows.Close()
		for _, person := range people {
			effective, err := ForUser(t.Context(), db.Read(), person)
			if err != nil {
				t.Fatal(err)
			}
			resolved := effective.Get(MFARequired)
			want := resolved.Source.Kind == SourcePolicy && resolved.Value == true
			var one, onePasskey bool
			if err := db.Read().QueryRow(TwoFactorForUserSQL(), person, person).Scan(&one, &onePasskey); err != nil {
				t.Fatal(err)
			}
			if named[person] != want || one != want {
				t.Fatalf("round %d, %s: the queries say %v and %v, the resolver %v (%s)", round, person, named[person], one, want, effective.describe())
			}
			if want && deciding[person] != resolved.Source.PolicyID || !want && deciding[person] != "" {
				t.Fatalf("round %d, %s: the query names %q, the resolver %q", round, person, deciding[person], resolved.Source.PolicyID)
			}
			// A passkey counts when the deciding policy says so.
			wantPasskey := false
			if want {
				values, err := Values(t.Context(), db.Read(), resolved.Source.PolicyID)
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range values {
					wantPasskey = wantPasskey || value.Key == MFAPasskeyCounts.Key && string(value.JSON) == "true"
				}
			}
			ownProxies, err := OwnProxiesAllowed(t.Context(), db.Read(), person)
			if err != nil {
				t.Fatal(err)
			}
			if ownProxies != effective.Bool(OwnProxies) {
				t.Fatalf("round %d, %s: own proxies by the query %v, by the resolver %v (%s)", round, person, ownProxies, effective.Bool(OwnProxies), effective.describe())
			}
			if allowed, ok := owners[person]; ok != withProxies[person] || ok && allowed != ownProxies {
				t.Fatalf("round %d, %s: own proxies for everyone %v (listed %v), for one %v", round, person, allowed, ok, ownProxies)
			}
			if passkeys[person] != wantPasskey || onePasskey != wantPasskey {
				t.Fatalf("round %d, %s: a passkey counts by the queries %v and %v, by the deciding policy %v", round, person, passkeys[person], onePasskey, wantPasskey)
			}
		}
	}
}
