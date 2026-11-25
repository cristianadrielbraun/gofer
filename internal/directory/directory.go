// Package directory keeps groups of people: their names and descriptions,
// who is in each, and the revision counters that tell what was worked out
// from them (policies, roles) that it is out of date.
//
// Groups organize people for administration, not for mail delivery. Every
// write takes the transaction of the change it belongs to, so the service
// that checks permissions and records the audit event commits them together.
// Like audit, it depends on nothing of Gofer's.
package directory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits on what an administrator types.
const (
	NameMaxLength        = 100
	DescriptionMaxLength = 500
)

// Revision names. Each counter goes up when what it names changes.
const (
	RevisionMembership = "membership"
	RevisionPolicy     = "policy"
	RevisionRoles      = "roles"
	RevisionAccess     = "access"
)

// SourceLocal is a group made in the console. Groups from SCIM, OIDC or
// LDAP come later, with their own sources.
const SourceLocal = "local"

// MemberManual is someone an administrator added.
const MemberManual = "manual"

var (
	ErrNotFound = errors.New("the group doesn't exist")
	// ErrStale is a change made from an outdated copy of the group.
	ErrStale            = errors.New("the group changed since it was loaded")
	ErrNameTaken        = errors.New("another group has that name")
	ErrNameInvalid      = errors.New("the group name is empty, too long or has control characters")
	ErrDescriptionLong  = errors.New("the group description is too long or has control characters")
	ErrPersonNotFound   = errors.New("someone to add doesn't exist")
	ErrRevisionUnknown  = errors.New("unknown revision")
	errMissingID        = errors.New("a group needs an ID")
	errMissingTimestamp = errors.New("a change needs a time")
)

// Querier reads: a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Group is one group, with how many people are in it.
type Group struct {
	ID          string
	Name        string
	Description string
	Source      string
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string
	// Version goes up with each rename or new description; a change names
	// the version it was made from.
	Version     int64
	MemberCount int
}

// Member is someone in a group.
type Member struct {
	UserID  string
	Source  string
	AddedAt time.Time
	AddedBy string
}

// NewGroup is a group to create. ID comes from the caller, like every other
// ID in Gofer; By is the user ID of who creates it.
type NewGroup struct {
	ID          string
	Name        string
	Description string
	By          string
	At          time.Time
}

// GroupChange renames a group or changes its description, from Version.
type GroupChange struct {
	ID          string
	Version     int64
	Name        string
	Description string
	By          string
	At          time.Time
}

// NormalizeName is the form names are compared in: spaces collapsed and
// case folded, so "Field  Team" and "field team" are the same group.
func NormalizeName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// CleanName trims and collapses the spaces in a name and checks it.
func CleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > NameMaxLength || !utf8.ValidString(name) || hasControl(name) {
		return "", ErrNameInvalid
	}
	return name, nil
}

// CleanDescription trims a description and checks it. Line breaks are kept.
func CleanDescription(description string) (string, error) {
	description = strings.TrimSpace(description)
	if utf8.RuneCountInString(description) > DescriptionMaxLength || !utf8.ValidString(description) ||
		hasControl(strings.NewReplacer("\n", "", "\r", "", "\t", "").Replace(description)) {
		return "", ErrDescriptionLong
	}
	return description, nil
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

// Create adds a group.
func Create(ctx context.Context, tx *sql.Tx, group NewGroup) (Group, error) {
	if group.ID == "" {
		return Group{}, errMissingID
	}
	if group.At.IsZero() {
		return Group{}, errMissingTimestamp
	}
	name, err := CleanName(group.Name)
	if err != nil {
		return Group{}, err
	}
	description, err := CleanDescription(group.Description)
	if err != nil {
		return Group{}, err
	}
	if err := nameFree(ctx, tx, name, ""); err != nil {
		return Group{}, err
	}
	at := group.At.UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directory_groups (id, name, name_normalized, description, source, created_at, created_by, updated_at, updated_by, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		group.ID, name, NormalizeName(name), description, SourceLocal, at, group.By, at, group.By,
	); err != nil {
		return Group{}, fmt.Errorf("create group: %w", err)
	}
	return Get(ctx, tx, group.ID)
}

// Update renames a group or changes its description. Nothing changes, and
// the version stays, when both are what they were.
func Update(ctx context.Context, tx *sql.Tx, change GroupChange) (Group, error) {
	if change.At.IsZero() {
		return Group{}, errMissingTimestamp
	}
	current, err := Get(ctx, tx, change.ID)
	if err != nil {
		return Group{}, err
	}
	if current.Version != change.Version {
		return Group{}, ErrStale
	}
	name, err := CleanName(change.Name)
	if err != nil {
		return Group{}, err
	}
	description, err := CleanDescription(change.Description)
	if err != nil {
		return Group{}, err
	}
	if name == current.Name && description == current.Description {
		return current, nil
	}
	if err := nameFree(ctx, tx, name, change.ID); err != nil {
		return Group{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE directory_groups
		SET name = ?, name_normalized = ?, description = ?, updated_at = ?, updated_by = ?, version = version + 1
		WHERE id = ? AND version = ?`,
		name, NormalizeName(name), description, change.At.UTC(), change.By, change.ID, change.Version,
	)
	if err != nil {
		return Group{}, fmt.Errorf("update group: %w", err)
	}
	if err := oneRow(result); err != nil {
		return Group{}, err
	}
	return Get(ctx, tx, change.ID)
}

// Delete removes a group and everyone's membership in it. It returns how
// many people were in it.
func Delete(ctx context.Context, tx *sql.Tx, id string, version int64) (int, error) {
	current, err := Get(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if current.Version != version {
		return 0, ErrStale
	}
	removed, err := tx.ExecContext(ctx, `DELETE FROM directory_group_members WHERE group_id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("remove the group's members: %w", err)
	}
	members, err := removed.RowsAffected()
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM directory_groups WHERE id = ? AND version = ?`, id, version)
	if err != nil {
		return 0, fmt.Errorf("delete group: %w", err)
	}
	if err := oneRow(result); err != nil {
		return 0, err
	}
	if members > 0 {
		if err := Bump(ctx, tx, RevisionMembership); err != nil {
			return 0, err
		}
	}
	return int(members), nil
}

// Get loads one group.
func Get(ctx context.Context, q Querier, id string) (Group, error) {
	group, err := scanGroup(q.QueryRowContext(ctx, groupSelect+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("load group: %w", err)
	}
	return group, nil
}

// List returns every group by name.
func List(ctx context.Context, q Querier) ([]Group, error) {
	return queryGroups(ctx, q, groupSelect+` ORDER BY g.name_normalized, g.id`)
}

// GroupsOf returns the groups someone is in, by name.
func GroupsOf(ctx context.Context, q Querier, userID string) ([]Group, error) {
	return queryGroups(ctx, q, groupSelect+`
		WHERE g.id IN (SELECT group_id FROM directory_group_members WHERE user_id = ?)
		ORDER BY g.name_normalized, g.id`, userID)
}

// Members returns who is in a group, oldest addition first.
func Members(ctx context.Context, q Querier, groupID string) ([]Member, error) {
	if _, err := Get(ctx, q, groupID); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT user_id, source, added_at, added_by FROM directory_group_members
		WHERE group_id = ? ORDER BY added_at, user_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.UserID, &member.Source, &member.AddedAt, &member.AddedBy); err != nil {
			return nil, fmt.Errorf("read group member: %w", err)
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// AddMembers puts people in a group. Someone already in it stays as they
// were. It returns the people it added, in the order given.
func AddMembers(ctx context.Context, tx *sql.Tx, groupID string, userIDs []string, by string, at time.Time) ([]string, error) {
	if at.IsZero() {
		return nil, errMissingTimestamp
	}
	if _, err := Get(ctx, tx, groupID); err != nil {
		return nil, err
	}
	var added []string
	for _, userID := range unique(userIDs) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check group member: %w", err)
		}
		if exists == 0 {
			return nil, ErrPersonNotFound
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO directory_group_members (group_id, user_id, source, added_at, added_by)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (group_id, user_id) DO NOTHING`,
			groupID, userID, MemberManual, at.UTC(), by)
		if err != nil {
			return nil, fmt.Errorf("add group member: %w", err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if count == 1 {
			added = append(added, userID)
		}
	}
	if len(added) > 0 {
		if err := Bump(ctx, tx, RevisionMembership); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// RemoveMembers takes people out of a group. Someone not in it is skipped.
// It returns the people it removed, in the order given.
func RemoveMembers(ctx context.Context, tx *sql.Tx, groupID string, userIDs []string) ([]string, error) {
	if _, err := Get(ctx, tx, groupID); err != nil {
		return nil, err
	}
	var removed []string
	for _, userID := range unique(userIDs) {
		result, err := tx.ExecContext(ctx, `DELETE FROM directory_group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
		if err != nil {
			return nil, fmt.Errorf("remove group member: %w", err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if count == 1 {
			removed = append(removed, userID)
		}
	}
	if len(removed) > 0 {
		if err := Bump(ctx, tx, RevisionMembership); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// RemovePerson takes someone out of every group, as their account is
// deleted. It returns how many groups they were in.
func RemovePerson(ctx context.Context, tx *sql.Tx, userID string) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM directory_group_members WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("remove someone from their groups: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if err := Bump(ctx, tx, RevisionMembership); err != nil {
			return 0, err
		}
	}
	return int(count), nil
}

// Revision reads a counter; one that never went up is 0.
func Revision(ctx context.Context, q Querier, name string) (int64, error) {
	if !knownRevision(name) {
		return 0, ErrRevisionUnknown
	}
	var revision int64
	err := q.QueryRowContext(ctx, `SELECT revision FROM admin_revisions WHERE name = ?`, name).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the %s revision: %w", name, err)
	}
	return revision, nil
}

// Bump moves a counter up by one.
func Bump(ctx context.Context, tx *sql.Tx, name string) error {
	if !knownRevision(name) {
		return ErrRevisionUnknown
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admin_revisions (name, revision) VALUES (?, 1)
		ON CONFLICT (name) DO UPDATE SET revision = revision + 1`, name); err != nil {
		return fmt.Errorf("bump the %s revision: %w", name, err)
	}
	return nil
}

func knownRevision(name string) bool {
	switch name {
	case RevisionMembership, RevisionPolicy, RevisionRoles, RevisionAccess:
		return true
	}
	return false
}

const groupSelect = `
	SELECT g.id, g.name, g.description, g.source, g.created_at, g.created_by, g.updated_at, g.updated_by, g.version,
		(SELECT COUNT(*) FROM directory_group_members m WHERE m.group_id = g.id)
	FROM directory_groups g`

type scanner interface{ Scan(dest ...any) error }

func scanGroup(row scanner) (Group, error) {
	var group Group
	err := row.Scan(&group.ID, &group.Name, &group.Description, &group.Source, &group.CreatedAt, &group.CreatedBy,
		&group.UpdatedAt, &group.UpdatedBy, &group.Version, &group.MemberCount)
	return group, err
}

func queryGroups(ctx context.Context, q Querier, query string, args ...any) ([]Group, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("read group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// nameFree refuses a name another group has, ignoring case and spacing.
func nameFree(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM directory_groups WHERE name_normalized = ? AND id <> ?`,
		NormalizeName(name), exceptID).Scan(&taken); err != nil {
		return fmt.Errorf("check group name: %w", err)
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

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
