package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactSuppressionWriterWaitRechecksOwnerAndReadsCurrentPolicy(t *testing.T) {
	for _, change := range []string{"disabled", "deleting", "policy"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, _ := newServiceAccountFixture(t)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				if _, err := db.SaveContactProfile(t.Context(), "alice", models.ContactProfile{ID: "observed-profile", PrimaryEmail: "observed@example.com", Fields: []models.ContactField{{Kind: "email", Value: "observed@example.com", Source: "observed"}}}); err != nil {
					return err
				}
				return db.UpsertObservedContact(t.Context(), "alice", "Observed", "observed@example.com", time.Now())
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				type result struct {
					count int64
					err   error
				}
				done := make(chan result, 1)
				go func() { count, err := accounts.DeleteObservedContacts(ctx, "alice"); done <- result{count, err} }()
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
				case "deleting":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`)
				case "policy":
					_, err = tx.Exec(`INSERT INTO app_settings(user_id,key,value) VALUES('alice','ui_settings','{"contacts_prevent_recreate_deleted":"false"}')`)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case got := <-done:
					if change == "policy" {
						if got.err != nil || got.count != 1 {
							t.Fatal("writer used stale policy", got)
						}
					} else if !errors.Is(got.err, storage.ErrUserStoreOwner) || got.count != 0 {
						t.Fatal("writer missed owner transition", got)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var deleted, observations int
				if err := db.Read().QueryRow(`SELECT is_deleted FROM contact_profiles WHERE id='observed-profile'`).Scan(&deleted); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_observations`).Scan(&observations); err != nil {
					return err
				}
				wantDeleted, wantObservations := 0, 1
				if change == "policy" {
					wantDeleted, wantObservations = 1, 0
				}
				if deleted != wantDeleted || observations != wantObservations {
					t.Fatal("wrong policy/owner result", deleted, observations)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
