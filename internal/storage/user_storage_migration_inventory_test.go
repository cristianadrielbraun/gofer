package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserStorageMigrationInventoryCoversActualRuntimeTablesColumnsAndForeignKeys(t *testing.T) {
	system, err := storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if _, err := system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('inventory-owner','inventory-owner','inventory-owner')`); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	work, cancel := context.WithCancel(t.Context())
	defer cancel()
	credentials, err := mailauth.NewUserCredentials(work, &mailauth.Config{}, routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Wait()
	policies := map[string]storage.UserStorageMigrationTable{}
	expectedLinks := map[string]bool{}
	for _, table := range storage.UserStorageMigrationInventory() {
		if _, duplicate := policies[table.Name]; duplicate {
			t.Fatal("duplicate policy", table.Name)
		}
		policies[table.Name] = table
		for _, ref := range table.References {
			if ref.Required {
				expectedLinks[migrationLinkKey(table.Name, ref.Table, ref.Columns, ref.TargetColumns)] = true
			}
		}
	}
	actualTables, actualLinks := map[string]bool{}, map[string]bool{}
	inspect := func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, name := range names {
			policy, known := policies[name]
			if !known {
				t.Fatal("native runtime table has no policy", name)
			}
			actualTables[name] = true
			columns, err := migrationRuntimeColumns(db, name)
			if err != nil {
				return err
			}
			want := append([]string(nil), policy.Columns...)
			sort.Strings(want)
			if !reflect.DeepEqual(columns, want) {
				t.Fatal("native runtime columns differ from policy", name, columns, want)
			}
			links, err := migrationRuntimeLinks(db, name)
			if err != nil {
				return err
			}
			for _, key := range links {
				actualLinks[key] = true
			}
		}
		return nil
	}
	if err := inspect(system); err != nil {
		t.Fatal(err)
	}
	if err := routing.WithUser(t.Context(), "inventory-owner", inspect); err != nil {
		t.Fatal(err)
	}
	// Inspect the identity extension through the actual publication path, not a
	// hand-written schema fixture that could drift from the publisher.
	options, _ := newMigrationStageFixture(t)
	if _, err := storage.StageUserStorageMigration(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	layout, err := storage.PublishUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	published, err := storage.OpenExisting(layout.CentralPath)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if err := inspect(published); err != nil {
		t.Fatal(err)
	}
	for name := range policies {
		if !actualTables[name] && name != "sqlite_stat1" && name != "sqlite_stat4" && policies[name].Destination != "retired-search-index" && policies[name].Destination != "retired-empty-table" {
			t.Fatal("policy not validated against actual runtime", name)
		}
	}
	if !reflect.DeepEqual(actualLinks, expectedLinks) {
		for key := range actualLinks {
			if !expectedLinks[key] {
				t.Error("native FK missing from policy", key)
			}
		}
		for key := range expectedLinks {
			if !actualLinks[key] {
				t.Error("policy FK missing from native runtime", key)
			}
		}
	}
	if len(actualTables) != 95 || len(actualLinks) != 123 {
		t.Fatal("runtime inventory changed", len(actualTables), len(actualLinks))
	}
}

func migrationRuntimeColumns(db *storage.DB, table string) ([]string, error) {
	rows, err := db.Read().Query(`SELECT name FROM pragma_table_xinfo(?) WHERE hidden<>1 ORDER BY name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func migrationRuntimeLinks(db *storage.DB, table string) ([]string, error) {
	type edge struct {
		seq          int
		parent, from string
		to           sql.NullString
	}
	rows, err := db.Read().Query(`SELECT id,seq,"table","from","to" FROM pragma_foreign_key_list(?) ORDER BY id,seq`, table)
	if err != nil {
		return nil, err
	}
	groups := map[int][]edge{}
	for rows.Next() {
		var id int
		var e edge
		if err := rows.Scan(&id, &e.seq, &e.parent, &e.from, &e.to); err != nil {
			rows.Close()
			return nil, err
		}
		groups[id] = append(groups[id], e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var result []string
	for _, group := range groups {
		var from, to []string
		for _, e := range group {
			from = append(from, e.from)
			if e.to.Valid {
				to = append(to, e.to.String)
			} else {
				var key string
				if err := db.Read().QueryRow(`SELECT name FROM pragma_table_info(?) WHERE pk=?`, e.parent, e.seq+1).Scan(&key); err != nil {
					return nil, err
				}
				to = append(to, key)
			}
		}
		result = append(result, migrationLinkKey(table, group[0].parent, from, to))
	}
	return result, nil
}

func migrationLinkKey(child, parent string, from, to []string) string {
	return child + "|" + parent + "|" + strings.Join(from, ",") + "|" + strings.Join(to, ",")
}

func TestUserStorageMigrationInventoryDoesNotExposeMutablePolicies(t *testing.T) {
	first := storage.UserStorageMigrationInventory()
	for i := range first {
		first[i].Name = "changed"
		if len(first[i].Columns) > 0 {
			first[i].Columns[0] = "changed"
		}
		for j := range first[i].References {
			first[i].References[j].Columns[0] = "changed"
			first[i].References[j].TargetColumns[0] = "changed"
		}
	}
	for _, table := range storage.UserStorageMigrationInventory() {
		if table.Name == "changed" {
			t.Fatal("mutable table policy escaped")
		}
		for _, column := range table.Columns {
			if column == "changed" {
				t.Fatal("mutable column policy escaped")
			}
		}
		for _, ref := range table.References {
			if ref.Columns[0] == "changed" || ref.TargetColumns[0] == "changed" {
				t.Fatal("mutable relationship policy escaped")
			}
		}
	}
}
