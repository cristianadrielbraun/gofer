package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/directory"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var monday = time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC)

// Settings the store tests save, registered under "test." so they don't
// pass for real ones.
var (
	storeFlag     = Bool(Spec{Key: "test.flag", Category: CategoryData, Kind: KindCapability, Label: "x"}, true)
	storeLanguage = Enum(Spec{Key: "test.language", Category: CategoryPreferences, Kind: KindPreference, Label: "x"}, "auto", "auto", "en", "pt-br")
	storeCount    = Int(Spec{Key: "test.count", Category: CategoryMail, Kind: KindLimit, Label: "x"}, 0, 0, 50)
	storeServer   = Bool(Spec{Key: "test.server_wide", Category: CategoryData, Kind: KindPrivacy, InstanceOnly: true, Label: "x"}, false)
)

func newStore(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`INSERT INTO users (id, username, username_normalized, created_at, updated_at) VALUES ('u-ines', 'ines', 'ines', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, user_type, is_admin, created_at, updated_at) VALUES ('u-admin', 'admin', 'admin', 'management', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO users (id, username, username_normalized, status, deletion_pending, created_at, updated_at) VALUES ('u-leaving', 'leaving', 'leaving', 'disabled', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-field', 'Field team', 'field team', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	return db
}

func inTx[T any](t *testing.T, db *storage.DB, change func(context.Context, *sql.Tx) (T, error)) (T, error) {
	t.Helper()
	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	value, err := change(t.Context(), tx)
	if err != nil {
		return value, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return value, nil
}

func create(t *testing.T, db *storage.DB, id, name string) Policy {
	t.Helper()
	policy, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Create(ctx, tx, NewPolicy{ID: id, Name: name, By: "u-admin", At: monday})
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func revision(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	value, err := directory.Revision(t.Context(), db.Read(), directory.RevisionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func order(t *testing.T, db *storage.DB) []string {
	t.Helper()
	policies, err := List(t.Context(), db.Read())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, policy := range policies {
		ids = append(ids, policy.ID)
	}
	return ids
}

func TestTheDefaultPolicyIsAlwaysThereAndLast(t *testing.T) {
	db := newStore(t)
	policy, err := Get(t.Context(), db.Read(), DefaultID)
	if err != nil || !policy.Default || policy.Version != 0 || policy.Priority != 0 {
		t.Fatalf("unsaved Default policy = %#v, %v", policy, err)
	}
	if got := order(t, db); !reflect.DeepEqual(got, []string{DefaultID}) {
		t.Fatalf("policies = %v", got)
	}
	create(t, db, "p-contractors", "Contractors")
	create(t, db, "p-finance", "Finance")
	if got := order(t, db); !reflect.DeepEqual(got, []string{"p-contractors", "p-finance", DefaultID}) {
		t.Fatalf("policies = %v", got)
	}
	for _, refused := range []func(context.Context, *sql.Tx) (any, error){
		func(ctx context.Context, tx *sql.Tx) (any, error) {
			return Update(ctx, tx, PolicyChange{ID: DefaultID, Name: "Everyone", At: monday})
		},
		func(ctx context.Context, tx *sql.Tx) (any, error) { return nil, Delete(ctx, tx, DefaultID, 0) },
		func(ctx context.Context, tx *sql.Tx) (any, error) {
			return Assign(ctx, tx, DefaultID, []Target{{TargetUser, "u-ines"}}, "u-admin", monday)
		},
		func(ctx context.Context, tx *sql.Tx) (any, error) {
			return nil, Reorder(ctx, tx, []string{DefaultID, "p-contractors", "p-finance"})
		},
	} {
		if _, err := inTx(t, db, refused); !errors.Is(err, ErrDefaultPolicy) && !errors.Is(err, ErrOrderInvalid) {
			t.Fatalf("change to the Default policy = %v", err)
		}
	}
	// No policy can take its name.
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Create(ctx, tx, NewPolicy{ID: "p-x", Name: "default  POLICY", At: monday})
	}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a policy named like the Default one = %v", err)
	}

	// Its row is made the first time a value is saved to it.
	saved, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, DefaultID, 0, []ValueChange{{Key: "test.flag", JSON: json.RawMessage(`false`), Enforced: true}}, "u-admin", monday)
	})
	if err != nil || saved.Version != 1 || !saved.Default || saved.CreatedBy != "u-admin" {
		t.Fatalf("saved Default policy = %#v, %v", saved, err)
	}
	if got := order(t, db); !reflect.DeepEqual(got, []string{"p-contractors", "p-finance", DefaultID}) {
		t.Fatalf("policies after saving the Default one = %v", got)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, DefaultID, 0, []ValueChange{{Key: "test.flag", JSON: json.RawMessage(`true`)}}, "u-admin", monday)
	}); !errors.Is(err, ErrStale) {
		t.Fatalf("save from before the Default policy was saved = %v", err)
	}
}

func TestPoliciesAreNamedOrderedAndDeleted(t *testing.T) {
	db := newStore(t)
	contractors := create(t, db, "p-contractors", "  Contractors ")
	create(t, db, "p-finance", "Finance")
	create(t, db, "p-interns", "Interns")
	if contractors.Name != "Contractors" || contractors.Priority != 1 || contractors.Version != 1 || revision(t, db) != 0 {
		t.Fatalf("created = %#v, revision %d", contractors, revision(t, db))
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Create(ctx, tx, NewPolicy{ID: "p-copy", Name: "FINANCE", At: monday})
	}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same name = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Create(ctx, tx, NewPolicy{ID: "p-bad", Name: strings.Repeat("n", 101), At: monday})
	}); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("long name = %v", err)
	}

	renamed, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Update(ctx, tx, PolicyChange{ID: "p-contractors", Version: 1, Name: "Contractors and temps", Description: "Short stays", By: "u-admin", At: monday})
	})
	if err != nil || renamed.Version != 2 || renamed.Description != "Short stays" {
		t.Fatalf("renamed = %#v, %v", renamed, err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return Update(ctx, tx, PolicyChange{ID: "p-contractors", Version: 1, Name: "Temps", At: monday})
	}); !errors.Is(err, ErrStale) {
		t.Fatalf("rename from an old page = %v", err)
	}

	for _, wrong := range [][]string{
		{"p-finance", "p-contractors"},
		{"p-finance", "p-finance", "p-interns"},
		{"p-finance", "p-contractors", "p-gone"},
	} {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) { return nil, Reorder(ctx, tx, wrong) }); !errors.Is(err, ErrOrderInvalid) {
			t.Fatalf("order %v = %v", wrong, err)
		}
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return nil, Reorder(ctx, tx, []string{"p-interns", "p-contractors", "p-finance"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := order(t, db); !reflect.DeepEqual(got, []string{"p-interns", "p-contractors", "p-finance", DefaultID}) || revision(t, db) != 1 {
		t.Fatalf("reordered = %v, revision %d", got, revision(t, db))
	}
	// The same order changes nothing.
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return nil, Reorder(ctx, tx, []string{"p-interns", "p-contractors", "p-finance"})
	}); err != nil || revision(t, db) != 1 {
		t.Fatalf("same order = %v, revision %d", err, revision(t, db))
	}

	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) { return nil, Delete(ctx, tx, "p-contractors", 1) }); !errors.Is(err, ErrStale) {
		t.Fatalf("delete from an old page = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) { return nil, Delete(ctx, tx, "p-contractors", 2) }); err != nil {
		t.Fatal(err)
	}
	policies, err := List(t.Context(), db.Read())
	if err != nil || len(policies) != 3 || policies[0].ID != "p-interns" || policies[0].Priority != 1 || policies[1].ID != "p-finance" || policies[1].Priority != 2 {
		t.Fatalf("after deleting = %#v, %v", policies, err)
	}
	// A new policy goes last, before the Default one.
	if added := create(t, db, "p-night", "Night shift"); added.Priority != 3 {
		t.Fatalf("new policy at %d", added.Priority)
	}
}

func TestValuesAreSavedAsOneChangeFromTheVersionLoaded(t *testing.T) {
	db := newStore(t)
	create(t, db, "p-contractors", "Contractors")
	later := monday.Add(time.Hour)
	saved, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, "p-contractors", 1, []ValueChange{
			{Key: "test.flag", JSON: json.RawMessage(`false`)},
			{Key: "test.language", JSON: json.RawMessage(` "pt-br" `), Locked: true},
			{Key: "test.count", JSON: nil},
		}, "u-admin", later)
	})
	if err != nil || saved.Version != 2 || !saved.UpdatedAt.Equal(later) || revision(t, db) != 1 {
		t.Fatalf("saved = %#v, %v, revision %d", saved, err, revision(t, db))
	}
	values, err := Values(t.Context(), db.Read(), "p-contractors")
	if err != nil || len(values) != 2 || values[0].Key != "test.flag" || string(values[0].JSON) != "false" ||
		values[1].Key != "test.language" || !values[1].Locked || values[1].Enforced {
		t.Fatalf("values = %#v, %v", values, err)
	}
	// The same values, spaced differently, change nothing.
	same, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, "p-contractors", 2, []ValueChange{{Key: "test.language", JSON: json.RawMessage(`"pt-br"`), Locked: true}}, "u-admin", later.Add(time.Hour))
	})
	if err != nil || same.Version != 2 || revision(t, db) != 1 {
		t.Fatalf("unchanged = %#v, %v, revision %d", same, err, revision(t, db))
	}
	cleared, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, "p-contractors", 2, []ValueChange{{Key: "test.flag"}}, "u-admin", later)
	})
	if values, _ := Values(t.Context(), db.Read(), "p-contractors"); err != nil || cleared.Version != 3 || len(values) != 1 {
		t.Fatalf("cleared = %#v, %v, %#v", cleared, err, values)
	}
	for _, test := range []struct {
		change ValueChange
		want   error
	}{
		{ValueChange{Key: "test.flag", JSON: json.RawMessage(`false`), Enforced: true}, ErrEnforcedOutsideDefault},
		{ValueChange{Key: "Mail Export", JSON: json.RawMessage(`false`)}, ErrKeyInvalid},
		{ValueChange{Key: "mail..export", JSON: json.RawMessage(`false`)}, ErrKeyInvalid},
		{ValueChange{Key: "test.flag", JSON: json.RawMessage(`{nope`)}, ErrValueInvalid},
		{ValueChange{Key: "test.flag", JSON: json.RawMessage(`"yes"`)}, ErrValueInvalid},
		{ValueChange{Key: "test.count", JSON: json.RawMessage(`51`)}, ErrValueInvalid},
		{ValueChange{Key: "test.language", JSON: json.RawMessage(`"fr"`)}, ErrValueInvalid},
		{ValueChange{Key: "test.not_declared", JSON: json.RawMessage(`true`)}, ErrUnknownSetting},
		{ValueChange{Key: "test.server_wide", JSON: json.RawMessage(`true`)}, ErrInstanceOnly},
	} {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
			return SetValues(ctx, tx, "p-contractors", 3, []ValueChange{test.change}, "u-admin", later)
		}); !errors.Is(err, test.want) {
			t.Errorf("%+v = %v, want %v", test.change, err, test.want)
		}
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
		return SetValues(ctx, tx, "p-gone", 1, nil, "u-admin", later)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("values for a missing policy = %v", err)
	}
}

func TestPoliciesAreAssignedToGroupsAndPeople(t *testing.T) {
	db := newStore(t)
	create(t, db, "p-contractors", "Contractors")
	create(t, db, "p-finance", "Finance")
	field := Target{TargetGroup, "g-field"}
	ines := Target{TargetUser, "u-ines"}
	added, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]Target, error) {
		return Assign(ctx, tx, "p-contractors", []Target{field, ines, field}, "u-admin", monday)
	})
	if err != nil || !reflect.DeepEqual(added, []Target{field, ines}) || revision(t, db) != 1 {
		t.Fatalf("assigned = %v, %v, revision %d", added, err, revision(t, db))
	}
	if again, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]Target, error) {
		return Assign(ctx, tx, "p-contractors", []Target{ines}, "u-admin", monday)
	}); err != nil || len(again) != 0 || revision(t, db) != 1 {
		t.Fatalf("assigned again = %v, %v", again, err)
	}
	for _, refused := range []Target{{TargetUser, "u-admin"}, {TargetUser, "u-leaving"}, {TargetUser, "u-nobody"}, {TargetGroup, "g-gone"}} {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]Target, error) {
			return Assign(ctx, tx, "p-finance", []Target{field, refused}, "u-admin", monday)
		}); !errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("assign to %v = %v", refused, err)
		}
	}
	if assigned, err := AssignedTo(t.Context(), db.Read(), field); err != nil || len(assigned) != 1 || assigned[0].ID != "p-contractors" {
		t.Fatalf("Field team's policies after refused changes = %#v, %v", assigned, err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]Target, error) {
		return Assign(ctx, tx, "p-finance", []Target{field}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}
	assignments, err := Assignments(t.Context(), db.Read(), "p-contractors")
	if err != nil || len(assignments) != 2 || assignments[0].Target != field || assignments[1].Target != ines || assignments[0].CreatedBy != "u-admin" {
		t.Fatalf("assignments = %#v, %v", assignments, err)
	}

	removed, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]Target, error) {
		return Unassign(ctx, tx, "p-contractors", []Target{ines, ines, {TargetUser, "u-nobody"}})
	})
	if err != nil || !reflect.DeepEqual(removed, []Target{ines}) {
		t.Fatalf("unassigned = %v, %v", removed, err)
	}

	// Deleting the group takes both policies away from it, and nothing else.
	before := revision(t, db)
	count, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return RemoveTarget(ctx, tx, field) })
	if err != nil || count != 2 || revision(t, db) != before+1 {
		t.Fatalf("remove Field team = %d, %v", count, err)
	}
	if policies := order(t, db); len(policies) != 3 {
		t.Fatalf("policies after removing a group = %v", policies)
	}
	if count, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return RemoveTarget(ctx, tx, field) }); err != nil || count != 0 || revision(t, db) != before+1 {
		t.Fatalf("remove again = %d, %v", count, err)
	}
}
