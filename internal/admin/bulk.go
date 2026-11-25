package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// MaxBulkPeople is how many people one bulk change takes, until changes to
// more run as background jobs.
const MaxBulkPeople = 200

// BulkAction is a change made to many people at once.
type BulkAction string

const (
	BulkDisable               BulkAction = "disable"
	BulkEnable                BulkAction = "enable"
	BulkRequireMFA            BulkAction = "require-mfa"
	BulkClearMFA              BulkAction = "clear-mfa"
	BulkRequirePasswordChange BulkAction = "require-password-change"
	BulkSignOut               BulkAction = "sign-out"
	BulkAddToGroup            BulkAction = "add-to-group"
	BulkRemoveFromGroup       BulkAction = "remove-from-group"
)

// BulkActions are the bulk changes, in the order they're offered.
var BulkActions = []BulkAction{BulkDisable, BulkEnable, BulkRequireMFA, BulkClearMFA, BulkRequirePasswordChange, BulkSignOut, BulkAddToGroup, BulkRemoveFromGroup}

// ForGroup reports whether action changes who is in a group, which it
// names (BulkGroup).
func (action BulkAction) ForGroup() bool {
	return action == BulkAddToGroup || action == BulkRemoveFromGroup
}

// Valid reports whether action is one of BulkActions.
func (action BulkAction) Valid() bool {
	for _, known := range BulkActions {
		if action == known {
			return true
		}
	}
	return false
}

// permission is what the administrator needs to make action.
func (action BulkAction) permission() authz.Permission {
	switch action {
	case BulkRequireMFA, BulkClearMFA:
		return authz.UsersMFAPolicy
	case BulkRequirePasswordChange:
		return authz.UsersCredentials
	case BulkAddToGroup, BulkRemoveFromGroup:
		return authz.GroupsManage
	default:
		return authz.UsersAccess
	}
}

// Reason says why a bulk change left someone out, or what went wrong with
// them. Failures use the Code of the error, such as "needs_authenticator".
type Reason string

const (
	// ReasonYou: the change isn't for the administrator making it.
	ReasonYou Reason = "you"
	// ReasonUnchanged: they already are as the change asks.
	ReasonUnchanged Reason = "unchanged"
	// ReasonPending: they haven't accepted their invitation yet.
	ReasonPending Reason = "pending"
	// ReasonDeleting: they're being deleted.
	ReasonDeleting Reason = "deleting"
	// ReasonAdministrator: the change isn't for administrators.
	ReasonAdministrator Reason = "administrator"
	// ReasonNoPassword: they sign in without a password.
	ReasonNoPassword Reason = "no_password"
	// ReasonEveryoneRequired: the instance requires two-factor
	// authentication of everyone already.
	ReasonEveryoneRequired Reason = "everyone_required"
	// ReasonGone: nobody has that ID any more.
	ReasonGone Reason = "gone"
	// ReasonFollowUpFailed: the change was made, but what follows it, such
	// as stopping their mail, failed.
	ReasonFollowUpFailed Reason = "follow_up_failed"
)

// BulkReason says why action would leave user out, or "" when it would
// change them. The change itself checks again when it's made.
func BulkReason(actorUserID string, action BulkAction, user auth.AdministratorUserSummary, policy auth.InstanceMFAPolicy) Reason {
	actions := ActionsFor(actorUserID, user, policy)
	switch action {
	case BulkDisable, BulkEnable, BulkSignOut:
		if user.ID == actorUserID {
			return ReasonYou
		}
	}
	if user.DeletionPending {
		return ReasonDeleting
	}
	unavailable := Reason(CodeUnavailable)
	switch action {
	case BulkDisable, BulkEnable:
		switch {
		case user.Status == auth.UserStatusPending:
			return ReasonPending
		case (action == BulkDisable) == (user.Status == auth.UserStatusDisabled):
			return ReasonUnchanged
		case !actions.SetStatus:
			return unavailable
		}
	case BulkRequireMFA, BulkClearMFA:
		require := action == BulkRequireMFA
		switch {
		case user.UserType == auth.UserTypeManagement:
			return ReasonAdministrator
		case user.MFARequired == require:
			return ReasonUnchanged
		case require && policy.RequiresAllUsers():
			return ReasonEveryoneRequired
		case !actions.SetMFARequirement:
			return unavailable
		}
	case BulkRequirePasswordChange:
		switch {
		case user.UserType != auth.UserTypeWebmail || user.IsAdmin:
			return ReasonAdministrator
		case user.Status == auth.UserStatusPending:
			return ReasonPending
		case !user.HasPassword:
			return ReasonNoPassword
		case user.PasswordChangeRequired:
			return ReasonUnchanged
		case !actions.RequirePasswordChange:
			return unavailable
		}
	case BulkAddToGroup, BulkRemoveFromGroup:
		// Whether they're in the group already is known once it's chosen.
		if user.UserType != auth.UserTypeWebmail {
			return ReasonAdministrator
		}
	case BulkSignOut:
		switch {
		case user.Status == auth.UserStatusPending:
			return ReasonPending
		// Disabling signed them out.
		case user.Status == auth.UserStatusDisabled:
			return ReasonUnchanged
		case !actions.SignOut:
			return unavailable
		}
	default:
		return Reason(CodeInvalid)
	}
	return ""
}

// BulkOutcome is what a bulk change did to one person.
type BulkOutcome struct {
	UserID   string
	Username string
	// Changed: the change was made to them. Reason can still say that
	// something after it failed.
	Changed bool
	Reason  Reason
	// Sessions is how many of their sessions the change ended.
	Sessions int64
	// Err is the error behind a failure, for the log.
	Err error
}

// BulkResult is what a bulk change did, person by person, in the order
// they were asked for.
type BulkResult struct {
	Action BulkAction
	// Group is the group a change to its members was made to.
	Group string
	// Correlation ties the change's records in the audit log together.
	Correlation string
	Outcomes    []BulkOutcome
}

// Changed counts the people the change was made to.
func (result *BulkResult) Changed() int {
	count := 0
	for _, outcome := range result.Outcomes {
		if outcome.Changed {
			count++
		}
	}
	return count
}

var (
	errBulkActionInvalid = errors.New("admin: unknown bulk change")
	errBulkNobody        = errors.New("admin: a bulk change needs someone to change")
	errBulkTooMany       = fmt.Errorf("admin: a bulk change takes at most %d people", MaxBulkPeople)
	errBulkNeedsGroup    = errors.New("admin: a change to a group's members names the group")
)

// Bulk makes action to each of the people userIDs name, at most
// MaxBulkPeople, each as their own change, recorded under one correlation
// ID. Someone the change doesn't apply to is left out, with the reason. It
// fails as a whole only when the administrator may not make the change at
// all; once changes start, a failure is that person's outcome.
func (s *Service) Bulk(ctx context.Context, actor Actor, action BulkAction, userIDs []string) (*BulkResult, error) {
	if !action.Valid() {
		return nil, errBulkActionInvalid
	}
	if action.ForGroup() {
		return nil, errBulkNeedsGroup
	}
	ids, err := bulkIDs(userIDs)
	if err != nil {
		return nil, err
	}
	if err := s.auth.RequireAdministratorPermission(ctx, actor.UserID, actor.SessionID, action.permission()); err != nil {
		return nil, err
	}
	people, policy, err := s.bulkPeople(ctx, actor, ids)
	if err != nil {
		return nil, err
	}
	correlation, err := newCorrelationID()
	if err != nil {
		return nil, err
	}
	ctx = audit.WithCorrelation(ctx, correlation)

	result := &BulkResult{Action: action, Correlation: correlation, Outcomes: make([]BulkOutcome, 0, len(ids))}
	// Losing the permission, or the session's verification, stops the
	// change for everyone after.
	var stopped Reason
	for _, id := range ids {
		user, found := people[id]
		outcome := BulkOutcome{UserID: id, Username: user.Username}
		switch {
		case !found:
			outcome.Reason = ReasonGone
		case stopped != "":
			outcome.Reason = stopped
		default:
			if outcome.Reason = BulkReason(actor.UserID, action, user, policy); outcome.Reason == "" {
				outcome = s.bulkChange(ctx, actor, action, outcome)
				if code := Code(outcome.Reason); code == CodeForbidden || code == CodeVerificationRequired {
					stopped = outcome.Reason
				}
			}
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	return result, nil
}

// BulkGroup adds the people userIDs name to the group with groupID, or
// removes them, in one change recorded once for the group. Someone it
// doesn't apply to is left out with the reason, and someone already in, or
// already out, is unchanged. It fails as a whole when the change can't be
// made: the group is gone, or the administrator may not change it.
func (s *Service) BulkGroup(ctx context.Context, actor Actor, action BulkAction, groupID string, userIDs []string) (*BulkResult, error) {
	if !action.ForGroup() {
		return nil, errBulkActionInvalid
	}
	if strings.TrimSpace(groupID) == "" {
		return nil, errBulkNeedsGroup
	}
	ids, err := bulkIDs(userIDs)
	if err != nil {
		return nil, err
	}
	if err := s.auth.RequireAdministratorPermission(ctx, actor.UserID, actor.SessionID, action.permission()); err != nil {
		return nil, err
	}
	people, policy, err := s.bulkPeople(ctx, actor, ids)
	if err != nil {
		return nil, err
	}
	result := &BulkResult{Action: action, Outcomes: make([]BulkOutcome, 0, len(ids))}
	var eligible []string
	for _, id := range ids {
		user, found := people[id]
		outcome := BulkOutcome{UserID: id, Username: user.Username}
		if !found {
			outcome.Reason = ReasonGone
		} else if outcome.Reason = BulkReason(actor.UserID, action, user, policy); outcome.Reason == "" {
			eligible = append(eligible, id)
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	if len(eligible) == 0 {
		detail, err := s.Group(ctx, actor, groupID)
		if err != nil {
			return nil, err
		}
		result.Group = detail.Group.Name
		return result, nil
	}
	var change *auth.GroupMembershipChange
	if action == BulkAddToGroup {
		change, err = s.AddToGroup(ctx, actor, groupID, eligible)
	} else {
		change, err = s.RemoveFromGroup(ctx, actor, groupID, eligible)
	}
	if err != nil {
		return nil, err
	}
	result.Group = change.Group.Name
	changed := make(map[string]bool, len(change.ChangedIDs))
	for _, id := range change.ChangedIDs {
		changed[id] = true
	}
	for i, outcome := range result.Outcomes {
		if outcome.Reason != "" {
			continue
		}
		if changed[outcome.UserID] {
			result.Outcomes[i].Changed = true
		} else {
			result.Outcomes[i].Reason = ReasonUnchanged
		}
	}
	return result, nil
}

// bulkIDs are the people a bulk change names, each once.
func bulkIDs(userIDs []string) ([]string, error) {
	ids := make([]string, 0, len(userIDs))
	seen := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 0:
		return nil, errBulkNobody
	case len(ids) > MaxBulkPeople:
		return nil, errBulkTooMany
	}
	return ids, nil
}

// bulkPeople loads the people ids name, by ID, and the instance's
// two-factor policy their reasons depend on.
func (s *Service) bulkPeople(ctx context.Context, actor Actor, ids []string) (map[string]auth.AdministratorUserSummary, auth.InstanceMFAPolicy, error) {
	page, err := s.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, auth.AdministratorUserQuery{UserIDs: ids, Limit: MaxBulkPeople})
	if err != nil {
		return nil, "", err
	}
	people := make(map[string]auth.AdministratorUserSummary, len(page.Users))
	for _, user := range page.Users {
		people[user.ID] = user
	}
	policy, err := s.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load the instance's two-factor policy: %w", err)
	}
	return people, policy.MFA, nil
}

// bulkChange makes action to the person outcome names.
func (s *Service) bulkChange(ctx context.Context, actor Actor, action BulkAction, outcome BulkOutcome) BulkOutcome {
	var changed bool
	var err error
	switch action {
	case BulkDisable, BulkEnable:
		status := auth.UserStatusDisabled
		if action == BulkEnable {
			status = auth.UserStatusActive
		}
		var result *auth.SetAdministratorUserStatusResult
		if result, err = s.SetStatus(ctx, actor, outcome.UserID, status); result != nil {
			changed, outcome.Sessions = result.Changed, result.RevokedSessions
		}
	case BulkRequireMFA, BulkClearMFA:
		var result *auth.SetAdministratorUserMFAPolicyResult
		// A new requirement takes the dialog's default: a passkey is enough.
		// Someone already required was left out before.
		require := action == BulkRequireMFA
		if result, err = s.SetMFARequirement(ctx, actor, outcome.UserID, require, require); result != nil {
			changed = result.Changed
		}
	case BulkRequirePasswordChange:
		// Someone already asked to change it was left out before.
		err = s.RequirePasswordChange(ctx, actor, outcome.UserID)
		changed = err == nil
	case BulkSignOut:
		var result *auth.SignOutAdministratorUserResult
		if result, err = s.SignOut(ctx, actor, outcome.UserID); result != nil {
			outcome.Sessions = result.RevokedSessions
			changed = result.RevokedSessions > 0
		}
	}
	var hookErr *HookError
	switch {
	case errors.As(err, &hookErr):
		outcome.Changed, outcome.Reason, outcome.Err = changed, ReasonFollowUpFailed, hookErr.Err
	case err != nil:
		outcome.Reason, outcome.Err = Reason(CodeOf(err)), err
	case !changed:
		outcome.Reason = ReasonUnchanged
	default:
		outcome.Changed = true
	}
	return outcome
}

func newCorrelationID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("name the bulk change: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
