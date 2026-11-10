package audit

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Querier is what reading needs: a database or a transaction.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Filter narrows a listing. Empty fields don't narrow it.
type Filter struct {
	// Action is an exact action ("user.disabled") or a kind of target
	// followed by a dot ("user.") for all of its actions.
	Action     string
	ActorID    string
	TargetKind string
	TargetID   string
	Outcome    Outcome
	Since      time.Time
	Until      time.Time
}

// Page is one page of a listing, newest first. Next is the cursor for the
// page after it, or 0 when there's none.
type Page struct {
	Records []Record
	Next    int64
}

// MaxPageSize bounds a page.
const MaxPageSize = 500

const recordColumns = `seq, id, occurred_at, actor_kind, actor_id, actor_label, session_id, token_id,
	action, target_kind, target_id, target_label, outcome, changes_json, metadata_json,
	client_json, correlation_id, chain_mac`

// List returns the events filter keeps, newest first, from cursor on (0
// for the start), at most limit of them.
func List(ctx context.Context, q Querier, filter Filter, cursor int64, limit int) (Page, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}
	where, args := filter.where()
	if cursor > 0 {
		where = append(where, "seq < ?")
		args = append(args, cursor)
	}
	query := `SELECT ` + recordColumns + ` FROM audit_events`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("audit: list events: %w", err)
	}
	defer rows.Close()
	var page Page
	for rows.Next() {
		record, _, err := scanRecord(rows)
		if err != nil {
			return Page{}, err
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("audit: list events: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.Next = page.Records[limit-1].Seq
	}
	return page, nil
}

func (filter Filter) where() ([]string, []any) {
	var where []string
	var args []any
	if action := strings.TrimSpace(filter.Action); action != "" {
		if strings.HasSuffix(action, ".") {
			where = append(where, "substr(action, 1, ?) = ?")
			args = append(args, len(action), action)
		} else {
			where = append(where, "action = ?")
			args = append(args, action)
		}
	}
	if filter.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, filter.ActorID)
	}
	if filter.TargetKind != "" {
		where = append(where, "target_kind = ?")
		args = append(args, filter.TargetKind)
	}
	if filter.TargetID != "" {
		where = append(where, "target_id = ?")
		args = append(args, filter.TargetID)
	}
	if filter.Outcome != "" {
		where = append(where, "outcome = ?")
		args = append(args, string(filter.Outcome))
	}
	if !filter.Since.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, filter.Since.UTC())
	}
	if !filter.Until.IsZero() {
		where = append(where, "occurred_at < ?")
		args = append(args, filter.Until.UTC())
	}
	return where, args
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanRecord reads a row in recordColumns order, returning the record and
// its columns as the chain covers them.
func scanRecord(row rowScanner) (Record, storedRecord, error) {
	var record Record
	var stored storedRecord
	var occurredAt time.Time
	if err := row.Scan(
		&record.Seq, &stored.ID, &occurredAt, &stored.ActorKind, &stored.ActorID, &stored.ActorLabel,
		&stored.SessionID, &stored.TokenID, &stored.Action, &stored.TargetKind, &stored.TargetID,
		&stored.TargetLabel, &stored.Outcome, &stored.Changes, &stored.Metadata, &stored.Client,
		&stored.CorrelationID, &record.ChainMAC,
	); err != nil {
		return Record{}, storedRecord{}, fmt.Errorf("audit: read event: %w", err)
	}
	stored.OccurredAt = canonicalTime(occurredAt)
	record.ID = stored.ID
	record.OccurredAt = occurredAt.UTC()
	record.Action = stored.Action
	record.Actor = Actor{Kind: ActorKind(stored.ActorKind), ID: stored.ActorID, Label: stored.ActorLabel, SessionID: stored.SessionID, TokenID: stored.TokenID}
	record.Target = Target{Kind: stored.TargetKind, ID: stored.TargetID, Label: stored.TargetLabel}
	record.Outcome = Outcome(stored.Outcome)
	record.CorrelationID = stored.CorrelationID
	if err := json.Unmarshal([]byte(stored.Changes), &record.Changes); err != nil {
		return Record{}, storedRecord{}, fmt.Errorf("audit: event %d changes: %w", record.Seq, err)
	}
	if err := json.Unmarshal([]byte(stored.Metadata), &record.Metadata); err != nil {
		return Record{}, storedRecord{}, fmt.Errorf("audit: event %d metadata: %w", record.Seq, err)
	}
	if err := json.Unmarshal([]byte(stored.Client), &record.Client); err != nil {
		return Record{}, storedRecord{}, fmt.Errorf("audit: event %d client: %w", record.Seq, err)
	}
	return record, stored, nil
}

// Verification is what Verify found.
type Verification struct {
	// Checked counts the events verified, from FirstSeq to LastSeq.
	Checked  int64
	FirstSeq int64
	LastSeq  int64
	// BrokenAt is the first event whose chain doesn't hold, or 0.
	BrokenAt int64
}

// OK reports whether every event verified.
func (v Verification) OK() bool { return v.BrokenAt == 0 }

// Verify walks every event, oldest first, and checks each one's chain value
// against the one before it. An edited, inserted or removed event breaks
// the chain from there on. Removing the newest events leaves no trace in
// the chain itself: exports and streaming are the record of those.
func Verify(ctx context.Context, q Querier, key []byte) (Verification, error) {
	var result Verification
	previous := ""
	next := int64(0)
	first := true
	for {
		rows, err := q.QueryContext(ctx, `SELECT `+recordColumns+` FROM audit_events WHERE seq > ? ORDER BY seq ASC LIMIT ?`, next, 1000)
		if err != nil {
			return result, fmt.Errorf("audit: verify: %w", err)
		}
		count := 0
		for rows.Next() {
			record, stored, err := scanRecord(rows)
			if err != nil {
				rows.Close()
				return result, err
			}
			count++
			next = record.Seq
			if first {
				first = false
				result.FirstSeq = record.Seq
				// The oldest event chains to the checkpoint pruning left
				// below it, or to nothing when it was the first.
				previous, err = checkpointBelow(ctx, q, record.Seq)
				if err != nil {
					rows.Close()
					return result, err
				}
			}
			if !hmac.Equal([]byte(chainMAC(key, previous, stored)), []byte(record.ChainMAC)) {
				rows.Close()
				result.BrokenAt = record.Seq
				return result, nil
			}
			previous = record.ChainMAC
			result.Checked++
			result.LastSeq = record.Seq
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, fmt.Errorf("audit: verify: %w", err)
		}
		if count == 0 {
			return result, nil
		}
	}
}

func checkpointBelow(ctx context.Context, q Querier, seq int64) (string, error) {
	var mac string
	err := q.QueryRowContext(ctx, `SELECT chain_mac FROM audit_checkpoints WHERE seq < ? ORDER BY seq DESC LIMIT 1`, seq).Scan(&mac)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("audit: read checkpoint: %w", err)
	}
	return mac, nil
}

// Prune removes events older than cutoff, oldest first, at most batch of
// them, and says how many it removed. It only ever removes the start of
// the chain, stopping at the first event that isn't older than cutoff,
// even if a later one is (a clock set back), and it first records a
// checkpoint for the newest event it removes, so the chain still verifies
// from the events left. Run it until it removes fewer than batch.
func Prune(ctx context.Context, tx *sql.Tx, cutoff time.Time, batch int, now time.Time) (int64, error) {
	if batch <= 0 || batch > 1000 {
		batch = 1000
	}
	var lastSeq int64
	var lastMAC string
	err := tx.QueryRowContext(ctx, `
		SELECT seq, chain_mac FROM (
			SELECT seq, chain_mac FROM audit_events
			WHERE seq < COALESCE((SELECT MIN(seq) FROM audit_events WHERE occurred_at >= ?), 9223372036854775807)
			ORDER BY seq ASC LIMIT ?
		) ORDER BY seq DESC LIMIT 1`, cutoff.UTC(), batch).Scan(&lastSeq, &lastMAC)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("audit: find events to prune: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_checkpoints (seq, chain_mac, created_at, reason) VALUES (?, ?, ?, 'retention')`,
		lastSeq, lastMAC, now.UTC(),
	); err != nil {
		return 0, fmt.Errorf("audit: record checkpoint: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE seq <= ?`, lastSeq)
	if err != nil {
		return 0, fmt.Errorf("audit: prune events: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit: count pruned events: %w", err)
	}
	return deleted, nil
}
