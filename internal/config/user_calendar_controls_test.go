package config

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newCalendarControlFixture(t *testing.T) (*storage.DB, *storage.AccountRouting, *UserAccountStore, map[string]*models.Account) {
	t.Helper()
	system, routing, accounts, owners := newServiceAccountFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			return db.ReplaceCalendarSources(t.Context(), owner, owners[owner].ID, "caldav", []storage.CalendarSource{
				{ID: "same-source", RemoteID: "/primary/", Name: owner + " primary", IsSelected: true},
				{ID: "second-source", RemoteID: "/second/", Name: owner + " secondary"},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return system, routing, accounts, owners
}

func TestUserCalendarControlsWriterWaitRechecksLifecycleAndSource(t *testing.T) {
	for _, operation := range []string{"selection", "visibility", "enablement"} {
		for _, change := range []string{"disabled", "owner-deleting", "account-deleting", "local-account-deleting", "source-deleted"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				system, _, accounts, owners := newCalendarControlFixture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
					tx, err := db.Write().BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					before := db.Write().Stats().WaitCount
					done := make(chan error, 1)
					go func() {
						var err error
						switch operation {
						case "selection":
							err = accounts.SetCalendarSourcesSelected(ctx, "alice", owners["alice"].ID, []string{"same-source", "second-source"})
						case "visibility":
							err = accounts.SetCalendarSourceVisible(ctx, "alice", "same-source", false)
						case "enablement":
							err = accounts.SetCalendarServiceEnabled(ctx, "alice", owners["alice"].ID, true)
						}
						done <- err
					}()
					tick := time.NewTicker(time.Millisecond)
					defer tick.Stop()
					for db.Write().Stats().WaitCount == before {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-tick.C:
						}
					}
					switch change {
					case "disabled":
						_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					case "owner-deleting":
						_, err = system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`)
					case "account-deleting":
						_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
					case "local-account-deleting":
						_, err = tx.Exec(`UPDATE accounts SET is_deleting=1 WHERE id=?`, owners["alice"].ID)
					case "source-deleted":
						_, err = tx.Exec(`UPDATE calendar_sources SET is_deleted=1,is_selected=0 WHERE id='same-source'`)
					}
					if err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					select {
					case err := <-done:
						// Service enablement acts on the current remaining live set.
						if change == "source-deleted" && operation == "enablement" {
							if err != nil {
								t.Fatal("remaining sources not enabled", err)
							}
						} else if err == nil {
							t.Fatal("late calendar control missed transition")
						} else if change == "disabled" || change == "owner-deleting" {
							if !errors.Is(err, storage.ErrUserStoreOwner) {
								t.Fatal("owner transition classification", err)
							}
						}
					case <-ctx.Done():
						return ctx.Err()
					}
					var hidden, selected int
					if err := db.Read().QueryRow(`SELECT is_hidden FROM calendar_sources WHERE id='same-source'`).Scan(&hidden); err != nil {
						return err
					}
					if err := db.Read().QueryRow(`SELECT is_selected FROM calendar_sources WHERE id='second-source'`).Scan(&selected); err != nil {
						return err
					}
					wantSelected := 0
					if change == "source-deleted" && operation == "enablement" {
						wantSelected = 1
					}
					if hidden != 0 || selected != wantSelected {
						t.Fatal("failed control leaked a preference", hidden, selected)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarControlsAtomicFailureForeignAccountAndMissingGuard(t *testing.T) {
	_, routing, accounts, owners := newCalendarControlFixture(t)
	if err := accounts.SetCalendarSourcesSelected(t.Context(), "alice", owners["bob"].ID, []string{"same-source"}); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign calendar account accepted", err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		for _, operation := range []func() error{
			func() error {
				return db.SetUserCalendarSourceSelection(t.Context(), "alice", owners["alice"].ID, nil, nil)
			},
			func() error {
				return db.SetUserCalendarSourceVisibility(t.Context(), "alice", "same-source", false, nil)
			},
			func() error {
				return db.SetUserCalendarServiceEnabled(t.Context(), "alice", owners["alice"].ID, false, nil)
			},
		} {
			if err := operation(); !errors.Is(err, storage.ErrAccountRoute) {
				t.Fatal("owned control accepted missing authority", err)
			}
		}
		_, err := db.Write().Exec(`CREATE TRIGGER fail_second_selection BEFORE UPDATE OF is_selected ON calendar_sources
 WHEN NEW.id='second-source' AND NEW.is_selected=1 BEGIN SELECT RAISE(ABORT,'synthetic selection fault'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SetCalendarSourcesSelected(t.Context(), "alice", owners["alice"].ID, []string{"second-source"}); err == nil {
		t.Fatal("selection fault not reached")
	}
	if err := accounts.SetCalendarServiceEnabled(t.Context(), "alice", owners["alice"].ID, true); err == nil {
		t.Fatal("enablement fault not reached")
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			sources, err := db.ListSelectedCalendarSources(t.Context(), owner)
			if err == nil && (len(sources) != 1 || sources[0].ID != "same-source") {
				return errors.New("partial selection escaped or crossed owner")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER fail_second_selection`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SetCalendarSourcesSelected(t.Context(), "alice", owners["alice"].ID, []string{"second-source"}); err != nil {
		t.Fatal("selection retry failed", err)
	}
	if err := accounts.SetCalendarSourceVisible(t.Context(), "alice", "same-source", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unselected source visibility accepted", err)
	}
}
