// Package policy decides the settings that apply to each person: the
// Default policy for everyone, and named policies, applied in priority
// order to the groups and people they're assigned to.
//
// This file is the store: policies, the values each sets, and who each is
// assigned to. Every write takes the transaction of the change it belongs
// to, so the service that checks permissions and records the audit event
// commits them together, and moves the policy revision on, so what was
// worked out from the policies can tell it's out of date. Like directory,
// it depends on nothing else of Gofer's.
package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/directory"
)

// DefaultID is the Default policy's ID. It always applies, to everyone,
// after every other policy. Its row is made the first time a value is
// saved to it; until then it sets nothing.
const DefaultID = "default"

// defaultName is the Default policy's stored name. The console shows it
// in the reader's language.
const defaultName = "Default policy"

// TargetKind is what a policy is assigned to.
type TargetKind string

const (
	TargetGroup TargetKind = "group"
	TargetUser  TargetKind = "user"
)

// keyMaxLength bounds a setting key.
const keyMaxLength = 128

var (
	ErrNotFound = errors.New("the policy doesn't exist")
	// ErrStale is a change made from an outdated copy of the policy.
	ErrStale           = errors.New("the policy changed since it was loaded")
	ErrNameTaken       = errors.New("another policy has that name")
	ErrNameInvalid     = errors.New("the policy name is empty, too long or has control characters")
	ErrDescriptionLong = errors.New("the policy description is too long or has control characters")
	// ErrDefaultPolicy is a change the Default policy doesn't take: it
	// can't be renamed, deleted, moved or assigned.
	ErrDefaultPolicy = errors.New("the Default policy can't change that way")
	// ErrEnforcedOutsideDefault: only the Default policy enforces values.
	ErrEnforcedOutsideDefault = errors.New("only the Default policy enforces a value")
	// ErrOrderInvalid is an order that doesn't name every policy once.
	ErrOrderInvalid   = errors.New("the order must name every policy once")
	ErrTargetNotFound = errors.New("the group or person doesn't exist, or can't have a policy")
	ErrKeyInvalid     = errors.New("the setting key is empty, too long or has unexpected characters")
	// ErrAdapted is a value saved to the Default policy for a setting that
	// lives elsewhere, which is changed where it lives.
	ErrAdapted      = errors.New("this setting is changed where it lives, not on the Default policy")
	ErrValueInvalid = errors.New("the setting value isn't JSON")
	errMissingID    = errors.New("a policy needs an ID")
	errMissingTime  = errors.New("a change needs a time")
	errUnknownKind  = errors.New("unknown assignment target")
)

// Querier reads: a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Policy is one policy. Priority is its place in the order, 1 first; the
// Default policy has none and comes last.
type Policy struct {
	ID          string
	Name        string
	Description string
	Default     bool
	Priority    int
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string
	// Version goes up with each change to the policy or its values; a
	// change names the version it was made from. A Default policy that
	// was never saved is version 0.
	Version int64
}

// Value is one setting a policy sets.
type Value struct {
	Key  string
	JSON json.RawMessage
	// Locked: people can't change it for themselves (preferences).
	Locked bool
	// Enforced: no other policy can change it (Default policy only).
	Enforced  bool
	UpdatedAt time.Time
	UpdatedBy string
}

// ValueChange sets one setting on a policy, or clears it when JSON is nil.
type ValueChange struct {
	Key      string
	JSON     json.RawMessage
	Locked   bool
	Enforced bool
}

// Target is a group or a person a policy is assigned to.
type Target struct {
	Kind TargetKind
	ID   string
}

// Assignment is a policy's assignment to a target.
type Assignment struct {
	Target
	CreatedAt time.Time
	CreatedBy string
}

// NewPolicy is a policy to create, last in the order before the Default
// policy. ID comes from the caller; By is the user ID of who creates it.
type NewPolicy struct {
	ID          string
	Name        string
	Description string
	By          string
	At          time.Time
}

// PolicyChange renames a policy or changes its description, from Version.
type PolicyChange struct {
	ID          string
	Version     int64
	Name        string
	Description string
	By          string
	At          time.Time
}

// unsavedDefault is the Default policy before anything is saved to it.
func unsavedDefault() Policy {
	return Policy{ID: DefaultID, Name: defaultName, Default: true}
}

// Get loads one policy. The Default policy is always there.
func Get(ctx context.Context, q Querier, id string) (Policy, error) {
	policy, err := scanPolicy(q.QueryRowContext(ctx, policySelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		if id == DefaultID {
			return unsavedDefault(), nil
		}
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, fmt.Errorf("load policy: %w", err)
	}
	return policy, nil
}

// List returns every policy in priority order, the Default policy last.
func List(ctx context.Context, q Querier) ([]Policy, error) {
	rows, err := q.QueryContext(ctx, policySelect+` ORDER BY is_default, priority`)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	defer rows.Close()
	var policies []Policy
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("read policy: %w", err)
		}
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(policies) == 0 || !policies[len(policies)-1].Default {
		policies = append(policies, unsavedDefault())
	}
	return policies, nil
}

// Create adds a policy, last in the order before the Default policy.
func Create(ctx context.Context, tx *sql.Tx, policy NewPolicy) (Policy, error) {
	if policy.ID == "" || policy.ID == DefaultID {
		return Policy{}, errMissingID
	}
	if policy.At.IsZero() {
		return Policy{}, errMissingTime
	}
	name, description, err := cleanNameAndDescription(policy.Name, policy.Description)
	if err != nil {
		return Policy{}, err
	}
	if err := nameFree(ctx, tx, name, ""); err != nil {
		return Policy{}, err
	}
	var last int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(priority), 0) FROM policies`).Scan(&last); err != nil {
		return Policy{}, fmt.Errorf("find the last policy: %w", err)
	}
	at := policy.At.UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO policies (id, name, name_normalized, description, is_default, priority, created_at, created_by, updated_at, updated_by, version)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?, 1)`,
		policy.ID, name, directory.NormalizeName(name), description, last+1, at, policy.By, at, policy.By,
	); err != nil {
		return Policy{}, fmt.Errorf("create policy: %w", err)
	}
	// A policy assigned to nobody changes nobody's settings: the revision
	// stays.
	return Get(ctx, tx, policy.ID)
}

// Update renames a policy or changes its description. Nothing changes,
// and the version stays, when both are what they were.
func Update(ctx context.Context, tx *sql.Tx, change PolicyChange) (Policy, error) {
	if change.At.IsZero() {
		return Policy{}, errMissingTime
	}
	if change.ID == DefaultID {
		return Policy{}, ErrDefaultPolicy
	}
	current, err := Get(ctx, tx, change.ID)
	if err != nil {
		return Policy{}, err
	}
	if current.Version != change.Version {
		return Policy{}, ErrStale
	}
	name, description, err := cleanNameAndDescription(change.Name, change.Description)
	if err != nil {
		return Policy{}, err
	}
	if name == current.Name && description == current.Description {
		return current, nil
	}
	if err := nameFree(ctx, tx, name, change.ID); err != nil {
		return Policy{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE policies SET name = ?, name_normalized = ?, description = ?, updated_at = ?, updated_by = ?, version = version + 1
		WHERE id = ? AND version = ?`,
		name, directory.NormalizeName(name), description, change.At.UTC(), change.By, change.ID, change.Version)
	if err != nil {
		return Policy{}, fmt.Errorf("update policy: %w", err)
	}
	if err := oneRow(result); err != nil {
		return Policy{}, err
	}
	return Get(ctx, tx, change.ID)
}

// Delete removes a policy, its values and its assignments, from the
// version that was loaded, and closes the gap it leaves in the order.
func Delete(ctx context.Context, tx *sql.Tx, id string, version int64) error {
	if id == DefaultID {
		return ErrDefaultPolicy
	}
	current, err := Get(ctx, tx, id)
	if err != nil {
		return err
	}
	if current.Version != version {
		return ErrStale
	}
	for _, statement := range []string{
		`DELETE FROM policy_values WHERE policy_id = ?`,
		`DELETE FROM policy_assignments WHERE policy_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return fmt.Errorf("delete the policy's values and assignments: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM policies WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if err := oneRow(result); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE policies SET priority = priority - 1 WHERE priority > ?`, current.Priority); err != nil {
		return fmt.Errorf("close the gap in the policy order: %w", err)
	}
	return directory.Bump(ctx, tx, directory.RevisionPolicy)
}

// Reorder puts the policies in the order ids gives, which names every
// policy but the Default one exactly once.
func Reorder(ctx context.Context, tx *sql.Tx, ids []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, priority FROM policies WHERE is_default = 0`)
	if err != nil {
		return fmt.Errorf("list the policy order: %w", err)
	}
	current := map[string]int{}
	for rows.Next() {
		var id string
		var priority int
		if err := rows.Scan(&id, &priority); err != nil {
			rows.Close()
			return fmt.Errorf("read the policy order: %w", err)
		}
		current[id] = priority
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) != len(current) {
		return ErrOrderInvalid
	}
	seen := map[string]bool{}
	changed := false
	for i, id := range ids {
		priority, found := current[id]
		if !found || seen[id] {
			return ErrOrderInvalid
		}
		seen[id] = true
		changed = changed || priority != i+1
	}
	if !changed {
		return nil
	}
	// Out of the way first, so no two policies share a place on the way.
	if _, err := tx.ExecContext(ctx, `UPDATE policies SET priority = priority + ? WHERE is_default = 0`, len(ids)); err != nil {
		return fmt.Errorf("move the policies: %w", err)
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE policies SET priority = ? WHERE id = ?`, i+1, id); err != nil {
			return fmt.Errorf("move policy: %w", err)
		}
	}
	return directory.Bump(ctx, tx, directory.RevisionPolicy)
}

// Values returns the settings a policy sets, by key.
func Values(ctx context.Context, q Querier, id string) ([]Value, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT setting_key, value_json, locked, enforced, updated_at, updated_by
		FROM policy_values WHERE policy_id = ? ORDER BY setting_key`, id)
	if err != nil {
		return nil, fmt.Errorf("list policy values: %w", err)
	}
	defer rows.Close()
	var values []Value
	for rows.Next() {
		var value Value
		var raw string
		if err := rows.Scan(&value.Key, &raw, &value.Locked, &value.Enforced, &value.UpdatedAt, &value.UpdatedBy); err != nil {
			return nil, fmt.Errorf("read policy value: %w", err)
		}
		value.JSON = json.RawMessage(raw)
		values = append(values, value)
	}
	return values, rows.Err()
}

// SetValues saves changes to a policy's settings, from the version that
// was loaded, as one change. Each must be a declared setting with a value
// that checks; whole-server settings go on the Default policy only, and
// settings that live elsewhere are changed there, not on it. Saving to the Default policy makes its row
// the first time. A change that changes nothing keeps the version.
func SetValues(ctx context.Context, tx *sql.Tx, id string, version int64, changes []ValueChange, by string, at time.Time) (Policy, error) {
	if at.IsZero() {
		return Policy{}, errMissingTime
	}
	current, err := Get(ctx, tx, id)
	if err != nil {
		return Policy{}, err
	}
	if current.Version != version {
		return Policy{}, ErrStale
	}
	for _, change := range changes {
		if !validKey(change.Key) {
			return Policy{}, ErrKeyInvalid
		}
		setting, declared := Lookup(change.Key)
		switch {
		case !declared:
			return Policy{}, ErrUnknownSetting
		case setting.InstanceOnly && !current.Default:
			return Policy{}, ErrInstanceOnly
		case setting.Adapted() && current.Default:
			return Policy{}, ErrAdapted
		case change.Enforced && !current.Default:
			return Policy{}, ErrEnforcedOutsideDefault
		}
		if change.JSON != nil {
			if !json.Valid(change.JSON) {
				return Policy{}, ErrValueInvalid
			}
		}
	}
	// Values are stored as the setting writes them, so equal ones compare
	// equal.
	changes = slices.Clone(changes)
	for i, change := range changes {
		if change.JSON == nil {
			continue
		}
		setting, _ := Lookup(change.Key)
		canonical, err := setting.canonical(change.JSON)
		if err != nil {
			return Policy{}, err
		}
		changes[i].JSON = canonical
	}
	at = at.UTC()
	if current.Version == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policies (id, name, name_normalized, is_default, priority, created_at, created_by, updated_at, updated_by, version)
			VALUES (?, ?, ?, 1, NULL, ?, ?, ?, ?, 1)`,
			DefaultID, defaultName, directory.NormalizeName(defaultName), at, by, at, by,
		); err != nil {
			return Policy{}, fmt.Errorf("save the Default policy: %w", err)
		}
	}
	existing, err := Values(ctx, tx, id)
	if err != nil {
		return Policy{}, err
	}
	stored := make(map[string]Value, len(existing))
	for _, value := range existing {
		stored[value.Key] = value
	}
	changed := false
	for _, change := range changes {
		old, had := stored[change.Key]
		if change.JSON == nil {
			if !had {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM policy_values WHERE policy_id = ? AND setting_key = ?`, id, change.Key); err != nil {
				return Policy{}, fmt.Errorf("clear policy value: %w", err)
			}
			delete(stored, change.Key)
			changed = true
			continue
		}
		if had && sameJSON(old.JSON, change.JSON) && old.Locked == change.Locked && old.Enforced == change.Enforced {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policy_values (policy_id, setting_key, value_json, locked, enforced, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (policy_id, setting_key) DO UPDATE SET
				value_json = excluded.value_json, locked = excluded.locked, enforced = excluded.enforced,
				updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
			id, change.Key, string(change.JSON), change.Locked, change.Enforced, at, by,
		); err != nil {
			return Policy{}, fmt.Errorf("save policy value: %w", err)
		}
		stored[change.Key] = Value{Key: change.Key, JSON: change.JSON, Locked: change.Locked, Enforced: change.Enforced}
		changed = true
	}
	if !changed {
		return Get(ctx, tx, id)
	}
	if current.Version > 0 {
		result, err := tx.ExecContext(ctx, `UPDATE policies SET updated_at = ?, updated_by = ?, version = version + 1 WHERE id = ? AND version = ?`,
			at, by, id, current.Version)
		if err != nil {
			return Policy{}, fmt.Errorf("update policy: %w", err)
		}
		if err := oneRow(result); err != nil {
			return Policy{}, err
		}
	}
	if err := directory.Bump(ctx, tx, directory.RevisionPolicy); err != nil {
		return Policy{}, err
	}
	return Get(ctx, tx, id)
}

// Assignments returns who a policy is assigned to: groups, then people.
func Assignments(ctx context.Context, q Querier, id string) ([]Assignment, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT target_kind, target_id, created_at, created_by FROM policy_assignments
		WHERE policy_id = ? ORDER BY target_kind, target_id`, id)
	if err != nil {
		return nil, fmt.Errorf("list policy assignments: %w", err)
	}
	defer rows.Close()
	var assignments []Assignment
	for rows.Next() {
		var assignment Assignment
		if err := rows.Scan(&assignment.Kind, &assignment.ID, &assignment.CreatedAt, &assignment.CreatedBy); err != nil {
			return nil, fmt.Errorf("read policy assignment: %w", err)
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

// AssignedTo returns the policies assigned directly to target, in
// priority order.
func AssignedTo(ctx context.Context, q Querier, target Target) ([]Policy, error) {
	rows, err := q.QueryContext(ctx, policySelect+`
		WHERE id IN (SELECT policy_id FROM policy_assignments WHERE target_kind = ? AND target_id = ?)
		ORDER BY priority`, string(target.Kind), target.ID)
	if err != nil {
		return nil, fmt.Errorf("list the policies assigned to a %s: %w", target.Kind, err)
	}
	defer rows.Close()
	var policies []Policy
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("read policy: %w", err)
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// Assign gives a policy to groups or people. One it has already is
// skipped; a group that doesn't exist, or someone who isn't a webmail user
// or is being deleted, refuses the whole change. It returns the targets it
// added, in the order given.
func Assign(ctx context.Context, tx *sql.Tx, id string, targets []Target, by string, at time.Time) ([]Target, error) {
	if at.IsZero() {
		return nil, errMissingTime
	}
	if id == DefaultID {
		return nil, ErrDefaultPolicy
	}
	if _, err := Get(ctx, tx, id); err != nil {
		return nil, err
	}
	var added []Target
	for _, target := range uniqueTargets(targets) {
		if err := requireTarget(ctx, tx, target); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at, created_by)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (policy_id, target_kind, target_id) DO NOTHING`,
			id, string(target.Kind), target.ID, at.UTC(), by)
		if err != nil {
			return nil, fmt.Errorf("assign policy: %w", err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if count == 1 {
			added = append(added, target)
		}
	}
	if len(added) > 0 {
		if err := directory.Bump(ctx, tx, directory.RevisionPolicy); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// Unassign takes a policy away from groups or people. One it doesn't have
// is skipped. It returns the targets it removed, in the order given.
func Unassign(ctx context.Context, tx *sql.Tx, id string, targets []Target) ([]Target, error) {
	if id == DefaultID {
		return nil, ErrDefaultPolicy
	}
	if _, err := Get(ctx, tx, id); err != nil {
		return nil, err
	}
	var removed []Target
	for _, target := range uniqueTargets(targets) {
		result, err := tx.ExecContext(ctx, `DELETE FROM policy_assignments WHERE policy_id = ? AND target_kind = ? AND target_id = ?`,
			id, string(target.Kind), target.ID)
		if err != nil {
			return nil, fmt.Errorf("unassign policy: %w", err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if count == 1 {
			removed = append(removed, target)
		}
	}
	if len(removed) > 0 {
		if err := directory.Bump(ctx, tx, directory.RevisionPolicy); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// RemoveTarget takes every policy away from a group or a person, as the
// group or the person is deleted, in the same transaction. It returns how
// many policies they had.
func RemoveTarget(ctx context.Context, tx *sql.Tx, target Target) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM policy_assignments WHERE target_kind = ? AND target_id = ?`, string(target.Kind), target.ID)
	if err != nil {
		return 0, fmt.Errorf("remove a %s's policies: %w", target.Kind, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if err := directory.Bump(ctx, tx, directory.RevisionPolicy); err != nil {
			return 0, err
		}
	}
	return int(count), nil
}

func requireTarget(ctx context.Context, tx *sql.Tx, target Target) error {
	var query string
	switch target.Kind {
	case TargetGroup:
		query = `SELECT EXISTS(SELECT 1 FROM directory_groups WHERE id = ?)`
	case TargetUser:
		query = `SELECT EXISTS(SELECT 1 FROM users WHERE id = ? AND user_type = 'webmail' AND deletion_pending = 0)`
	default:
		return errUnknownKind
	}
	var exists int
	if err := tx.QueryRowContext(ctx, query, target.ID).Scan(&exists); err != nil {
		return fmt.Errorf("check policy target: %w", err)
	}
	if exists != 1 {
		return ErrTargetNotFound
	}
	return nil
}

const policySelect = `
	SELECT id, name, description, is_default, COALESCE(priority, 0), created_at, created_by, updated_at, updated_by, version
	FROM policies`

type scanner interface{ Scan(dest ...any) error }

func scanPolicy(row scanner) (Policy, error) {
	var policy Policy
	err := row.Scan(&policy.ID, &policy.Name, &policy.Description, &policy.Default, &policy.Priority,
		&policy.CreatedAt, &policy.CreatedBy, &policy.UpdatedAt, &policy.UpdatedBy, &policy.Version)
	return policy, err
}

func cleanNameAndDescription(name, description string) (string, string, error) {
	name, err := directory.CleanName(name)
	if err != nil {
		return "", "", ErrNameInvalid
	}
	description, err = directory.CleanDescription(description)
	if err != nil {
		return "", "", ErrDescriptionLong
	}
	return name, description, nil
}

// nameFree refuses a name another policy has, ignoring case and spacing.
// The Default policy's name is taken too.
func nameFree(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	normalized := directory.NormalizeName(name)
	if normalized == directory.NormalizeName(defaultName) {
		return ErrNameTaken
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM policies WHERE name_normalized = ? AND id <> ?`, normalized, exceptID).Scan(&taken); err != nil {
		return fmt.Errorf("check policy name: %w", err)
	}
	if taken > 0 {
		return ErrNameTaken
	}
	return nil
}

func oneRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStale
	}
	return nil
}

// validKey: lower-case words joined by dots, such as "mail.export".
func validKey(key string) bool {
	if key == "" || len(key) > keyMaxLength || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
		return false
	}
	return strings.IndexFunc(key, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_')
	}) < 0
}

// sameJSON compares two JSON values by what they say, not their spacing.
func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	leftText, _ := json.Marshal(left)
	rightText, _ := json.Marshal(right)
	return string(leftText) == string(rightText)
}

func uniqueTargets(targets []Target) []Target {
	seen := map[Target]bool{}
	out := make([]Target, 0, len(targets))
	for _, target := range targets {
		if target.ID == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}
