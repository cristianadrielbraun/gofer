package audit

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Format is an export's file format.
type Format string

const (
	FormatCSV   Format = "csv"
	FormatJSONL Format = "jsonl"
)

// exportRecord is one event as exports write it.
type exportRecord struct {
	Seq           int64             `json:"seq"`
	ID            string            `json:"id"`
	OccurredAt    string            `json:"occurred_at"`
	ActorKind     ActorKind         `json:"actor_kind"`
	ActorID       string            `json:"actor_id,omitempty"`
	ActorLabel    string            `json:"actor_label,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	TokenID       string            `json:"token_id,omitempty"`
	Action        string            `json:"action"`
	TargetKind    string            `json:"target_kind,omitempty"`
	TargetID      string            `json:"target_id,omitempty"`
	TargetLabel   string            `json:"target_label,omitempty"`
	Outcome       Outcome           `json:"outcome"`
	Changes       map[string]Change `json:"changes,omitempty"`
	Metadata      map[string]any    `json:"metadata,omitempty"`
	Client        Client            `json:"client"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	ChainMAC      string            `json:"chain_mac"`
}

func exportOf(record Record) exportRecord {
	return exportRecord{
		Seq: record.Seq, ID: record.ID, OccurredAt: record.OccurredAt.UTC().Format(time.RFC3339Nano),
		ActorKind: record.Actor.Kind, ActorID: record.Actor.ID, ActorLabel: record.Actor.Label,
		SessionID: record.Actor.SessionID, TokenID: record.Actor.TokenID,
		Action: record.Action, TargetKind: record.Target.Kind, TargetID: record.Target.ID,
		TargetLabel: record.Target.Label, Outcome: record.Outcome, Changes: record.Changes,
		Metadata: record.Metadata, Client: record.Client, CorrelationID: record.CorrelationID,
		ChainMAC: record.ChainMAC,
	}
}

var csvHeader = []string{
	"seq", "id", "occurred_at", "actor_kind", "actor_id", "actor_label", "session_id", "token_id",
	"action", "target_kind", "target_id", "target_label", "outcome", "changes", "metadata",
	"user_agent", "correlation_id", "chain_mac",
}

// Export writes the events filter keeps to out, newest first, one page at a
// time, so an export of any size is never held in memory. It returns how
// many events it wrote.
func Export(ctx context.Context, q Querier, filter Filter, format Format, out io.Writer) (int64, error) {
	var write func(Record) error
	var finish func() error
	switch format {
	case FormatCSV:
		writer := csv.NewWriter(out)
		if err := writer.Write(csvHeader); err != nil {
			return 0, fmt.Errorf("audit: export: %w", err)
		}
		write = func(record Record) error {
			changes, err := encodeObject(record.Changes)
			if err != nil {
				return err
			}
			metadata, err := encodeObject(record.Metadata)
			if err != nil {
				return err
			}
			line := exportOf(record)
			return writer.Write([]string{
				fmt.Sprint(line.Seq), line.ID, line.OccurredAt, string(line.ActorKind), line.ActorID,
				csvSafe(line.ActorLabel), line.SessionID, line.TokenID, line.Action, line.TargetKind,
				line.TargetID, csvSafe(line.TargetLabel), string(line.Outcome), changes, metadata,
				csvSafe(line.Client.UserAgent), line.CorrelationID, line.ChainMAC,
			})
		}
		finish = func() error {
			writer.Flush()
			return writer.Error()
		}
	case FormatJSONL:
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false)
		write = func(record Record) error { return encoder.Encode(exportOf(record)) }
		finish = func() error { return nil }
	default:
		return 0, fmt.Errorf("audit: unknown export format %q", format)
	}
	var written int64
	cursor := int64(0)
	for {
		page, err := List(ctx, q, filter, cursor, MaxPageSize)
		if err != nil {
			return written, err
		}
		for _, record := range page.Records {
			if err := write(record); err != nil {
				return written, fmt.Errorf("audit: export: %w", err)
			}
			written++
		}
		if page.Next == 0 {
			break
		}
		cursor = page.Next
	}
	if err := finish(); err != nil {
		return written, fmt.Errorf("audit: export: %w", err)
	}
	return written, nil
}

// csvSafe keeps a name that starts like a formula from running as one when
// the export is opened in a spreadsheet.
func csvSafe(value string) string {
	if value != "" && (value[0] == '=' || value[0] == '+' || value[0] == '-' || value[0] == '@' || value[0] == '\t' || value[0] == '\r') {
		return "'" + value
	}
	return value
}
