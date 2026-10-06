package storage

import (
	"errors"
	"strings"
	"testing"
)

func removeUserOAuthGuards(t *testing.T, db *DB) {
	t.Helper()
	for _, table := range []string{"oauth_accounts", "oauth_account_flows"} {
		for _, action := range []string{"insert", "update", "delete"} {
			if _, err := db.Write().Exec(`DROP TRIGGER ` + quoteStoreIdentifier("gofer_store_central_"+table+"_"+action)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestUserOAuthBoundaryRejectsWritesAndUpgradesEmptyStores(t *testing.T) {
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
	alice, err := m.Acquire(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Release()
	db := alice.DB()
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','alice','gmail','alice@mail.test')`); err != nil {
		t.Fatal(err)
	}
	assertBlocked := func(db *DB) {
		t.Helper()
		for _, query := range []string{
			`INSERT INTO oauth_accounts(id,account_id,provider,provider_account_id,access_token) VALUES('grant','account','google','subject','must-not-be-local')`,
			`INSERT INTO oauth_account_flows(state_hash,user_id,session_token_hash,provider,form_data,expires_at) VALUES('state','alice','session','google','{}',CURRENT_TIMESTAMP)`,
		} {
			if _, err := db.Write().Exec(query); err == nil || !strings.Contains(err.Error(), "system records belong") {
				t.Fatalf("local OAuth write not blocked by boundary: %v", err)
			}
		}
	}
	assertBlocked(db)
	removeUserOAuthGuards(t, db)
	alice.Release()
	bob, err := m.Acquire(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob.Release()
	alice, err = m.Acquire(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Release()
	assertBlocked(alice.DB())
}

func TestUserOAuthBoundaryPreservesUnexpectedOldLocalGrants(t *testing.T) {
	for _, table := range []string{"oauth_accounts", "oauth_account_flows"} {
		t.Run(table, func(t *testing.T) {
			m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
			alice, err := m.Acquire(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			defer alice.Release()
			db := alice.DB()
			removeUserOAuthGuards(t, db)
			query := `INSERT INTO oauth_account_flows(state_hash,user_id,session_token_hash,provider,form_data,expires_at) VALUES('old-state','alice','session','google','{}',CURRENT_TIMESTAMP)`
			if table == "oauth_accounts" {
				if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','alice','gmail','alice@mail.test')`); err != nil {
					t.Fatal(err)
				}
				query = `INSERT INTO oauth_accounts(id,account_id,provider,provider_account_id,access_token) VALUES('grant','account','google','subject','preserved-old-grant')`
			}
			if _, err := db.Write().Exec(query); err != nil {
				t.Fatal(err)
			}
			alice.Release()
			bob, err := m.Acquire(t.Context(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			bob.Release()
			if lease, err := m.Acquire(t.Context(), "alice"); !errors.Is(err, ErrUserStoreIdentity) {
				if lease != nil {
					lease.Release()
				}
				t.Fatalf("unexpected local grants admitted: %v", err)
			}
			inspection, err := openReadOnlyDB(m.userPath("alice"))
			if err != nil {
				t.Fatal(err)
			}
			defer inspection.Close()
			var count int
			if err := inspection.QueryRow(`SELECT COUNT(*) FROM ` + quoteStoreIdentifier(table)).Scan(&count); err != nil || count != 1 {
				t.Fatalf("unexpected old records changed: %d %v", count, err)
			}
		})
	}
}
