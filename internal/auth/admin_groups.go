package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/directory"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// auditTargetGroup is what the administration audit log calls a group.
const auditTargetGroup = "group"

// ErrGroupMemberInvalid means someone can't be put in a group: they don't
// exist, aren't a webmail user, or are being deleted.
var ErrGroupMemberInvalid = errors.New("only webmail users who aren't being deleted can be in a group")

// GroupMember is someone in a group, as the console shows them.
type GroupMember struct {
	UserID          string
	Username        string
	Name            string
	Status          UserStatus
	AddedAt         time.Time
	AddedByUsername string
}

// GroupDetail is a group and who is in it.
type GroupDetail struct {
	Group   directory.Group
	Members []GroupMember
}

// GroupDeletionPreview is what deleting a group would undo. Policies
// assigned to it join in a later phase.
type GroupDeletionPreview struct {
	Group directory.Group
}

// GroupMembershipChange is who a change to a group's members affected.
type GroupMembershipChange struct {
	Group directory.Group
	// Changed are the usernames added or removed, and ChangedIDs their IDs;
	// people already in, or already out, aren't.
	Changed    []string
	ChangedIDs []string
}

// CreateGroupOptions names a new group.
type CreateGroupOptions struct {
	ActorUserID    string
	ActorSessionID string
	Name           string
	Description    string
}

// UpdateGroupOptions renames a group or changes its description, from the
// version the administrator loaded.
type UpdateGroupOptions struct {
	ActorUserID    string
	ActorSessionID string
	GroupID        string
	Version        int64
	Name           string
	Description    string
}

// ChangeGroupMembersOptions adds people to a group or removes them.
type ChangeGroupMembersOptions struct {
	ActorUserID    string
	ActorSessionID string
	GroupID        string
	UserIDs        []string
}

// ListGroups returns every group with how many people are in it.
func (m *Manager) ListGroups(ctx context.Context, actorUserID string) ([]directory.Group, error) {
	var groups []directory.Group
	err := m.readGroups(ctx, actorUserID, func(tx *sql.Tx) (err error) {
		groups, err = directory.List(ctx, tx)
		return err
	})
	return groups, err
}

// GetGroup returns a group and who is in it, by username.
func (m *Manager) GetGroup(ctx context.Context, actorUserID, groupID string) (*GroupDetail, error) {
	var detail *GroupDetail
	err := m.readGroups(ctx, actorUserID, func(tx *sql.Tx) error {
		group, err := directory.Get(ctx, tx, groupID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT u.id, u.username, u.name, u.status, m.added_at, COALESCE(a.username, '')
			FROM directory_group_members m
			JOIN users u ON u.id = m.user_id
			LEFT JOIN users a ON a.id = m.added_by
			WHERE m.group_id = ?
			ORDER BY u.username_normalized, u.id`, groupID)
		if err != nil {
			return fmt.Errorf("list group members: %w", err)
		}
		defer rows.Close()
		detail = &GroupDetail{Group: group}
		for rows.Next() {
			var member GroupMember
			if err := rows.Scan(&member.UserID, &member.Username, &member.Name, &member.Status, &member.AddedAt, &member.AddedByUsername); err != nil {
				return fmt.Errorf("read group member: %w", err)
			}
			detail.Members = append(detail.Members, member)
		}
		return rows.Err()
	})
	return detail, err
}

// GroupRef names a group.
type GroupRef struct {
	ID   string
	Name string
}

// GroupsOfUsers returns, for each of userIDs who is in a group, the groups
// they're in by name.
func (m *Manager) GroupsOfUsers(ctx context.Context, actorUserID string, userIDs []string) (map[string][]GroupRef, error) {
	groups := map[string][]GroupRef{}
	if len(userIDs) == 0 {
		return groups, nil
	}
	err := m.readGroups(ctx, actorUserID, func(tx *sql.Tx) error {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(userIDs)), ",")
		args := make([]any, len(userIDs))
		for i, id := range userIDs {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT m.user_id, g.id, g.name FROM directory_group_members m
			JOIN directory_groups g ON g.id = m.group_id
			WHERE m.user_id IN (`+placeholders+`)
			ORDER BY g.name_normalized, g.id`, args...)
		if err != nil {
			return fmt.Errorf("list people's groups: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var userID string
			var group GroupRef
			if err := rows.Scan(&userID, &group.ID, &group.Name); err != nil {
				return fmt.Errorf("read a person's group: %w", err)
			}
			groups[userID] = append(groups[userID], group)
		}
		return rows.Err()
	})
	return groups, err
}

// GroupsOfUser returns the groups someone is in.
func (m *Manager) GroupsOfUser(ctx context.Context, actorUserID, userID string) ([]directory.Group, error) {
	var groups []directory.Group
	err := m.readGroups(ctx, actorUserID, func(tx *sql.Tx) (err error) {
		groups, err = directory.GroupsOf(ctx, tx, userID)
		return err
	})
	return groups, err
}

// PreviewGroupDeletion says what deleting a group would undo.
func (m *Manager) PreviewGroupDeletion(ctx context.Context, actorUserID, groupID string) (*GroupDeletionPreview, error) {
	var preview *GroupDeletionPreview
	err := m.readGroups(ctx, actorUserID, func(tx *sql.Tx) error {
		group, err := directory.Get(ctx, tx, groupID)
		if err != nil {
			return err
		}
		preview = &GroupDeletionPreview{Group: group}
		return nil
	})
	return preview, err
}

func (m *Manager) readGroups(ctx context.Context, actorUserID string, read func(*sql.Tx) error) error {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin group read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, strings.TrimSpace(actorUserID), authz.GroupsRead); err != nil {
		return err
	}
	return read(tx)
}

// CreateGroup adds a group.
func (m *Manager) CreateGroup(ctx context.Context, options CreateGroupOptions) (*directory.Group, error) {
	var created directory.Group
	err := m.changeGroups(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		id, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate group ID: %w", err)
		}
		created, err = directory.Create(ctx, tx, directory.NewGroup{
			ID: id, Name: options.Name, Description: options.Description, By: actorUserID, At: now,
		})
		if err != nil {
			return err
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: "group.created",
			Target: groupAuditTarget(created),
			Changes: map[string]audit.Change{
				"name":        {From: nil, To: created.Name},
				"description": {From: nil, To: created.Description},
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// UpdateGroup renames a group or changes its description. Renaming is
// recorded as group.renamed, a new description alone as group.updated;
// a change that changes nothing isn't recorded.
func (m *Manager) UpdateGroup(ctx context.Context, options UpdateGroupOptions) (*directory.Group, error) {
	var updated directory.Group
	err := m.changeGroups(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		before, err := directory.Get(ctx, tx, options.GroupID)
		if err != nil {
			return err
		}
		updated, err = directory.Update(ctx, tx, directory.GroupChange{
			ID: options.GroupID, Version: options.Version, Name: options.Name, Description: options.Description,
			By: actorUserID, At: now,
		})
		if err != nil || updated.Version == before.Version {
			return err
		}
		changes := map[string]audit.Change{}
		action := "group.updated"
		if updated.Name != before.Name {
			action = "group.renamed"
			changes["name"] = audit.Change{From: before.Name, To: updated.Name}
		}
		if updated.Description != before.Description {
			changes["description"] = audit.Change{From: before.Description, To: updated.Description}
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action: action, Target: groupAuditTarget(updated), Changes: changes,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DeleteGroup removes a group, from the version the administrator loaded,
// and everyone's membership in it. It returns how many people were in it.
func (m *Manager) DeleteGroup(ctx context.Context, actorUserID, actorSessionID, groupID string, version int64) (int, error) {
	var members int
	err := m.changeGroups(ctx, actorUserID, actorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, _ time.Time) error {
		group, err := directory.Get(ctx, tx, groupID)
		if err != nil {
			return err
		}
		members, err = directory.Delete(ctx, tx, groupID, version)
		if err != nil {
			return err
		}
		// The policies it had stop applying through it, and nothing else.
		if _, err := policy.RemoveTarget(ctx, tx, policy.Target{Kind: policy.TargetGroup, ID: groupID}); err != nil {
			return err
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:   "group.deleted",
			Target:   groupAuditTarget(group),
			Metadata: map[string]any{"members": members},
		})
	})
	return members, err
}

// AddGroupMembers puts webmail users in a group. Someone already in it is
// skipped; someone who can't be in a group refuses the whole change.
func (m *Manager) AddGroupMembers(ctx context.Context, options ChangeGroupMembersOptions) (*GroupMembershipChange, error) {
	return m.changeGroupMembers(ctx, options, true)
}

// RemoveGroupMembers takes people out of a group. Someone not in it is
// skipped.
func (m *Manager) RemoveGroupMembers(ctx context.Context, options ChangeGroupMembersOptions) (*GroupMembershipChange, error) {
	return m.changeGroupMembers(ctx, options, false)
}

func (m *Manager) changeGroupMembers(ctx context.Context, options ChangeGroupMembersOptions, add bool) (*GroupMembershipChange, error) {
	result := &GroupMembershipChange{}
	err := m.changeGroups(ctx, options.ActorUserID, options.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		group, err := directory.Get(ctx, tx, options.GroupID)
		if err != nil {
			return err
		}
		var changed []string
		if add {
			for _, userID := range options.UserIDs {
				if userID == "" {
					continue
				}
				if err := requireGroupMemberEligible(ctx, tx, userID); err != nil {
					return err
				}
			}
			changed, err = directory.AddMembers(ctx, tx, group.ID, options.UserIDs, actorUserID, now)
		} else {
			changed, err = directory.RemoveMembers(ctx, tx, group.ID, options.UserIDs)
		}
		if err != nil {
			return err
		}
		if result.Group, err = directory.Get(ctx, tx, group.ID); err != nil {
			return err
		}
		if len(changed) == 0 {
			return nil
		}
		people := make([]map[string]string, 0, len(changed))
		for _, userID := range changed {
			username, err := usernameForAudit(ctx, tx, userID)
			if err != nil {
				return err
			}
			people = append(people, map[string]string{"id": userID, "username": username})
			result.Changed = append(result.Changed, username)
			result.ChangedIDs = append(result.ChangedIDs, userID)
		}
		action := "group.members_removed"
		if add {
			action = "group.members_added"
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:   action,
			Target:   groupAuditTarget(group),
			Metadata: map[string]any{"count": len(changed), "people": people},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// changeGroups runs change in a transaction once the actor may manage
// groups from a recently verified session.
func (m *Manager) changeGroups(
	ctx context.Context, actorUserID, actorSessionID string,
	change func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error,
) error {
	actorUserID = strings.TrimSpace(actorUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	if actorUserID == "" {
		return ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return ErrRecentStepUpRequired
	}
	now := m.clock.Now().UTC()
	return m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.GroupsManage, now); err != nil {
			return err
		}
		return change(tx, actorUserID, actorSessionID, now)
	})
}

func requireGroupMemberEligible(ctx context.Context, tx *sql.Tx, userID string) error {
	var eligible int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM users WHERE id = ? AND user_type = 'webmail' AND deletion_pending = 0)`,
		userID).Scan(&eligible); err != nil {
		return fmt.Errorf("check group member: %w", err)
	}
	if eligible != 1 {
		return ErrGroupMemberInvalid
	}
	return nil
}

func groupAuditTarget(group directory.Group) audit.Target {
	return audit.Target{Kind: auditTargetGroup, ID: group.ID, Label: group.Name}
}
