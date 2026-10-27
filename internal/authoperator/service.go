package authoperator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type Service struct {
	db      *storage.DB
	manager *auth.Manager
}

type InstanceStatus struct {
	SchemaVersion        int
	Initialized          bool
	OwnerUserID          string
	SetupTokenConfigured bool
	SetupTokenExpiresAt  *time.Time
	SetupTokenAttempts   int64
	ActiveAdministrators int64
}

type UserSummary struct {
	ID       string
	Username string
	Status   auth.UserStatus
	UserType auth.UserType
	IsAdmin  bool
}

func NewService(db *storage.DB) *Service {
	return &Service{db: db, manager: auth.NewManager(&auth.Config{}, db)}
}

// UserTarget names the user an operator command acts on. Exactly one field is
// set, matching the flag the operator typed.
type UserTarget struct {
	ID       string
	Username string
}

func (service *Service) Recover(ctx context.Context, target UserTarget) (*auth.LocalRecoveryResult, error) {
	userID, err := service.resolveUserID(ctx, target)
	if err != nil {
		return nil, err
	}
	return service.manager.RecoverUserLocally(ctx, userID)
}

func (service *Service) RevokeSessions(ctx context.Context, target UserTarget) (*auth.LocalSessionRevocationResult, error) {
	userID, err := service.resolveUserID(ctx, target)
	if err != nil {
		return nil, err
	}
	return service.manager.RevokeUserSessionsLocally(ctx, userID)
}

func (service *Service) CreateInvitation(ctx context.Context, name, username string, lifetime time.Duration) (*auth.AdministratorUserInvitation, error) {
	return service.manager.CreateUserInvitationLocally(ctx, name, username, lifetime)
}

func (service *Service) resolveUserID(ctx context.Context, target UserTarget) (string, error) {
	query, value, label := `SELECT id FROM users WHERE id = ?`, target.ID, "ID"
	if target.Username != "" {
		query, value, label = `SELECT id FROM users WHERE username_normalized = ?`,
			strings.ToLower(strings.TrimSpace(target.Username)), "username"
	}
	var userID string
	err := service.db.Read().QueryRowContext(ctx, query, value).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no user has %s %q", label, target.ID+target.Username)
	}
	if err != nil {
		return "", fmt.Errorf("look up user %s: %w", label, err)
	}
	return userID, nil
}

func (service *Service) RotateSetupToken(ctx context.Context) (*auth.SetupTokenProvision, error) {
	return service.manager.RotateSetupTokenLocally(ctx)
}

func (service *Service) Status(ctx context.Context) (InstanceStatus, error) {
	status := InstanceStatus{SchemaVersion: storage.CurrentSchemaVersion}
	setupState, err := service.manager.SetupState(ctx)
	if err != nil {
		return InstanceStatus{}, err
	}
	status.Initialized = setupState.Initialized
	status.OwnerUserID = setupState.OwnerUserID
	status.SetupTokenConfigured = setupState.TokenConfigured
	status.SetupTokenExpiresAt = setupState.TokenExpiresAt
	status.SetupTokenAttempts = setupState.TokenAttempts
	if err := service.db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users
		WHERE status = 'active' AND is_admin = 1`,
	).Scan(&status.ActiveAdministrators); err != nil {
		return InstanceStatus{}, fmt.Errorf("count active administrators: %w", err)
	}
	return status, nil
}

func (service *Service) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := service.db.Read().QueryContext(ctx, `
		SELECT id, username, status, user_type, is_admin
		FROM users
		ORDER BY username_normalized, id`)
	if err != nil {
		return nil, fmt.Errorf("list authentication users: %w", err)
	}
	defer rows.Close()

	var users []UserSummary
	for rows.Next() {
		var user UserSummary
		var status string
		var isAdmin int
		if err := rows.Scan(&user.ID, &user.Username, &status, &user.UserType, &isAdmin); err != nil {
			return nil, fmt.Errorf("scan authentication user: %w", err)
		}
		user.Status = auth.UserStatus(status)
		if user.Status != auth.UserStatusPending && user.Status != auth.UserStatusActive && user.Status != auth.UserStatusDisabled {
			return nil, fmt.Errorf("user %q has invalid status %q", user.ID, status)
		}
		if user.UserType != auth.UserTypeWebmail && user.UserType != auth.UserTypeManagement {
			return nil, fmt.Errorf("user %q has invalid type %q", user.ID, user.UserType)
		}
		user.IsAdmin = isAdmin == 1
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authentication users: %w", err)
	}
	return users, nil
}
