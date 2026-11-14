package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SourceKind says where a setting's value came from.
type SourceKind string

const (
	// SourceBuiltin: nothing sets it; the built-in default applies.
	SourceBuiltin SourceKind = "builtin"
	// SourceDefault: the Default policy sets it.
	SourceDefault SourceKind = "default"
	// SourcePolicy: a policy assigned to the person, or to a group of
	// theirs, sets it.
	SourcePolicy SourceKind = "policy"
)

// Source is where a setting's value came from, which the console shows
// beside it.
type Source struct {
	Kind       SourceKind
	PolicyID   string
	PolicyName string
	// Enforced: the Default policy enforces it, so no other policy could
	// change it.
	Enforced bool
	// Group is the group the policy reached the person through; empty when
	// it's assigned to them directly, or for the Default policy.
	GroupID   string
	GroupName string
}

// Resolved is one setting's value for someone, and where it came from.
type Resolved struct {
	Value any
	// Locked: people can't change it for themselves.
	Locked bool
	Source Source
}

// Ignored is a stored value the resolver skipped: a setting no longer
// declared, one only the Default policy can set, or a value that doesn't
// check. The console flags it.
type Ignored struct {
	PolicyID string
	Key      string
	Err      error
}

// Effective is every setting's value for one person, or for the instance.
type Effective struct {
	// UserID is the person; empty for the instance's settings.
	UserID  string
	values  map[string]Resolved
	Ignored []Ignored
}

// Get returns setting's value and where it came from.
func (effective *Effective) Get(setting *Setting) Resolved {
	if resolved, ok := effective.values[setting.Key]; ok {
		return resolved
	}
	return Resolved{Value: setting.Default(), Source: Source{Kind: SourceBuiltin}}
}

// Bool returns an on/off setting's value.
func (effective *Effective) Bool(setting *Setting) bool {
	value, _ := effective.Get(setting).Value.(bool)
	return value
}

// Int returns a number setting's value.
func (effective *Effective) Int(setting *Setting) int {
	value, _ := effective.Get(setting).Value.(int)
	return value
}

// String returns a choice setting's value.
func (effective *Effective) String(setting *Setting) string {
	value, _ := effective.Get(setting).Value.(string)
	return value
}

// applied is a policy that applies to the person, and how it reached them.
type applied struct {
	direct    bool
	groupID   string
	groupName string
}

// resolveInput is everything resolution reads, loaded beforehand, so the
// rules can be tested on their own.
type resolveInput struct {
	userID   string
	policies []Policy // in priority order, the Default policy last
	values   map[string][]Value
	applies  map[string]applied // by policy ID; the Default policy isn't in it
	settings []*Setting
}

// resolve applies the rules to in:
//
//  1. A value the Default policy enforces applies.
//  2. Otherwise, the first policy in priority order that applies to the
//     person and sets the value.
//  3. Otherwise, the Default policy's value, or the built-in default.
//
// Instance-only settings only look at the Default policy. Whole values
// win: lists don't merge.
func resolve(in resolveInput) *Effective {
	effective := &Effective{UserID: in.userID, values: map[string]Resolved{}}
	declared := make(map[string]*Setting, len(in.settings))
	for _, setting := range in.settings {
		declared[setting.Key] = setting
	}
	// Each policy's checked values, by key, noting what's skipped.
	checked := make(map[string]map[string]Resolved, len(in.policies))
	var defaultPolicy Policy
	for _, policy := range in.policies {
		if policy.Default {
			defaultPolicy = policy
		}
		byKey := map[string]Resolved{}
		for _, stored := range in.values[policy.ID] {
			setting, ok := declared[stored.Key]
			if !ok {
				effective.Ignored = append(effective.Ignored, Ignored{PolicyID: policy.ID, Key: stored.Key, Err: ErrUnknownSetting})
				continue
			}
			if setting.InstanceOnly && !policy.Default {
				effective.Ignored = append(effective.Ignored, Ignored{PolicyID: policy.ID, Key: stored.Key, Err: ErrInstanceOnly})
				continue
			}
			value, err := setting.Decode(stored.JSON)
			if err != nil {
				effective.Ignored = append(effective.Ignored, Ignored{PolicyID: policy.ID, Key: stored.Key, Err: err})
				continue
			}
			byKey[stored.Key] = Resolved{
				Value:  value,
				Locked: stored.Locked || (policy.Default && stored.Enforced),
				Source: Source{Kind: SourcePolicy, PolicyID: policy.ID, PolicyName: policy.Name, Enforced: policy.Default && stored.Enforced},
			}
		}
		checked[policy.ID] = byKey
	}
	defaults := checked[defaultPolicy.ID]
	for _, setting := range in.settings {
		fromDefault, defaultSets := defaults[setting.Key]
		if defaultSets {
			fromDefault.Source.Kind = SourceDefault
		}
		if defaultSets && fromDefault.Source.Enforced {
			effective.values[setting.Key] = fromDefault
			continue
		}
		found := false
		if !setting.InstanceOnly {
			for _, policy := range in.policies {
				how, applies := in.applies[policy.ID]
				if policy.Default || !applies {
					continue
				}
				if resolved, sets := checked[policy.ID][setting.Key]; sets {
					if !how.direct {
						resolved.Source.GroupID, resolved.Source.GroupName = how.groupID, how.groupName
					}
					effective.values[setting.Key] = resolved
					found = true
					break
				}
			}
		}
		if found {
			continue
		}
		if defaultSets {
			effective.values[setting.Key] = fromDefault
			continue
		}
		effective.values[setting.Key] = Resolved{Value: setting.Default(), Source: Source{Kind: SourceBuiltin}}
	}
	return effective
}

// ErrInstanceOnly is a value for a whole-server setting on a policy other
// than the Default one.
var ErrInstanceOnly = errors.New("only the Default policy sets this")

// ForUser works out every setting's value for the person with userID,
// from the policies assigned to them and to their groups.
func ForUser(ctx context.Context, q Querier, userID string) (*Effective, error) {
	in, err := load(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	return resolve(in), nil
}

// ForInstance works out the whole server's settings: the Default policy's
// values, or the built-in defaults.
func ForInstance(ctx context.Context, q Querier) (*Effective, error) {
	in, err := load(ctx, q, "")
	if err != nil {
		return nil, err
	}
	return resolve(in), nil
}

func load(ctx context.Context, q Querier, userID string) (resolveInput, error) {
	in := resolveInput{userID: userID, values: map[string][]Value{}, applies: map[string]applied{}, settings: Settings()}
	policies, err := List(ctx, q)
	if err != nil {
		return in, err
	}
	in.policies = policies
	rows, err := q.QueryContext(ctx, `
		SELECT policy_id, setting_key, value_json, locked, enforced, updated_at, updated_by
		FROM policy_values ORDER BY policy_id, setting_key`)
	if err != nil {
		return in, fmt.Errorf("load policy values: %w", err)
	}
	for rows.Next() {
		var policyID, raw string
		var value Value
		if err := rows.Scan(&policyID, &value.Key, &raw, &value.Locked, &value.Enforced, &value.UpdatedAt, &value.UpdatedBy); err != nil {
			rows.Close()
			return in, fmt.Errorf("read policy value: %w", err)
		}
		value.JSON = json.RawMessage(raw)
		in.values[policyID] = append(in.values[policyID], value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, err
	}
	// Adapted settings' Default values are read where they live.
	for _, setting := range in.settings {
		if !setting.Adapted() {
			continue
		}
		kept := in.values[DefaultID][:0:0]
		for _, stored := range in.values[DefaultID] {
			if stored.Key != setting.Key {
				kept = append(kept, stored)
			}
		}
		raw, enforced, ok, err := setting.adapter(ctx, q)
		if err != nil {
			return in, err
		}
		if ok {
			kept = append(kept, Value{Key: setting.Key, JSON: raw, Enforced: enforced})
		}
		in.values[DefaultID] = kept
	}
	if userID == "" {
		return in, nil
	}
	// The policies assigned to them, directly or through their groups. A
	// direct assignment says so; otherwise the first of their groups by
	// name that has it.
	rows, err = q.QueryContext(ctx, `
		SELECT a.policy_id, a.target_kind, COALESCE(g.id, ''), COALESCE(g.name, '')
		FROM policy_assignments a
		LEFT JOIN directory_groups g ON a.target_kind = 'group' AND g.id = a.target_id
		WHERE (a.target_kind = 'user' AND a.target_id = ?)
		   OR (a.target_kind = 'group' AND a.target_id IN (SELECT group_id FROM directory_group_members WHERE user_id = ?))
		ORDER BY a.policy_id, a.target_kind DESC, g.name_normalized, g.id`, userID, userID)
	if err != nil {
		return in, fmt.Errorf("load policy assignments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var policyID, kind, groupID, groupName string
		if err := rows.Scan(&policyID, &kind, &groupID, &groupName); err != nil {
			return in, fmt.Errorf("read policy assignment: %w", err)
		}
		// 'user' sorts after 'group', so with DESC a direct assignment
		// comes first and wins.
		if _, seen := in.applies[policyID]; seen {
			continue
		}
		in.applies[policyID] = applied{direct: TargetKind(kind) == TargetUser, groupID: groupID, groupName: groupName}
	}
	return in, rows.Err()
}

// describe is a resolution as text, for tests and logs.
func (effective *Effective) describe() string {
	keys := make([]string, 0, len(effective.values))
	for key := range effective.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		resolved := effective.values[key]
		line := fmt.Sprintf("%s=%v from %s", key, resolved.Value, resolved.Source.Kind)
		if resolved.Source.PolicyID != "" {
			line += " " + resolved.Source.PolicyID
		}
		if resolved.Source.GroupID != "" {
			line += " via " + resolved.Source.GroupID
		}
		if resolved.Locked {
			line += " locked"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
