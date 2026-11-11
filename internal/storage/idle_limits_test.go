package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestIdleConnectionLimitIsKeptPerAccount(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, found, err := db.GetIdleConnectionLimit(ctx, "acct"); err != nil || found {
		t.Fatalf("before anything was learned: found %v, err %v", found, err)
	}
	failed := time.Date(2026, 11, 10, 8, 30, 0, 0, time.UTC)
	stable := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	want := models.IdleConnectionLimit{Limit: 3, FailedAt: 5, FailedTime: failed, StableAt: 3, StableSince: stable}
	if err := db.SaveIdleConnectionLimit(ctx, "acct", want); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetIdleConnectionLimit(ctx, "acct")
	if err != nil || !found || got.Limit != 3 || got.FailedAt != 5 || !got.FailedTime.Equal(failed) || got.StableAt != 3 || !got.StableSince.Equal(stable) {
		t.Fatalf("got %+v, found %v, err %v; want %+v", got, found, err, want)
	}
	want.Limit, want.StableSince = 2, time.Time{}
	if err := db.SaveIdleConnectionLimit(ctx, "acct", want); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.GetIdleConnectionLimit(ctx, "acct"); got.Limit != 2 || !got.StableSince.IsZero() {
		t.Fatalf("after an update got %+v, want limit 2 and no stable time", got)
	}
	// An account deleted meanwhile is not brought back.
	if err := db.SaveIdleConnectionLimit(ctx, "gone", want); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := db.GetIdleConnectionLimit(ctx, "gone"); found {
		t.Fatal("saved a limit for an account that doesn't exist")
	}
}

func TestIdleFolderRanksPutInboxFirstThenTheSidebarOrder(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`UPDATE folders SET sort_order = CASE id WHEN 'box-inbox' THEN 0 WHEN 'box-archive' THEN 4 ELSE 6 END;
		INSERT INTO folders(id, account_id, remote_id, name, role, sort_order) VALUES
			('box-garden', 'acct', 'Garden', 'Garden', 'custom', 100),
			('box-books', 'acct', 'Books', 'Books', 'custom', 101);
		INSERT INTO folders(id, account_id, parent_id, remote_id, name, role, sort_order) VALUES
			('box-seeds', 'acct', 'box-garden', 'Garden/Seeds', 'Seeds', 'custom', 102),
			('box-tools', 'acct', 'box-garden', 'Garden/Tools', 'Tools', 'custom', 103)`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderOrder(ctx, "reader", "acct", []string{"box-books", "box-garden", "box-inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderOrder(ctx, "reader", "acct", []string{"box-tools", "box-seeds"}); err != nil {
		t.Fatal(err)
	}
	ranks, err := db.IdleFolderRanks(ctx, "reader", "acct")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"box-inbox", "box-books", "box-garden", "box-tools", "box-seeds", "box-archive", "box-trash"}
	for i := 1; i < len(order); i++ {
		if ranks[order[i-1]] >= ranks[order[i]] {
			t.Fatalf("ranks %v: %s should come before %s", ranks, order[i-1], order[i])
		}
	}
	if ranks["box-inbox"] != 0 {
		t.Fatalf("Inbox rank = %d, want 0", ranks["box-inbox"])
	}
}
