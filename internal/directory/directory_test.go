package directory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var monday = time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC)

func newStore(t *testing.T, people ...string) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, person := range people {
		if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, created_at, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			"u-"+person, person, person); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// inTx runs change in a transaction and commits it when it succeeds.
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

func create(t *testing.T, db *storage.DB, id, name string) Group {
	t.Helper()
	group, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Create(ctx, tx, NewGroup{ID: id, Name: name, Description: "Who covers the " + name, By: "u-admin", At: monday})
	})
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func revision(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	value, err := Revision(t.Context(), db.Read(), RevisionMembership)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestGroupsAreCreatedRenamedAndDeletedFromTheVersionLoaded(t *testing.T) {
	db := newStore(t)
	group := create(t, db, "g-field", "  Field   team ")
	if group.Name != "Field team" || group.Description != "Who covers the   Field   team" || group.Source != SourceLocal ||
		group.Version != 1 || group.CreatedBy != "u-admin" || !group.CreatedAt.Equal(monday) || group.MemberCount != 0 {
		t.Fatalf("created group = %#v", group)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Create(ctx, tx, NewGroup{ID: "g-copy", Name: "FIELD team", At: monday})
	}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same name in other case = %v", err)
	}
	for _, name := range []string{"   ", strings.Repeat("n", NameMaxLength+1), "Night\x00shift"} {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
			return Create(ctx, tx, NewGroup{ID: "g-bad", Name: name, At: monday})
		}); !errors.Is(err, ErrNameInvalid) {
			t.Fatalf("name %q = %v", name, err)
		}
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Create(ctx, tx, NewGroup{ID: "g-bad", Name: "Long", Description: strings.Repeat("d", DescriptionMaxLength+1), At: monday})
	}); !errors.Is(err, ErrDescriptionLong) {
		t.Fatalf("long description = %v", err)
	}
	night := create(t, db, "g-night", "Night shift")

	later := monday.Add(time.Hour)
	renamed, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Update(ctx, tx, GroupChange{ID: group.ID, Version: 1, Name: "Field crew", Description: "Out on the road\nmost days", By: "u-other", At: later})
	})
	if err != nil || renamed.Name != "Field crew" || renamed.Description != "Out on the road\nmost days" || renamed.Version != 2 ||
		renamed.UpdatedBy != "u-other" || !renamed.UpdatedAt.Equal(later) || renamed.CreatedBy != "u-admin" {
		t.Fatalf("renamed group = %#v, %v", renamed, err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Update(ctx, tx, GroupChange{ID: group.ID, Version: 1, Name: "Field people", At: later})
	}); !errors.Is(err, ErrStale) {
		t.Fatalf("rename from an old copy = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Update(ctx, tx, GroupChange{ID: group.ID, Version: 2, Name: "night SHIFT", At: later})
	}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename to another group's name = %v", err)
	}
	same, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Update(ctx, tx, GroupChange{ID: group.ID, Version: 2, Name: "Field crew ", Description: "Out on the road\nmost days", At: later.Add(time.Hour)})
	})
	if err != nil || same.Version != 2 || !same.UpdatedAt.Equal(later) {
		t.Fatalf("unchanged group = %#v, %v", same, err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Group, error) {
		return Update(ctx, tx, GroupChange{ID: "g-gone", Version: 1, Name: "Gone", At: later})
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename a missing group = %v", err)
	}

	listed, err := List(t.Context(), db.Read())
	if err != nil || len(listed) != 2 || listed[0].Name != "Field crew" || listed[1].Name != "Night shift" {
		t.Fatalf("groups = %#v, %v", listed, err)
	}

	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return Delete(ctx, tx, night.ID, 7) }); !errors.Is(err, ErrStale) {
		t.Fatalf("delete from an old copy = %v", err)
	}
	if members, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return Delete(ctx, tx, night.ID, 1) }); err != nil || members != 0 {
		t.Fatalf("delete = %d, %v", members, err)
	}
	if _, err := Get(t.Context(), db.Read(), night.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group = %v", err)
	}
	if revision(t, db) != 0 {
		t.Fatal("deleting an empty group moved the membership revision")
	}
	// The name is free again once the group is gone.
	create(t, db, "g-night-2", "Night Shift")
}

func TestMembersAreAddedAndRemovedOnceEach(t *testing.T) {
	db := newStore(t, "ines", "pavel", "noor")
	field := create(t, db, "g-field", "Field team")
	night := create(t, db, "g-night", "Night shift")

	added, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, field.ID, []string{"u-ines", "u-pavel", "u-ines", ""}, "u-admin", monday)
	})
	if err != nil || !reflect.DeepEqual(added, []string{"u-ines", "u-pavel"}) || revision(t, db) != 1 {
		t.Fatalf("added = %v, %v, revision %d", added, err, revision(t, db))
	}
	added, err = inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, field.ID, []string{"u-pavel", "u-noor"}, "u-other", monday.Add(time.Hour))
	})
	if err != nil || !reflect.DeepEqual(added, []string{"u-noor"}) || revision(t, db) != 2 {
		t.Fatalf("added again = %v, %v, revision %d", added, err, revision(t, db))
	}
	if added, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, field.ID, []string{"u-ines"}, "u-admin", monday)
	}); err != nil || len(added) != 0 || revision(t, db) != 2 {
		t.Fatalf("nobody new = %v, %v, revision %d", added, err, revision(t, db))
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, field.ID, []string{"u-noor", "u-nobody"}, "u-admin", monday)
	}); !errors.Is(err, ErrPersonNotFound) {
		t.Fatalf("add someone missing = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, "g-gone", []string{"u-noor"}, "u-admin", monday)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("add to a missing group = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return AddMembers(ctx, tx, night.ID, []string{"u-ines"}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}

	members, err := Members(t.Context(), db.Read(), field.ID)
	if err != nil || len(members) != 3 || members[0].UserID != "u-ines" || members[0].AddedBy != "u-admin" ||
		members[0].Source != MemberManual || !members[0].AddedAt.Equal(monday) || members[2].UserID != "u-noor" || members[2].AddedBy != "u-other" {
		t.Fatalf("members = %#v, %v", members, err)
	}
	if group, err := Get(t.Context(), db.Read(), field.ID); err != nil || group.MemberCount != 3 || group.Version != 1 {
		t.Fatalf("group with members = %#v, %v", group, err)
	}
	groups, err := GroupsOf(t.Context(), db.Read(), "u-ines")
	if err != nil || len(groups) != 2 || groups[0].ID != field.ID || groups[1].ID != night.ID {
		t.Fatalf("Ines's groups = %#v, %v", groups, err)
	}

	before := revision(t, db)
	removed, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return RemoveMembers(ctx, tx, field.ID, []string{"u-pavel", "u-pavel", "u-nobody"})
	})
	if err != nil || !reflect.DeepEqual(removed, []string{"u-pavel"}) || revision(t, db) != before+1 {
		t.Fatalf("removed = %v, %v", removed, err)
	}
	if removed, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return RemoveMembers(ctx, tx, field.ID, []string{"u-pavel"})
	}); err != nil || len(removed) != 0 || revision(t, db) != before+1 {
		t.Fatalf("remove someone not in it = %v, %v", removed, err)
	}

	left, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return RemovePerson(ctx, tx, "u-ines") })
	if err != nil || left != 2 || revision(t, db) != before+2 {
		t.Fatalf("remove Ines everywhere = %d, %v", left, err)
	}
	if groups, err := GroupsOf(t.Context(), db.Read(), "u-ines"); err != nil || len(groups) != 0 {
		t.Fatalf("Ines's groups after = %#v, %v", groups, err)
	}

	count, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (int, error) { return Delete(ctx, tx, field.ID, 1) })
	if err != nil || count != 1 || revision(t, db) != before+3 {
		t.Fatalf("delete a group with Noor in it = %d, %v", count, err)
	}
	if members, err := Members(t.Context(), db.Read(), field.ID); !errors.Is(err, ErrNotFound) || members != nil {
		t.Fatalf("members of a deleted group = %v, %v", members, err)
	}
}

// A change that fails rolls back with the transaction it was part of.
func TestChangesRollBackWithTheirTransaction(t *testing.T) {
	db := newStore(t, "ines")
	field := create(t, db, "g-field", "Field team")
	failed := errors.New("the audit record couldn't be written")
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		if _, err := AddMembers(ctx, tx, field.ID, []string{"u-ines"}, "u-admin", monday); err != nil {
			return nil, err
		}
		return nil, failed
	}); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if group, err := Get(t.Context(), db.Read(), field.ID); err != nil || group.MemberCount != 0 || revision(t, db) != 0 {
		t.Fatalf("after the rollback = %#v, %v, revision %d", group, err, revision(t, db))
	}
}

func TestRevisionsCountUpByName(t *testing.T) {
	db := newStore(t)
	for range 3 {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (struct{}, error) {
			return struct{}{}, Bump(ctx, tx, RevisionPolicy)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if value, err := Revision(t.Context(), db.Read(), RevisionPolicy); err != nil || value != 3 {
		t.Fatalf("policy revision = %d, %v", value, err)
	}
	if value, err := Revision(t.Context(), db.Read(), RevisionRoles); err != nil || value != 0 {
		t.Fatalf("roles revision = %d, %v", value, err)
	}
	if _, err := Revision(t.Context(), db.Read(), "colors"); !errors.Is(err, ErrRevisionUnknown) {
		t.Fatalf("unknown revision = %v", err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (struct{}, error) {
		return struct{}{}, Bump(ctx, tx, "colors")
	}); !errors.Is(err, ErrRevisionUnknown) {
		t.Fatalf("bump unknown revision = %v", err)
	}
}
