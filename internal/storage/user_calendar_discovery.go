package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

var ErrCalendarDiscoveryChanged = errors.New("calendar configuration changed during discovery")
var ErrCalendarDiscoveryInvalid = errors.New("invalid discovered calendar sources")

// The immutable catalog excludes visibility and worker timestamps: discovery
// preserves display preferences and does not replace synchronization progress.
type CalendarDiscoverySnapshot struct {
	owner, account, provider string
	catalog                  []CalendarSource
	state                    [32]byte
}

func (s *CalendarDiscoverySnapshot) OwnerID() string   { return s.owner }
func (s *CalendarDiscoverySnapshot) AccountID() string { return s.account }
func (s *CalendarDiscoverySnapshot) Provider() string  { return s.provider }
func (s *CalendarDiscoverySnapshot) Sources() []CalendarSource {
	return append([]CalendarSource(nil), s.catalog...)
}

func calendarDiscoverySnapshotTx(ctx context.Context, tx *sql.Tx, owner, account string, guard func(*sql.Tx, string) error) (*CalendarDiscoverySnapshot, error) {
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	snapshot := &CalendarDiscoverySnapshot{owner: owner, account: account}
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id=? AND user_id=?`, account, owner).Scan(&snapshot.provider); err != nil {
		return nil, err
	}
	if snapshot.provider == "imap" {
		snapshot.provider = CalendarSourceProviderCalDAV
	}
	if snapshot.provider != "gmail" && snapshot.provider != "outlook" && snapshot.provider != CalendarSourceProviderCalDAV {
		return nil, ErrAccountRoute
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,user_id,account_id,provider,remote_id,name,description,timezone,color,access_role,is_primary,is_selected,is_deleted
 FROM calendar_sources WHERE user_id=? AND account_id=? ORDER BY id`, owner, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var source CalendarSource
		var primary, selected, deleted int
		if err := rows.Scan(&source.ID, &source.UserID, &source.AccountID, &source.Provider, &source.RemoteID,
			&source.Name, &source.Description, &source.TimeZone, &source.Color, &source.AccessRole, &primary, &selected, &deleted); err != nil {
			return nil, err
		}
		if source.Provider != snapshot.provider {
			return nil, ErrCalendarDiscoveryChanged
		}
		source.IsPrimary, source.IsSelected, source.IsDeleted = primary == 1, selected == 1, deleted == 1
		snapshot.catalog = append(snapshot.catalog, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	wire, err := json.Marshal(snapshot.catalog)
	if err != nil {
		return nil, err
	}
	snapshot.state = sha256.Sum256(wire)
	return snapshot, nil
}

func (db *DB) SnapshotUserCalendarDiscovery(ctx context.Context, owner, account string, guard func(*sql.Tx, string) error, inspect func(*sql.Tx) error) (*CalendarDiscoverySnapshot, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := calendarDiscoverySnapshotTx(ctx, tx, owner, account, guard)
	if err != nil {
		return nil, err
	}
	if inspect != nil {
		if err := inspect(tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateCalendarDiscoveryTx(ctx context.Context, tx *sql.Tx, snapshot *CalendarDiscoverySnapshot, guard func(*sql.Tx, string) error) error {
	if snapshot == nil || snapshot.owner == "" || snapshot.account == "" || snapshot.provider == "" {
		return ErrCalendarDiscoveryChanged
	}
	current, err := calendarDiscoverySnapshotTx(ctx, tx, snapshot.owner, snapshot.account, guard)
	if err != nil {
		return err
	}
	if current.provider != snapshot.provider || current.state != snapshot.state {
		return ErrCalendarDiscoveryChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarDiscovery(ctx context.Context, snapshot *CalendarDiscoverySnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarDiscoveryTx(ctx, tx, snapshot, guard); err != nil {
		return err
	}
	return tx.Commit()
}

// before/after run in this transaction and must not retain it or call a provider.
// The facade uses them for atomic CalDAV settings and a fresh service snapshot.
func (db *DB) PublishUserCalendarDiscovery(ctx context.Context, snapshot *CalendarDiscoverySnapshot, sources []CalendarSource, guard func(*sql.Tx, string) error, before, after func(*sql.Tx) error) (*CalendarDiscoverySnapshot, error) {
	if snapshot == nil || snapshot.owner == "" || snapshot.account == "" || snapshot.provider == "" || guard == nil {
		return nil, ErrCalendarDiscoveryChanged
	}
	inputs := append([]CalendarSource(nil), sources...)
	for i := range inputs {
		if strings.TrimSpace(inputs[i].RemoteID) == "" {
			return nil, ErrCalendarDiscoveryInvalid
		}
		// Local identity is resolved from the stored account/remote pair or
		// generated internally. Provider/browser fields cannot pick a local ID.
		inputs[i].ID = ""
		inputs[i].UserID, inputs[i].AccountID, inputs[i].Provider = snapshot.owner, snapshot.account, snapshot.provider
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateCalendarDiscoveryTx(ctx, tx, snapshot, guard); err != nil {
		return nil, err
	}
	if before != nil {
		if err := before(tx); err != nil {
			return nil, err
		}
	}
	if err := replaceCalendarSourcesTx(ctx, tx, snapshot.owner, snapshot.account, snapshot.provider, inputs); err != nil {
		return nil, err
	}
	// The publication has intentionally changed service state. Central
	// lifecycle and the old configuration were checked before the first write;
	// the new catalog is read from this same transaction without that old guard.
	next, err := calendarDiscoverySnapshotTx(ctx, tx, snapshot.owner, snapshot.account, func(*sql.Tx, string) error { return nil })
	if err != nil {
		return nil, err
	}
	if err := calendarDiscoveryResult(snapshot, next, inputs); err != nil {
		return nil, err
	}
	var missingState int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sources s WHERE s.user_id=? AND s.account_id=?
 AND NOT EXISTS(SELECT 1 FROM calendar_sync_state state WHERE state.source_id=s.id)`, snapshot.owner, snapshot.account).Scan(&missingState); err != nil {
		return nil, err
	}
	if missingState != 0 {
		return nil, ErrCalendarDiscoveryChanged
	}
	if after != nil {
		if err := after(tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

// Check the committed shape before commit, including ignored writes. A trigger
// must not silently turn a complete discovery into a partially replaced catalog.
func calendarDiscoveryResult(previous, next *CalendarDiscoverySnapshot, inputs []CalendarSource) error {
	expected := make(map[string]CalendarSource, len(previous.catalog)+len(inputs))
	original := make(map[string]CalendarSource, len(previous.catalog))
	for _, source := range previous.catalog {
		original[source.RemoteID] = source
		source.IsDeleted, source.IsSelected = true, false
		expected[source.RemoteID] = source
	}
	for _, input := range inputs {
		input.RemoteID = strings.TrimSpace(input.RemoteID)
		input.Name = strings.TrimSpace(input.Name)
		input.Description = strings.TrimSpace(input.Description)
		input.TimeZone = strings.TrimSpace(input.TimeZone)
		input.Color = strings.TrimSpace(input.Color)
		input.AccessRole = strings.TrimSpace(input.AccessRole)
		// Only provider metadata and default selection affect reconciliation.
		value := CalendarSource{UserID: previous.owner, AccountID: previous.account, Provider: previous.provider,
			RemoteID: input.RemoteID, Name: input.Name, Description: input.Description, TimeZone: input.TimeZone,
			Color: input.Color, AccessRole: input.AccessRole, IsPrimary: input.IsPrimary, IsSelected: input.IsSelected}
		if old, exists := original[input.RemoteID]; exists {
			value.ID, value.IsSelected = old.ID, old.IsSelected
		}
		expected[input.RemoteID] = value
	}
	if len(expected) != len(next.catalog) {
		return ErrCalendarDiscoveryChanged
	}
	for _, actual := range next.catalog {
		want, ok := expected[actual.RemoteID]
		if !ok || actual.ID == "" {
			return ErrCalendarDiscoveryChanged
		}
		if want.ID == "" {
			want.ID = actual.ID
		}
		if !reflect.DeepEqual(actual, want) {
			return ErrCalendarDiscoveryChanged
		}
	}
	return nil
}
