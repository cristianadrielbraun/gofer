package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedUserContactSync(t *testing.T, accounts *UserAccountStore, owner, account string) string {
	t.Helper()
	var op string
	if err := accounts.WithAccountForUser(t.Context(), owner, account, func(_ *AccountStore, db *storage.DB) error {
		contact, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-profile", Name: owner, Email: "same@example.com", Phone: owner, GoferSyncEnabled: true, SaveTargets: []string{"book:" + owner + "-book"}})
		if err != nil {
			return err
		}
		if err := db.InitializeContactCanonicalFields(t.Context(), owner, contact.ID, nil); err != nil {
			return err
		}
		op, err = db.EnqueueContactSyncOperation(t.Context(), owner, contact, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return op
}

func userContactSyncFor(t *testing.T, accounts *UserAccountStore, owner, account string) (*UserContactSyncClaim, *UserContactSyncProfile, *UserContactSyncDestination) {
	t.Helper()
	claims, err := accounts.ClaimContactSync(t.Context(), owner, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatal("claim", len(claims), err)
	}
	p, err := accounts.SnapshotContactSync(t.Context(), claims[0])
	if err != nil || p == nil {
		t.Fatal("profile", p, err)
	}
	d, err := accounts.SnapshotContactSyncDestination(t.Context(), p, serviceSnapshotFor(t, accounts, owner, account), owner+"-book")
	if err != nil || d == nil {
		t.Fatal("destination", d, err)
	}
	return claims[0], p, d
}

func TestUserContactSyncOwnerBindingSurvivesCacheEviction(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	ids := map[string]string{}
	for _, owner := range []string{"alice", "bob"} {
		ids[owner] = seedUserContactSync(t, accounts, owner, owners[owner].ID)
	}
	claims := map[string]*UserContactSyncClaim{}
	destinations := map[string]*UserContactSyncDestination{}
	for _, owner := range []string{"alice", "bob"} {
		c, p, d := userContactSyncFor(t, accounts, owner, owners[owner].ID)
		claims[owner], destinations[owner] = c, d
		if c.OperationID() != ids[owner] || c.ContactID() != "same-profile" || p.Contact().Phone != owner {
			t.Fatal("wrong copied owner")
		}
		foreign := "bob"
		if owner == "bob" {
			foreign = "alice"
		}
		if d, err := accounts.SnapshotContactSyncDestination(t.Context(), p, serviceSnapshotFor(t, accounts, foreign, owners[foreign].ID), foreign+"-book"); !errors.Is(err, storage.ErrAccountRoute) || d != nil {
			t.Fatal("foreign services accepted", err)
		}
	}
	// Bob's acquisition evicted Alice. These opaque copies must reacquire the
	// correct store rather than retaining its closed database or trusting IDs.
	for _, owner := range []string{"alice", "bob"} {
		d := destinations[owner]
		if err := accounts.ValidateContactSyncDestination(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if err := accounts.PublishContactSyncResult(t.Context(), d, "https://"+owner+".test/book/shared.vcf", owner); err != nil {
			t.Fatal(err)
		}
		if err := accounts.FinishContactSync(t.Context(), claims[owner], "done", "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			source, err := db.GetContactSource(t.Context(), owner, "same-profile", "carddav", owners[owner].ID)
			if err != nil {
				return err
			}
			if source == nil || source.Etag != owner || source.RemoteID != "https://"+owner+".test/book/shared.vcf" {
				t.Fatal("wrong source", source)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_cards", "contact_fields", "contact_sync_operations", "contact_activity_events"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("central contact copy", table, count, err)
		}
	}
	for _, bad := range []*UserContactSyncClaim{nil, {}, {claim: claims["alice"].claim}, {repository: accounts}} {
		if err := accounts.FinishContactSync(t.Context(), bad, "done", "", time.Time{}); err == nil {
			t.Fatal("unbound claim accepted")
		}
	}
	foreignRepository, err := NewUserAccountStore(accounts.Routing(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := foreignRepository.PublishContactSyncResult(t.Context(), destinations["alice"], "https://alice.test/book/late.vcf", ""); err == nil {
		t.Fatal("snapshot moved to another repository")
	}
}

func TestUserContactSyncFacadeRejectsLateServicesAndLifecycle(t *testing.T) {
	for _, change := range []string{"identity", "endpoint", "cursor", "book", "credentials", "disabled", "deleted-account", "disabled-owner", "deleted-owner", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			seedUserContactSync(t, accounts, "alice", id)
			claim, _, d := userContactSyncFor(t, accounts, "alice", id)
			ctx := t.Context()
			switch change {
			case "deleted-account":
				if err := routing.RequestAccountDeletion(ctx, "alice", id); err != nil {
					t.Fatal(err)
				}
			case "disabled-owner":
				if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "deleted-owner":
				if _, err := system.Write().Exec(`UPDATE users SET status='disabled',user_type='webmail',deletion_pending=1 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			default:
				q := map[string]string{"identity": `UPDATE accounts SET provider_account_id='replacement' WHERE id=?`, "endpoint": `UPDATE account_contact_sync_configs SET base_url='https://changed.test' WHERE account_id=?`, "cursor": `UPDATE account_contact_address_books SET last_sync_token='new' WHERE account_id=?`, "book": `DELETE FROM account_contact_address_books WHERE account_id=?`, "credentials": `UPDATE account_contact_sync_configs SET encrypted_password=x'123456' WHERE account_id=?`, "disabled": `UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id=?`}[change]
				if err := accounts.WithAccountForUser(ctx, "alice", id, func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(q, id); return err }); err != nil {
					t.Fatal(err)
				}
			}
			if err := accounts.ValidateContactSyncDestination(ctx, d); err == nil {
				t.Fatal("late validation passed")
			}
			if err := accounts.PublishContactSyncResult(ctx, d, "https://alice.test/book/late.vcf", "late"); err == nil {
				t.Fatal("late acknowledgement passed")
			}
			if change == "disabled-owner" || change == "deleted-owner" || change == "cancelled" {
				if err := accounts.FinishContactSync(ctx, claim, "done", "", time.Time{}); err == nil {
					t.Fatal("late owner finalization passed")
				}
			}
			if change == "disabled-owner" || change == "deleted-owner" {
				if _, err := system.Write().Exec(`UPDATE users SET status='active',deletion_pending=0 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Fatal("late card committed", n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactSyncFacadeChecksAfterWriterWait(t *testing.T) {
	for _, change := range []string{"profile", "services", "owner", "claim", "completion"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			seedUserContactSync(t, accounts, "alice", id)
			claim, _, d := userContactSyncFor(t, accounts, "alice", id)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				go func() {
					if change == "completion" {
						done <- accounts.FinishContactSync(ctx, claim, "done", "", time.Time{})
					} else {
						done <- accounts.PublishContactSyncResult(ctx, d, "https://alice.test/book/late.vcf", "late")
					}
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
				case "profile":
					_, err = tx.ExecContext(ctx, `UPDATE contact_fields SET value='changed',normalized_value='changed' WHERE user_id='alice' AND source='canonical' AND kind='phone'`)
				case "services":
					_, err = tx.ExecContext(ctx, `UPDATE account_contact_address_books SET url='https://new.test/book' WHERE account_id=?`, id)
				case "owner":
					_, err = system.Write().ExecContext(ctx, `UPDATE users SET status='disabled' WHERE id='alice'`)
				case "claim", "completion":
					_, err = tx.ExecContext(ctx, `UPDATE contact_sync_operations SET attempt_count=attempt_count+1 WHERE id=?`, claim.OperationID())
				}
				if err != nil {
					return err
				}
				return tx.Commit()
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("writer wait bypassed fence")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "owner" {
				if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Fatal("card committed")
				}
				var status string
				if err := db.Read().QueryRow(`SELECT status FROM contact_sync_operations WHERE id=?`, claim.OperationID()).Scan(&status); err != nil {
					return err
				}
				if status != "running" {
					t.Fatal("claim finalized", status)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactSyncClaimRechecksOwnerAfterWriterWait(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	seedUserContactSync(t, accounts, "alice", id)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		go func() {
			claims, err := accounts.ClaimContactSync(ctx, "alice", 1, time.Minute)
			if len(claims) != 0 {
				done <- errors.New("claim escaped after owner disabled")
				return
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
		if _, err := system.Write().ExecContext(ctx, `UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
			return err
		}
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("owner guard bypassed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
		var status string
		var attempt int
		if err := db.Read().QueryRow(`SELECT status,attempt_count FROM contact_sync_operations`).Scan(&status, &attempt); err != nil {
			return err
		}
		if status != "pending" || attempt != 0 {
			t.Fatal("claim committed", status, attempt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
