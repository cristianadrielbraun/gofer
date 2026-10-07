package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUserMailOperationsProviderDraftRetryPreservesReconciliation(t *testing.T) {
	db := seedUserProviderDraft(t)
	if _, err := publishUserProviderDraft(t, db, "original"); err != nil {
		t.Fatal(err)
	}
	op := claimUserProviderDraft(t, db)
	if err := db.BeginUserProviderDraftCreate(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUserProviderDraftCandidate(t.Context(), op, "confirmed-draft", "confirmed-message"); err != nil {
		t.Fatal(err)
	}
	if err := db.FailUserProviderDraft(t.Context(), op, "ambiguous", "access_token=private-secret", time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("provider_draft:%d", op.ID)
	view, err := db.GetMailOperationForUser(t.Context(), "alice", id)
	if err != nil || !view.CanReconcile || !view.Ambiguous || view.CanRetry || strings.Contains(view.LastError, "private-secret") {
		t.Fatal("unsafe provider diagnostics", view, err)
	}
	if _, err := db.RetryMailOperationForUser(t.Context(), "bob", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign retry", err)
	}
	view, err = db.RetryMailOperationForUser(t.Context(), "alice", id)
	if err != nil || view.State != "ambiguous" || view.Attempts != op.AttemptCount || view.NextRetryAt.After(time.Now().Add(time.Second)) {
		t.Fatal("retry reset reconciliation", view, err)
	}
	next := claimUserProviderDraft(t, db)
	if !next.CreateAttempted || next.CandidateID != "confirmed-draft" || next.CandidateMessageID != "confirmed-message" || next.RevisionToken != op.RevisionToken || string(next.MIMEData) != string(op.MIMEData) || next.AttemptCount != op.AttemptCount+1 {
		t.Fatal("retry lost original publication evidence", next)
	}
	if _, err := db.RetryMailOperationForUser(t.Context(), "alice", id); !errors.Is(err, ErrMailOperationNotRetryable) {
		t.Fatal("in-flight operation retried", err)
	}
}

func TestUserMailOperationsProviderDraftRejectsChangedPrincipal(t *testing.T) {
	for _, change := range []string{"provider_account_id='other'", "provider='outlook'", "auth_method='plain'", "is_deleting=1"} {
		t.Run(change, func(t *testing.T) {
			db := seedUserProviderDraft(t)
			if _, err := publishUserProviderDraft(t, db, "original"); err != nil {
				t.Fatal(err)
			}
			op := claimUserProviderDraft(t, db)
			if err := db.FailUserProviderDraft(t.Context(), op, "blocked", "unavailable", time.Now().Add(time.Hour), true); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec("UPDATE accounts SET " + change); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("provider_draft:%d", op.ID)
			view, err := db.GetMailOperationForUser(t.Context(), "alice", id)
			if err != nil || view.CanRetry || view.CanReconcile {
				t.Fatal("changed principal advertised retry", view, err)
			}
			if _, err := db.RetryMailOperationForUser(t.Context(), "alice", id); !errors.Is(err, ErrMailOperationNotRetryable) {
				t.Fatal("changed principal retried", err)
			}
		})
	}
}

func TestUserMailOperationsLateGuardRollsBackRetry(t *testing.T) {
	db := seedUserProviderDraft(t)
	if _, err := publishUserProviderDraft(t, db, "original"); err != nil {
		t.Fatal(err)
	}
	op := claimUserProviderDraft(t, db)
	if err := db.FailUserProviderDraft(t.Context(), op, "blocked", "unavailable", time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("provider_draft:%d", op.ID)
	before, err := db.GetMailOperationForUser(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	guard := func(*sql.Tx) error {
		calls++
		if calls == 2 {
			return ErrUserStoreOwner
		}
		return nil
	}
	if _, err := db.RetryMailOperationForUserGuarded(t.Context(), "alice", id, guard); !errors.Is(err, ErrUserStoreOwner) || calls != 2 {
		t.Fatal("late guard bypassed", calls, err)
	}
	after, err := db.GetMailOperationForUser(t.Context(), "alice", id)
	if err != nil || before != after {
		t.Fatal("rejected retry committed", before, after, err)
	}
	if view, err := db.RetryMailOperationForUser(t.Context(), "alice", id); err != nil || view.State != "failed" {
		t.Fatal("same-principal blocked retry unavailable", view, err)
	}
}
