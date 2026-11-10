// Package audit keeps the record of administrative changes: who changed
// what, when, from where, and with what result. Each event is written in
// the transaction that makes the change, so a change that can't be recorded
// doesn't happen, and each is chained to the one before it with an HMAC, so
// removing or editing an event shows when the chain is verified.
//
// It depends on nothing of Gofer's, so auth can record events inside its
// own transactions.
package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ActorKind says who made a change.
type ActorKind string

const (
	// ActorAdmin is an administrator in the admin console.
	ActorAdmin ActorKind = "admin"
	// ActorOperator is someone with access to the server, through the CLI.
	ActorOperator ActorKind = "operator"
	// ActorSystem is Gofer itself, such as retention pruning.
	ActorSystem ActorKind = "system"
)

// Outcome says whether a change happened.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	// OutcomeDenied is an attempt refused for want of a permission or of
	// a recent verification.
	OutcomeDenied Outcome = "denied"
)

// Actor is who made a change. Label is their name when it happened, kept
// so the record still reads after they are renamed or deleted.
type Actor struct {
	Kind      ActorKind
	ID        string
	Label     string
	SessionID string
	TokenID   string
}

// Target is what a change was made to, with its name at the time.
type Target struct {
	Kind  string
	ID    string
	Label string
}

// Change is one field's value before and after.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Client describes where a change came from.
type Client struct {
	UserAgent string `json:"user_agent,omitempty"`
}

// Event is one administrative change, or one refused attempt.
type Event struct {
	// Action names the change: a target kind, a dot, and what happened,
	// such as "user.disabled".
	Action  string
	Actor   Actor
	Target  Target
	Outcome Outcome
	// Changes are the fields the change set, with their earlier values.
	Changes map[string]Change
	// Metadata is anything else worth keeping, such as how many sessions
	// a change signed out.
	Metadata map[string]any
	Client   Client
	// CorrelationID ties together the records of one change, such as its
	// sign-in security event.
	CorrelationID string
}

// Record is an event as stored.
type Record struct {
	Seq        int64
	ID         string
	OccurredAt time.Time
	Event
	ChainMAC string
}

type clientKey struct{}

// maxUserAgent bounds the browser description a record keeps.
const maxUserAgent = 512

// WithClient returns ctx carrying where the changes made under it come
// from, for events that don't say. The description is kept printable and
// short.
func WithClient(ctx context.Context, client Client) context.Context {
	agent := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(client.UserAgent, ""))
	agent = strings.TrimSpace(agent)
	for len(agent) > maxUserAgent {
		_, size := utf8.DecodeLastRuneInString(agent)
		agent = agent[:len(agent)-size]
	}
	client.UserAgent = agent
	return context.WithValue(ctx, clientKey{}, client)
}

// ErrInvalidEvent is an event without an action or an actor.
var ErrInvalidEvent = errors.New("audit: event needs an action and an actor")

// chainKeyLabel separates the chain key from everything else derived from
// the instance secret.
const chainKeyLabel = "gofer/audit/chain/v1"

// DeriveKey derives the key that chains events from the instance secret.
func DeriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(chainKeyLabel))
	return mac.Sum(nil)
}

// Writer records events.
type Writer struct {
	key   []byte
	now   func() time.Time
	newID func() (string, error)
}

// Option changes how a Writer works, for tests.
type Option func(*Writer)

// WithClock makes the Writer read the time from now.
func WithClock(now func() time.Time) Option {
	return func(w *Writer) { w.now = now }
}

// WithIDs makes the Writer name events with newID.
func WithIDs(newID func() (string, error)) Option {
	return func(w *Writer) { w.newID = newID }
}

// NewWriter returns a Writer that chains events with key (DeriveKey).
func NewWriter(key []byte, options ...Option) *Writer {
	w := &Writer{key: append([]byte(nil), key...), now: time.Now, newID: randomID}
	for _, option := range options {
		option(w)
	}
	return w
}

// Key returns the chain key, for verifying.
func (w *Writer) Key() []byte {
	return append([]byte(nil), w.key...)
}

// WriteTx records event inside tx, after the newest event, and returns the
// stored record. Recording in the change's own transaction keeps the two
// together: when either fails, neither stays.
func (w *Writer) WriteTx(ctx context.Context, tx *sql.Tx, event Event) (Record, error) {
	event.Action = strings.TrimSpace(event.Action)
	if event.Action == "" || event.Actor.Kind == "" {
		return Record{}, ErrInvalidEvent
	}
	if event.Outcome == "" {
		event.Outcome = OutcomeSuccess
	}
	if client, ok := ctx.Value(clientKey{}).(Client); ok && event.Client == (Client{}) {
		event.Client = client
	}
	id, err := w.newID()
	if err != nil {
		return Record{}, fmt.Errorf("audit: name event: %w", err)
	}
	record := Record{ID: id, OccurredAt: w.now().UTC().Truncate(time.Microsecond), Event: event}
	stored, err := encodeRecord(record)
	if err != nil {
		return Record{}, err
	}
	previous, err := latestChainMAC(ctx, tx)
	if err != nil {
		return Record{}, err
	}
	record.ChainMAC = chainMAC(w.key, previous, stored)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, occurred_at, actor_kind, actor_id, actor_label, session_id, token_id,
			action, target_kind, target_id, target_label, outcome,
			changes_json, metadata_json, client_json, correlation_id, chain_mac
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stored.ID, record.OccurredAt, stored.ActorKind, stored.ActorID, stored.ActorLabel,
		stored.SessionID, stored.TokenID, stored.Action, stored.TargetKind, stored.TargetID,
		stored.TargetLabel, stored.Outcome, stored.Changes, stored.Metadata, stored.Client,
		stored.CorrelationID, record.ChainMAC,
	)
	if err != nil {
		return Record{}, fmt.Errorf("audit: record %s: %w", event.Action, err)
	}
	if record.Seq, err = result.LastInsertId(); err != nil {
		return Record{}, fmt.Errorf("audit: read event sequence: %w", err)
	}
	return record, nil
}

// storedRecord is a record as its columns hold it. The chain covers these
// exact values, so verifying reads them back without re-encoding.
type storedRecord struct {
	ID            string `json:"id"`
	OccurredAt    string `json:"occurred_at"`
	ActorKind     string `json:"actor_kind"`
	ActorID       string `json:"actor_id"`
	ActorLabel    string `json:"actor_label"`
	SessionID     string `json:"session_id"`
	TokenID       string `json:"token_id"`
	Action        string `json:"action"`
	TargetKind    string `json:"target_kind"`
	TargetID      string `json:"target_id"`
	TargetLabel   string `json:"target_label"`
	Outcome       string `json:"outcome"`
	Changes       string `json:"changes"`
	Metadata      string `json:"metadata"`
	Client        string `json:"client"`
	CorrelationID string `json:"correlation_id"`
}

func encodeRecord(record Record) (storedRecord, error) {
	changes, err := encodeObject(record.Changes)
	if err != nil {
		return storedRecord{}, fmt.Errorf("audit: encode changes: %w", err)
	}
	metadata, err := encodeObject(record.Metadata)
	if err != nil {
		return storedRecord{}, fmt.Errorf("audit: encode metadata: %w", err)
	}
	client, err := json.Marshal(record.Client)
	if err != nil {
		return storedRecord{}, fmt.Errorf("audit: encode client: %w", err)
	}
	return storedRecord{
		ID: record.ID, OccurredAt: canonicalTime(record.OccurredAt),
		ActorKind: string(record.Actor.Kind), ActorID: record.Actor.ID, ActorLabel: record.Actor.Label,
		SessionID: record.Actor.SessionID, TokenID: record.Actor.TokenID,
		Action: record.Action, TargetKind: record.Target.Kind, TargetID: record.Target.ID,
		TargetLabel: record.Target.Label, Outcome: string(record.Outcome),
		Changes: changes, Metadata: metadata, Client: string(client), CorrelationID: record.CorrelationID,
	}, nil
}

// encodeObject writes a map as JSON, an empty object when there's nothing.
// encoding/json sorts the keys, so the same values always give the same
// text.
func encodeObject[V any](values map[string]V) (string, error) {
	if len(values) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(values)
	return string(encoded), err
}

// canonicalTime is the form the chain covers a time in: UTC, microseconds.
func canonicalTime(at time.Time) string {
	return at.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

func chainMAC(key []byte, previous string, record storedRecord) string {
	canonical, _ := json.Marshal(record)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(previous))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

// latestChainMAC is what the next event chains to: the newest event, or the
// checkpoint pruning left when it removed every event, or nothing at all.
func latestChainMAC(ctx context.Context, tx *sql.Tx) (string, error) {
	var mac string
	err := tx.QueryRowContext(ctx, `SELECT chain_mac FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&mac)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT chain_mac FROM audit_checkpoints ORDER BY seq DESC LIMIT 1`).Scan(&mac)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("audit: read the chain: %w", err)
	}
	return mac, nil
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
