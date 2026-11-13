package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// ListAdministratorUsers returns application-login profile metadata without
// any invitation action references. Call ListAdministratorUsersForSession for
// an interactive administrator page that needs invitation lifecycle actions.
func (m *Manager) ListAdministratorUsers(ctx context.Context, actorUserID string) ([]AdministratorUserSummary, error) {
	page, err := m.listAdministratorUsers(ctx, actorUserID, "", AdministratorUserQuery{Limit: -1})
	if err != nil {
		return nil, err
	}
	return page.Users, nil
}

// ListAdministratorUsersForSession also returns bounded invitation lifecycle
// metadata and opaque action references bound to the exact actor session. Raw
// enrollment token values, hashes, and database token IDs are never returned.
func (m *Manager) ListAdministratorUsersForSession(ctx context.Context, actorUserID, actorSessionID string) ([]AdministratorUserSummary, error) {
	page, err := m.listAdministratorUsers(ctx, actorUserID, actorSessionID, AdministratorUserQuery{Limit: -1})
	if err != nil {
		return nil, err
	}
	return page.Users, nil
}

// ListAdministratorUserPage returns one page of the people list as query
// narrows and orders it, with invitation action references bound to the
// actor's session, how many people match, and everyone's counts.
func (m *Manager) ListAdministratorUserPage(ctx context.Context, actorUserID, actorSessionID string, query AdministratorUserQuery) (*AdministratorUserPage, error) {
	return m.listAdministratorUsers(ctx, actorUserID, actorSessionID, query)
}

// Pages of the people list.
const (
	DefaultAdministratorUserPageSize = 50
	MaxAdministratorUserPageSize     = 200
)

// AdministratorUserQuery narrows, orders and pages the people list.
type AdministratorUserQuery struct {
	// Search keeps the people whose username holds it. Display names are
	// each person's own profile, which the console doesn't read.
	Search string
	// Status keeps one status; "" keeps every status.
	Status UserStatus
	// Attention keeps the people who need an administrator for one reason;
	// "" keeps everyone.
	Attention AdministratorUserAttention
	// Sort orders the list; "" is by username.
	Sort AdministratorUserSort
	// UserIDs keeps only these people, for a page about one of them.
	UserIDs []string
	// Administrators keeps only administrators.
	Administrators bool
	// Groupable keeps only the people who can be put in a group: webmail
	// users who aren't being deleted.
	Groupable bool
	// NotInGroup leaves out the people already in the group with this ID.
	NotInGroup string
	// Group keeps only the people in the group with this ID.
	Group string
	Offset         int
	// Limit is the page's size: 0 is DefaultAdministratorUserPageSize, and
	// it's never more than MaxAdministratorUserPageSize. A negative limit,
	// for callers inside auth, lists everyone.
	Limit int
}

// AdministratorUserAttention is a reason someone needs an administrator.
type AdministratorUserAttention string

const (
	AdministratorUserAttentionResetRequested    AdministratorUserAttention = "reset_requested"
	AdministratorUserAttentionMFASetup          AdministratorUserAttention = "mfa_setup"
	AdministratorUserAttentionInvitationExpired AdministratorUserAttention = "invitation_expired"
	AdministratorUserAttentionDeleting          AdministratorUserAttention = "deleting"
)

// Valid reports whether attention is one of the reasons, or none.
func (attention AdministratorUserAttention) Valid() bool {
	switch attention {
	case "", AdministratorUserAttentionResetRequested, AdministratorUserAttentionMFASetup,
		AdministratorUserAttentionInvitationExpired, AdministratorUserAttentionDeleting:
		return true
	}
	return false
}

// AdministratorUserSort is an order of the people list.
type AdministratorUserSort string

const (
	AdministratorUserSortUsername   AdministratorUserSort = "username"
	AdministratorUserSortNewest     AdministratorUserSort = "newest"
	AdministratorUserSortLastSignIn AdministratorUserSort = "last_sign_in"
	AdministratorUserSortStatus     AdministratorUserSort = "status"
)

// Valid reports whether sort is one of the orders, or the default.
func (sort AdministratorUserSort) Valid() bool {
	switch sort {
	case "", AdministratorUserSortUsername, AdministratorUserSortNewest, AdministratorUserSortLastSignIn, AdministratorUserSortStatus:
		return true
	}
	return false
}

// AdministratorUserPage is one page of the people list.
type AdministratorUserPage struct {
	Users []AdministratorUserSummary
	// Matching is how many people the query keeps, over every page.
	Matching int
	Counts   AdministratorUserCounts
}

// AdministratorUserCounts count everyone, whatever the list shows: by
// status, the administrators, and each reason someone needs an
// administrator.
type AdministratorUserCounts struct {
	Total             int
	Active            int
	Pending           int
	Disabled          int
	Administrators    int
	ResetRequested    int
	MFASetup          int
	InvitationExpired int
	Deleting          int
}

// administratorPeople is everyone, with what the list shows and filters on,
// as common table expressions; :now is the time invitations are judged at.
const administratorPeople = `
	WITH policy AS (
		SELECT COALESCE((SELECT state.mfa_policy FROM auth_system_state state WHERE state.id = 1), 'administrators') AS mfa_policy
	), invitations AS (
		SELECT candidate.user_id, candidate.id, candidate.expires_at, candidate.used_at, candidate.revoked_at,
		       ROW_NUMBER() OVER (
				PARTITION BY candidate.user_id
				ORDER BY CASE
					WHEN candidate.used_at IS NULL AND candidate.revoked_at IS NULL
					 AND candidate.expires_at > :now THEN 0 ELSE 1
				END, candidate.created_at DESC, candidate.id DESC
		       ) AS rank
		FROM user_enrollment_tokens candidate
		WHERE candidate.purpose = 'enrollment'
	), people AS (
		SELECT u.id, u.username, u.username_normalized, u.status, u.user_type, u.is_admin, u.mfa_required, u.mfa_passkey_counts,
		       u.password_reset_requested_at, u.deletion_pending, u.created_at, u.last_login_at,
		       EXISTS(SELECT 1 FROM auth_system_state state WHERE state.id = 1 AND state.owner_user_id = u.id) AS deletion_protected,
		       invitation.id AS invitation_id, invitation.expires_at, invitation.used_at, invitation.revoked_at,
		       EXISTS(SELECT 1 FROM password_credentials p WHERE p.user_id = u.id) AS has_password,
		       EXISTS(SELECT 1 FROM password_credentials p WHERE p.user_id = u.id AND p.must_change = 1) AS password_change_required,
		       (EXISTS(SELECT 1 FROM totp_credentials t WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL)
		        OR (u.mfa_required = 1 AND u.mfa_passkey_counts = 1 AND u.user_type = 'webmail' AND u.is_admin = 0
		            AND (SELECT mfa_policy FROM policy) <> 'all_users'
		            AND EXISTS(SELECT 1 FROM webauthn_credentials w WHERE w.user_id = u.id AND w.revoked_at IS NULL))) AS has_second_factor
		FROM users u
		LEFT JOIN invitations invitation ON invitation.user_id = u.id AND invitation.rank = 1
	)`

// What each reason to need an administrator means, over people. They match
// the Users page: two-factor authentication is required of administrators,
// of the people it's required of one by one, and of everyone when the
// instance says so.
const (
	peopleNeedMFASetup = `(people.status = 'active' AND people.deletion_pending = 0 AND people.has_second_factor = 0
		AND (people.mfa_required = 1 OR people.user_type = 'management' OR (SELECT mfa_policy FROM policy) = 'all_users'))`
	peopleInvitationExpired = `(people.status = 'pending' AND people.user_type = 'webmail' AND people.is_admin = 0
		AND people.invitation_id IS NOT NULL AND people.revoked_at IS NULL AND people.used_at IS NULL AND people.expires_at <= :now)`
)

// administratorUserFilter is the WHERE clause query asks for, with its
// arguments.
func administratorUserFilter(query AdministratorUserQuery) (string, []any) {
	var conditions []string
	var args []any
	if search := strings.ToLower(strings.TrimSpace(query.Search)); search != "" {
		conditions = append(conditions, `people.username_normalized LIKE :search ESCAPE '\'`)
		args = append(args, sql.Named("search", "%"+likeEscaper.Replace(search)+"%"))
	}
	if query.Administrators {
		conditions = append(conditions, `people.user_type = 'management'`)
	}
	if query.Groupable {
		conditions = append(conditions, `people.user_type = 'webmail' AND people.deletion_pending = 0`)
	}
	if query.Group != "" {
		conditions = append(conditions, `people.id IN (SELECT user_id FROM directory_group_members WHERE group_id = :in_group)`)
		args = append(args, sql.Named("in_group", query.Group))
	}
	if query.NotInGroup != "" {
		conditions = append(conditions, `people.id NOT IN (SELECT user_id FROM directory_group_members WHERE group_id = :not_in_group)`)
		args = append(args, sql.Named("not_in_group", query.NotInGroup))
	}
	if query.Status != "" {
		conditions = append(conditions, `people.status = :status`)
		args = append(args, sql.Named("status", string(query.Status)))
	}
	if query.UserIDs != nil {
		names := make([]string, 0, len(query.UserIDs))
		for i, id := range query.UserIDs {
			name := fmt.Sprintf("user%d", i)
			names = append(names, ":"+name)
			args = append(args, sql.Named(name, id))
		}
		if len(names) == 0 {
			conditions = append(conditions, `0`)
		} else {
			conditions = append(conditions, `people.id IN (`+strings.Join(names, ", ")+`)`)
		}
	}
	switch query.Attention {
	case AdministratorUserAttentionResetRequested:
		conditions = append(conditions, `people.password_reset_requested_at IS NOT NULL`)
	case AdministratorUserAttentionMFASetup:
		conditions = append(conditions, peopleNeedMFASetup)
	case AdministratorUserAttentionInvitationExpired:
		conditions = append(conditions, peopleInvitationExpired)
	case AdministratorUserAttentionDeleting:
		conditions = append(conditions, `people.deletion_pending = 1`)
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// likeEscaper keeps a search's own %, _ and \ from meaning anything.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// administratorUserOrder is the ORDER BY clause of sort.
func administratorUserOrder(sort AdministratorUserSort) string {
	switch sort {
	case AdministratorUserSortNewest:
		return ` ORDER BY people.created_at DESC, people.id DESC`
	case AdministratorUserSortLastSignIn:
		return ` ORDER BY people.last_login_at IS NULL, people.last_login_at DESC, people.username_normalized, people.id`
	case AdministratorUserSortStatus:
		return ` ORDER BY CASE people.status WHEN 'active' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END, people.username_normalized, people.id`
	}
	return ` ORDER BY people.username_normalized, people.id`
}

func (m *Manager) listAdministratorUsers(ctx context.Context, actorUserID, actorSessionID string, query AdministratorUserQuery) (*AdministratorUserPage, error) {
	if !query.Attention.Valid() || !query.Sort.Valid() || len(query.UserIDs) > MaxAdministratorUserPageSize ||
		(query.Status != "" && query.Status != UserStatusPending && query.Status != UserStatusActive && query.Status != UserStatusDisabled) {
		return nil, fmt.Errorf("invalid administrator user query %+v", query)
	}
	switch {
	case query.Limit == 0:
		query.Limit = DefaultAdministratorUserPageSize
	case query.Limit > MaxAdministratorUserPageSize:
		query.Limit = MaxAdministratorUserPageSize
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin administrator user list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, actorUserID, authz.UsersRead); err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	at := sql.Named("now", now)
	page := &AdministratorUserPage{Users: make([]AdministratorUserSummary, 0)}

	if err := tx.QueryRowContext(ctx, administratorPeople+`
		SELECT COUNT(*),
		       COALESCE(SUM(people.status = 'active'), 0), COALESCE(SUM(people.status = 'pending'), 0),
		       COALESCE(SUM(people.status = 'disabled'), 0), COALESCE(SUM(people.is_admin = 1), 0),
		       COALESCE(SUM(people.password_reset_requested_at IS NOT NULL), 0),
		       COALESCE(SUM(`+peopleNeedMFASetup+`), 0), COALESCE(SUM(`+peopleInvitationExpired+`), 0),
		       COALESCE(SUM(people.deletion_pending = 1), 0)
		FROM people`, at).Scan(
		&page.Counts.Total, &page.Counts.Active, &page.Counts.Pending, &page.Counts.Disabled, &page.Counts.Administrators,
		&page.Counts.ResetRequested, &page.Counts.MFASetup, &page.Counts.InvitationExpired, &page.Counts.Deleting,
	); err != nil {
		return nil, fmt.Errorf("count administrator users: %w", err)
	}
	where, args := administratorUserFilter(query)
	args = append(args, at)
	if where == "" {
		page.Matching = page.Counts.Total
	} else if err := tx.QueryRowContext(ctx, administratorPeople+` SELECT COUNT(*) FROM people`+where, args...).Scan(&page.Matching); err != nil {
		return nil, fmt.Errorf("count matching administrator users: %w", err)
	}

	rows, err := tx.QueryContext(ctx, administratorPeople+`
		SELECT people.id, people.username, people.status, people.user_type, people.is_admin, people.mfa_required, people.mfa_passkey_counts,
		       people.password_reset_requested_at, people.deletion_pending, people.deletion_protected,
		       people.invitation_id, people.expires_at, people.used_at, people.revoked_at,
		       people.has_password, people.password_change_required, people.has_second_factor,
		       people.created_at, people.last_login_at
		FROM people`+where+administratorUserOrder(query.Sort)+` LIMIT :limit OFFSET :offset`,
		append(args, sql.Named("limit", query.Limit), sql.Named("offset", query.Offset))...)
	if err != nil {
		return nil, fmt.Errorf("list administrator users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var user AdministratorUserSummary
		var isAdmin, mfaRequired, mfaPasskeyCounts, deletionPending, deletionProtected int
		var tokenID sql.NullString
		var passwordResetRequestedAt, expiresAt, usedAt, revokedAt, lastLoginAt sql.NullTime
		if err := rows.Scan(
			&user.ID, &user.Username, &user.Status, &user.UserType, &isAdmin, &mfaRequired, &mfaPasskeyCounts,
			&passwordResetRequestedAt, &deletionPending, &deletionProtected,
			&tokenID, &expiresAt, &usedAt, &revokedAt, &user.HasPassword, &user.PasswordChangeRequired,
			&user.HasSecondFactor, &user.CreatedAt, &lastLoginAt,
		); err != nil {
			return nil, fmt.Errorf("scan administrator user: %w", err)
		}
		if user.Status != UserStatusPending && user.Status != UserStatusActive && user.Status != UserStatusDisabled {
			return nil, fmt.Errorf("user %q has invalid status %q", user.ID, user.Status)
		}
		if isAdmin != 0 && isAdmin != 1 {
			return nil, fmt.Errorf("user %q has invalid administrator state", user.ID)
		}
		if mfaRequired != 0 && mfaRequired != 1 {
			return nil, fmt.Errorf("user %q has invalid MFA requirement", user.ID)
		}
		if deletionPending != 0 && deletionPending != 1 {
			return nil, fmt.Errorf("user %q has invalid deletion state", user.ID)
		}
		if deletionProtected != 0 && deletionProtected != 1 {
			return nil, fmt.Errorf("user %q has invalid deletion protection state", user.ID)
		}
		if !user.UserType.Valid() {
			return nil, fmt.Errorf("user %q has invalid type %q", user.ID, user.UserType)
		}
		user.IsAdmin = isAdmin == 1
		user.MFARequired = mfaRequired == 1
		user.MFAPasskeyCounts = user.MFARequired && mfaPasskeyCounts == 1
		user.DeletionPending = deletionPending == 1
		user.DeletionProtected = deletionProtected == 1
		if passwordResetRequestedAt.Valid {
			requestedAt := passwordResetRequestedAt.Time
			user.PasswordResetRequestedAt = &requestedAt
		}
		if lastLoginAt.Valid {
			signedIn := lastLoginAt.Time
			user.LastLoginAt = &signedIn
		}
		// Pending people, mail users and administrators alike, are waiting on
		// an invitation.
		if user.Status == UserStatusPending && (user.UserType == UserTypeWebmail && !user.IsAdmin || user.UserType == UserTypeManagement && user.IsAdmin) {
			user.InvitationState = AdministratorUserInvitationNotIssued
			switch {
			case tokenID.Valid && !usedAt.Valid && !revokedAt.Valid && expiresAt.Valid && expiresAt.Time.After(now):
				user.InvitationState = AdministratorUserInvitationActive
				invitationExpiry := expiresAt.Time
				user.InvitationExpiresAt = &invitationExpiry
			case tokenID.Valid && revokedAt.Valid:
				user.InvitationState = AdministratorUserInvitationRevoked
			case tokenID.Valid && !usedAt.Valid && expiresAt.Valid && !expiresAt.Time.After(now):
				user.InvitationState = AdministratorUserInvitationExpired
				invitationExpiry := expiresAt.Time
				user.InvitationExpiresAt = &invitationExpiry
			}
			if actorSessionID != "" {
				user.InvitationActionReference, err = m.administratorUserInvitationActionReference(actorSessionID, user.ID)
				if err != nil {
					return nil, err
				}
			}
		}
		page.Users = append(page.Users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate administrator users: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close administrator user list: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit administrator user list: %w", err)
	}
	return page, nil
}
