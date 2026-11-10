package audit_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type fixture struct {
	db     *storage.DB
	writer *audit.Writer
	now    time.Time
	ids    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{db: db, now: time.Date(2026, time.November, 10, 6, 0, 0, 0, time.UTC)}
	f.writer = audit.NewWriter(audit.DeriveKey([]byte("audit-test-secret-of-32-bytes!!!")),
		audit.WithClock(func() time.Time { return f.now }),
		audit.WithIDs(func() (string, error) { f.ids++; return fmt.Sprintf("event-%03d", f.ids), nil }),
	)
	return f
}

// write records event in its own transaction, as a change would.
func (f *fixture) write(t *testing.T, event audit.Event) audit.Record {
	t.Helper()
	tx, err := f.db.Write().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := f.writer.WriteTx(context.Background(), tx, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	return record
}

func disabled(target string) audit.Event {
	return audit.Event{
		Action:  "user.disabled",
		Actor:   audit.Actor{Kind: audit.ActorAdmin, ID: "admin-1", Label: "ada.admin", SessionID: "session-1"},
		Target:  audit.Target{Kind: "user", ID: target, Label: target},
		Changes: map[string]audit.Change{"status": {From: "active", To: "disabled"}},
		Metadata: map[string]any{
			"revoked_sessions": 2,
		},
		Client: audit.Client{UserAgent: "Sample Browser 1.0"},
	}
}

func (f *fixture) verify(t *testing.T) audit.Verification {
	t.Helper()
	result, err := audit.Verify(context.Background(), f.db.Read(), f.writer.Key())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestEventsChainAndVerify(t *testing.T) {
	f := newFixture(t)
	if result := f.verify(t); !result.OK() || result.Checked != 0 {
		t.Fatalf("empty log = %+v", result)
	}
	var records []audit.Record
	for _, target := range []string{"milo.rivers", "nora.vale", "ivo.stone", "pia.lund"} {
		records = append(records, f.write(t, disabled(target)))
	}
	if records[0].Seq == 0 || records[1].Seq <= records[0].Seq || records[0].ChainMAC == records[1].ChainMAC {
		t.Fatalf("records = %+v", records)
	}
	result := f.verify(t)
	if !result.OK() || result.Checked != 4 || result.FirstSeq != records[0].Seq || result.LastSeq != records[3].Seq {
		t.Fatalf("verification = %+v", result)
	}

	// A different key doesn't verify the chain.
	other, err := audit.Verify(context.Background(), f.db.Read(), audit.DeriveKey([]byte("another secret")))
	if err != nil || other.OK() || other.BrokenAt != records[0].Seq {
		t.Fatalf("verification with another key = %+v, %v", other, err)
	}
}

func TestVerifyFindsEditedAndRemovedEvents(t *testing.T) {
	for name, tamper := range map[string]struct {
		statement string
		brokenAt  int
	}{
		"edited label":      {`UPDATE audit_events SET target_label = 'someone.else' WHERE seq = 2`, 2},
		"edited change":     {`UPDATE audit_events SET changes_json = '{"status":{"from":"active","to":"active"}}' WHERE seq = 3`, 3},
		"edited time":       {`UPDATE audit_events SET occurred_at = '2026-01-01 00:00:00+00:00' WHERE seq = 2`, 2},
		"removed middle":    {`DELETE FROM audit_events WHERE seq = 2`, 3},
		"removed beginning": {`DELETE FROM audit_events WHERE seq = 1`, 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			for _, target := range []string{"milo.rivers", "nora.vale", "ivo.stone", "pia.lund"} {
				f.write(t, disabled(target))
			}
			if _, err := f.db.Write().Exec(tamper.statement); err != nil {
				t.Fatal(err)
			}
			if result := f.verify(t); result.OK() || result.BrokenAt != int64(tamper.brokenAt) {
				t.Fatalf("verification = %+v, want broken at %d", result, tamper.brokenAt)
			}
		})
	}
}

func TestPruneLeavesAVerifiableChain(t *testing.T) {
	f := newFixture(t)
	for index := 0; index < 5; index++ {
		f.write(t, disabled(fmt.Sprintf("person-%d", index)))
	}
	cutoff := f.now.Add(-150 * time.Second) // the first three are older
	prune := func(batch int) int64 {
		tx, err := f.db.Write().Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		deleted, err := audit.Prune(context.Background(), tx, cutoff, batch, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return deleted
	}
	if deleted := prune(2); deleted != 2 {
		t.Fatalf("first batch deleted %d", deleted)
	}
	if result := f.verify(t); !result.OK() || result.Checked != 3 || result.FirstSeq != 3 {
		t.Fatalf("after one batch = %+v", result)
	}
	if deleted := prune(2); deleted != 1 {
		t.Fatalf("second batch deleted %d", deleted)
	}
	if deleted := prune(2); deleted != 0 {
		t.Fatalf("nothing left to prune, deleted %d", deleted)
	}
	// New events keep chaining after pruning, even once every event is gone.
	cutoff = f.now.Add(time.Hour)
	if deleted := prune(10); deleted != 2 {
		t.Fatalf("pruning everything deleted %d", deleted)
	}
	f.write(t, disabled("after-pruning"))
	if result := f.verify(t); !result.OK() || result.Checked != 1 {
		t.Fatalf("after pruning everything = %+v", result)
	}
	var checkpoints int
	if err := f.db.Read().QueryRow(`SELECT COUNT(*) FROM audit_checkpoints WHERE reason = 'retention'`).Scan(&checkpoints); err != nil || checkpoints != 3 {
		t.Fatalf("checkpoints = %d, %v", checkpoints, err)
	}
}

func TestPruneStopsAtTheFirstEventItMustKeep(t *testing.T) {
	f := newFixture(t)
	f.write(t, disabled("old"))
	f.now = f.now.Add(48 * time.Hour)
	f.write(t, disabled("new"))
	f.now = f.now.Add(-72 * time.Hour) // a clock set back
	f.write(t, disabled("written-after-but-dated-before"))
	tx, err := f.db.Write().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	deleted, err := audit.Prune(context.Background(), tx, f.now.Add(24*time.Hour), 10, f.now)
	if err != nil || deleted != 1 {
		t.Fatalf("prune = %d, %v; only the first event may go", deleted, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if result := f.verify(t); !result.OK() || result.Checked != 2 {
		t.Fatalf("verification = %+v", result)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	f := newFixture(t)
	f.write(t, disabled("milo.rivers"))
	f.write(t, audit.Event{
		Action: "invitation.revoked", Actor: audit.Actor{Kind: audit.ActorAdmin, ID: "admin-2", Label: "bo.admin"},
		Target: audit.Target{Kind: "user", ID: "nora.vale", Label: "nora.vale"},
	})
	f.write(t, audit.Event{
		Action: "user.disabled", Actor: audit.Actor{Kind: audit.ActorAdmin, ID: "admin-1"},
		Target: audit.Target{Kind: "user", ID: "ivo.stone"}, Outcome: audit.OutcomeDenied,
	})
	f.write(t, audit.Event{Action: "audit.pruned", Actor: audit.Actor{Kind: audit.ActorSystem}})
	ctx := context.Background()
	count := func(filter audit.Filter) int {
		page, err := audit.List(ctx, f.db.Read(), filter, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Records)
	}
	for name, test := range map[string]struct {
		filter audit.Filter
		want   int
	}{
		"everything":     {audit.Filter{}, 4},
		"exact action":   {audit.Filter{Action: "user.disabled"}, 2},
		"action prefix":  {audit.Filter{Action: "user."}, 2},
		"other prefix":   {audit.Filter{Action: "invitation."}, 1},
		"actor":          {audit.Filter{ActorID: "admin-1"}, 2},
		"target":         {audit.Filter{TargetKind: "user", TargetID: "nora.vale"}, 1},
		"denied":         {audit.Filter{Outcome: audit.OutcomeDenied}, 1},
		"since":          {audit.Filter{Since: f.now.Add(-150 * time.Second)}, 2},
		"until":          {audit.Filter{Until: f.now.Add(-150 * time.Second)}, 2},
		"no such action": {audit.Filter{Action: "group."}, 0},
	} {
		if got := count(test.filter); got != test.want {
			t.Errorf("%s: %d events, want %d", name, got, test.want)
		}
	}

	page, err := audit.List(ctx, f.db.Read(), audit.Filter{}, 0, 3)
	if err != nil || len(page.Records) != 3 || page.Next == 0 || page.Records[0].Action != "audit.pruned" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	rest, err := audit.List(ctx, f.db.Read(), audit.Filter{}, page.Next, 3)
	if err != nil || len(rest.Records) != 1 || rest.Next != 0 || rest.Records[0].Target.ID != "milo.rivers" {
		t.Fatalf("second page = %+v, %v", rest, err)
	}
	first := rest.Records[0]
	if first.Changes["status"].To != "disabled" || first.Metadata["revoked_sessions"] != float64(2) ||
		first.Client.UserAgent != "Sample Browser 1.0" || first.Actor.Label != "ada.admin" || !first.OccurredAt.Equal(time.Date(2026, time.November, 10, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("stored record = %+v", first)
	}
}

func TestExportWritesEveryEvent(t *testing.T) {
	f := newFixture(t)
	f.write(t, disabled("milo.rivers"))
	formula := disabled("=HYPERLINK(\"https://attacker.example\")")
	f.write(t, formula)

	var out bytes.Buffer
	written, err := audit.Export(context.Background(), f.db.Read(), audit.Filter{}, audit.FormatCSV, &out)
	if err != nil || written != 2 {
		t.Fatalf("csv export = %d, %v", written, err)
	}
	rows, err := csv.NewReader(&out).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][0] != "seq" || rows[0][8] != "action" {
		t.Fatalf("csv rows = %q, %v", rows, err)
	}
	if label := rows[1][11]; !strings.HasPrefix(label, "'=") {
		t.Fatalf("a formula in a name was exported as %q", label)
	}

	out.Reset()
	written, err = audit.Export(context.Background(), f.db.Read(), audit.Filter{Action: "user."}, audit.FormatJSONL, &out)
	if err != nil || written != 2 {
		t.Fatalf("jsonl export = %d, %v", written, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var decoded map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &decoded) != nil || decoded["action"] != "user.disabled" || decoded["chain_mac"] == "" {
		t.Fatalf("jsonl = %q", out.String())
	}
	if _, err := audit.Export(context.Background(), f.db.Read(), audit.Filter{}, "xml", &out); err == nil {
		t.Fatal("unknown format exported")
	}
}

func TestWriteBelongsToTheChangesTransaction(t *testing.T) {
	f := newFixture(t)
	tx, err := f.db.Write().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.writer.WriteTx(context.Background(), tx, disabled("milo.rivers")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := f.db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("a rolled-back change left %d events, %v", events, err)
	}
	tx, err = f.db.Write().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, event := range []audit.Event{{Actor: audit.Actor{Kind: audit.ActorAdmin}}, {Action: "user.disabled"}} {
		if _, err := f.writer.WriteTx(context.Background(), tx, event); !errors.Is(err, audit.ErrInvalidEvent) {
			t.Fatalf("WriteTx(%+v) = %v", event, err)
		}
	}
}

// The chain must not depend on how a map was built: the same values give
// the same stored text.
func TestChangesEncodeTheSameWayEveryTime(t *testing.T) {
	f := newFixture(t)
	event := disabled("milo.rivers")
	event.Changes = map[string]audit.Change{"z": {From: 1, To: 2}, "a": {From: "x", To: "y"}, "m": {From: nil, To: true}}
	record := f.write(t, event)
	var stored string
	if err := f.db.Read().QueryRow(`SELECT changes_json FROM audit_events WHERE seq = ?`, record.Seq).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != `{"a":{"from":"x","to":"y"},"m":{"from":null,"to":true},"z":{"from":1,"to":2}}` {
		t.Fatalf("changes stored as %s", stored)
	}
	if result := f.verify(t); !result.OK() {
		t.Fatalf("verification = %+v", result)
	}
}

var _ = sql.ErrNoRows

func TestWithClientKeepsTheBrowserPrintableAndShort(t *testing.T) {
	f := newFixture(t)
	ctx := audit.WithClient(context.Background(), audit.Client{UserAgent: "Sample\x00Browser\n" + strings.Repeat("é", 400)})
	tx, err := f.db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := f.writer.WriteTx(ctx, tx, disabledWithoutClient("milo.rivers"))
	if err != nil {
		t.Fatal(err)
	}
	agent := record.Client.UserAgent
	if !strings.HasPrefix(agent, "Sample Browser ") || len(agent) > 512 || strings.ContainsAny(agent, "\x00\n") {
		t.Fatalf("stored browser = %q (%d bytes)", agent, len(agent))
	}
	// An event that names its own client keeps it.
	event := disabled("nora.vale")
	own, err := f.writer.WriteTx(ctx, tx, event)
	if err != nil || own.Client.UserAgent != "Sample Browser 1.0" {
		t.Fatalf("event's own client = %+v, %v", own.Client, err)
	}
}

func disabledWithoutClient(target string) audit.Event {
	event := disabled(target)
	event.Client = audit.Client{}
	return event
}
