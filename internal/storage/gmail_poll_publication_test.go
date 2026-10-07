package storage

import (
	"errors"
	"testing"
	"time"
)

func TestOwnedGmailPollPublicationChecksCurrentIdentity(t *testing.T) {
	for _, test := range []struct{ name, sql, owner string }{
		{"valid", "", "alice"}, {"foreign-owner", "", "bob"},
		{"subject", `UPDATE accounts SET provider_account_id='changed'`, "alice"},
		{"provider", `UPDATE accounts SET provider='outlook'`, "alice"},
		{"auth-method", `UPDATE accounts SET auth_method='plain'`, "alice"},
		{"disabled", `UPDATE accounts SET email_sync_enabled=0`, "alice"},
		{"deleting", `UPDATE accounts SET is_deleting=1`, "alice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, r := newAccountRoutingTest(t, 1)
			account := createRoutingTestAccount(t, r, "alice")
			err := r.WithUser(t.Context(), "alice", func(db *DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',provider_account_id='expected',auth_method='oauth2' WHERE id=?`, account.AccountID); err != nil {
					return err
				}
				if test.sql != "" {
					if _, err := db.Write().Exec(test.sql+` WHERE id=?`, account.AccountID); err != nil {
						return err
					}
				}
				state := GmailPollState{AccountID: account.AccountID, ProfileHistoryID: "100"}
				err := db.MarkOwnedGmailPollCheck(t.Context(), test.owner, "expected", state, true, nil)
				if test.name != "valid" {
					if !errors.Is(err, ErrAccountRoute) {
						t.Errorf("stale/foreign publication: %v", err)
					}
					var count int
					if err := db.Read().QueryRow(`SELECT COUNT(*) FROM gmail_poll_state`).Scan(&count); err != nil {
						return err
					}
					if count != 0 {
						t.Error("stale/foreign status was published")
					}
					return nil
				}
				if err != nil {
					return err
				}
				saved, err := db.GetGmailPollState(t.Context(), account.AccountID)
				if err != nil {
					return err
				}
				if saved.ProfileHistoryID != "100" || !saved.LastCheckedAt.Valid || !saved.LastChangedAt.Valid {
					t.Errorf("valid poll state: %+v", saved)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestOwnedGmailPollPublicationRollsBackAndPreservesPriorChange(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	account := createRoutingTestAccount(t, r, "alice")
	err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',provider_account_id='expected',auth_method='oauth2' WHERE id=?`, account.AccountID); err != nil {
			return err
		}
		state := GmailPollState{AccountID: account.AccountID, ProfileHistoryID: "100"}
		if err := db.MarkOwnedGmailPollCheck(t.Context(), "alice", "expected", state, true, nil); err != nil {
			return err
		}
		before, err := db.GetGmailPollState(t.Context(), account.AccountID)
		if err != nil {
			return err
		}
		if _, err := db.Write().Exec(`CREATE TRIGGER fail_owned_poll BEFORE UPDATE ON gmail_poll_state BEGIN SELECT RAISE(ABORT,'fixture publication failure');END`); err != nil {
			return err
		}
		state.ProfileHistoryID = "200"
		state.LastCheckedAt = before.LastCheckedAt
		state.LastCheckedAt.Time = state.LastCheckedAt.Time.Add(time.Second)
		if err := db.MarkOwnedGmailPollCheck(t.Context(), "alice", "expected", state, false, nil); err == nil {
			t.Error("failed publication succeeded")
		}
		after, err := db.GetGmailPollState(t.Context(), account.AccountID)
		if err != nil {
			return err
		}
		if after.ProfileHistoryID != before.ProfileHistoryID || !after.LastCheckedAt.Time.Equal(before.LastCheckedAt.Time) {
			t.Errorf("failed publication changed status: %+v", after)
		}
		if _, err := db.Write().Exec(`DROP TRIGGER fail_owned_poll`); err != nil {
			return err
		}
		if err := db.MarkOwnedGmailPollCheck(t.Context(), "alice", "expected", state, false, nil); err != nil {
			return err
		}
		after, err = db.GetGmailPollState(t.Context(), account.AccountID)
		if err == nil && (!after.LastChangedAt.Time.Equal(before.LastChangedAt.Time) || after.ProfileHistoryID != "200") {
			t.Errorf("retry status: %+v", after)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
