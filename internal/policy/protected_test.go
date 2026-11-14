package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

func TestAGroupIsProtectedWhenItsPoliciesSetSecurity(t *testing.T) {
	if SetsSecurity([]Value{value(storeFlag.Key, "true"), value(SecurityRetentionDays.Key, "30"), value("gone.setting", "1")}) {
		t.Fatal("a capability, a whole-server setting and an undeclared one count as security")
	}
	if !SetsSecurity([]Value{value(storeFlag.Key, "true"), value(OwnProxies.Key, "true")}) {
		t.Fatal("people's own proxies isn't a security setting")
	}

	db := newStore(t)
	if _, err := db.Write().Exec(`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-office', 'Office', 'office', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	secure := create(t, db, "p-secure", "Secure")
	plain := create(t, db, "p-plain", "Plain")
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		if _, err := SetValues(ctx, tx, secure.ID, secure.Version, []ValueChange{{Key: MFARequired.Key, JSON: json.RawMessage(`true`)}}, "u-admin", monday); err != nil {
			return Policy{}, err
		}
		if _, err := SetValues(ctx, tx, plain.ID, plain.Version, []ValueChange{{Key: storeFlag.Key, JSON: json.RawMessage(`false`)}}, "u-admin", monday); err != nil {
			return Policy{}, err
		}
		// Secure reaches the field team, and Ines directly; Plain the office.
		if _, err := Assign(ctx, tx, secure.ID, []Target{{Kind: TargetGroup, ID: "g-field"}, {Kind: TargetUser, ID: "u-ines"}}, "u-admin", monday); err != nil {
			return Policy{}, err
		}
		_, err := Assign(ctx, tx, plain.ID, []Target{{Kind: TargetGroup, ID: "g-office"}}, "u-admin", monday)
		return Policy{}, err
	}); err != nil {
		t.Fatal(err)
	}
	protected, err := ProtectedGroups(t.Context(), db.Read())
	if err != nil || !reflect.DeepEqual(protected, map[string]bool{"g-field": true}) {
		t.Fatalf("protected = %#v, %v", protected, err)
	}
}
