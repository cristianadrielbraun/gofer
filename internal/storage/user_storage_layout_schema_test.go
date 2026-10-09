package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutSchemaStepsRunOnceInOrder(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs []int
	step := func(n int, statement string) layoutSchemaStep {
		return func(ctx context.Context, tx *sql.Tx) error {
			runs = append(runs, n)
			_, err := tx.ExecContext(ctx, statement)
			return err
		}
	}
	v1 := []layoutSchemaStep{step(1, `CREATE TABLE layout_probe(id INTEGER PRIMARY KEY)`)}
	v2 := append(v1, step(2, `ALTER TABLE layout_probe ADD COLUMN note TEXT NOT NULL DEFAULT ''`))
	for _, steps := range [][]layoutSchemaStep{v1, v1, v2, v2} {
		if err := ensureLayoutSchema(t.Context(), db, steps); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := db.Read().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatal("layout version", version, err)
	}
	if len(runs) != 2 || runs[0] != 1 || runs[1] != 2 {
		t.Fatal("steps did not run once in order", runs)
	}
	if err := ensureLayoutSchema(t.Context(), db, v1); err == nil || !strings.Contains(err.Error(), "newer Gofer release") {
		t.Fatal("newer layout schema accepted", err)
	}
	// A failing step leaves the version unchanged.
	failing := append(v2, step(3, `ALTER TABLE missing_table ADD COLUMN x TEXT`))
	if err := ensureLayoutSchema(t.Context(), db, failing); err == nil {
		t.Fatal("failing step accepted")
	}
	if err := db.Read().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatal("failed step changed the version", version, err)
	}
}
