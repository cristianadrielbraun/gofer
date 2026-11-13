package admin

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/directory"
)

// Groups are read with groups.read and changed with groups.manage, which
// needs a recently verified session. Every change is recorded in the
// Activity log in the transaction that makes it.

// ListGroups returns every group with how many people are in it.
func (s *Service) ListGroups(ctx context.Context, actor Actor) ([]directory.Group, error) {
	return s.auth.ListGroups(ctx, actor.UserID)
}

// Group returns a group and who is in it.
func (s *Service) Group(ctx context.Context, actor Actor, groupID string) (*auth.GroupDetail, error) {
	return s.auth.GetGroup(ctx, actor.UserID, groupID)
}

// GroupsOf returns the groups someone is in.
func (s *Service) GroupsOf(ctx context.Context, actor Actor, userID string) ([]directory.Group, error) {
	return s.auth.GroupsOfUser(ctx, actor.UserID, userID)
}

// CreateGroup adds a group.
func (s *Service) CreateGroup(ctx context.Context, actor Actor, name, description string) (*directory.Group, error) {
	return s.auth.CreateGroup(ctx, auth.CreateGroupOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Name: name, Description: description,
	})
}

// UpdateGroup renames a group or changes its description, from the version
// the administrator loaded.
func (s *Service) UpdateGroup(ctx context.Context, actor Actor, groupID string, version int64, name, description string) (*directory.Group, error) {
	return s.auth.UpdateGroup(ctx, auth.UpdateGroupOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, GroupID: groupID, Version: version,
		Name: name, Description: description,
	})
}

// PreviewGroupDeletion says what deleting a group would undo.
func (s *Service) PreviewGroupDeletion(ctx context.Context, actor Actor, groupID string) (*auth.GroupDeletionPreview, error) {
	return s.auth.PreviewGroupDeletion(ctx, actor.UserID, groupID)
}

// DeleteGroup removes a group, from the version the administrator loaded,
// and says how many people were in it.
func (s *Service) DeleteGroup(ctx context.Context, actor Actor, groupID string, version int64) (int, error) {
	return s.auth.DeleteGroup(ctx, actor.UserID, actor.SessionID, groupID, version)
}

// AddToGroup puts up to MaxBulkPeople people in a group: one, a list, or
// a selection from the Users list.
func (s *Service) AddToGroup(ctx context.Context, actor Actor, groupID string, userIDs []string) (*auth.GroupMembershipChange, error) {
	if err := checkGroupPeople(userIDs); err != nil {
		return nil, err
	}
	return s.auth.AddGroupMembers(ctx, auth.ChangeGroupMembersOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, GroupID: groupID, UserIDs: userIDs,
	})
}

// RemoveFromGroup takes up to MaxBulkPeople people out of a group.
func (s *Service) RemoveFromGroup(ctx context.Context, actor Actor, groupID string, userIDs []string) (*auth.GroupMembershipChange, error) {
	if err := checkGroupPeople(userIDs); err != nil {
		return nil, err
	}
	return s.auth.RemoveGroupMembers(ctx, auth.ChangeGroupMembersOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, GroupID: groupID, UserIDs: userIDs,
	})
}

func checkGroupPeople(userIDs []string) error {
	switch {
	case len(userIDs) == 0:
		return errBulkNobody
	case len(userIDs) > MaxBulkPeople:
		return errBulkTooMany
	}
	return nil
}
