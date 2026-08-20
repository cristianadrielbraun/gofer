package authoperator

import (
	"context"
	"fmt"
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
	Email    string
	Status   auth.UserStatus
	UserType auth.UserType
	IsAdmin  bool
}

func NewService(db *storage.DB) *Service {
	return &Service{db: db, manager: auth.NewManager(&auth.Config{}, db)}
}

func (service *Service) Recover(ctx context.Context, userID string) (*auth.LocalRecoveryResult, error) {
	return service.manager.RecoverUserLocally(ctx, userID)
}

func (service *Service) RevokeSessions(ctx context.Context, userID string) (*auth.LocalSessionRevocationResult, error) {
	return service.manager.RevokeUserSessionsLocally(ctx, userID)
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
		SELECT id, COALESCE(username, ''), email, status, user_type, is_admin
		FROM users
		ORDER BY COALESCE(username_normalized, email_normalized, lower(trim(email))), id`)
	if err != nil {
		return nil, fmt.Errorf("list authentication users: %w", err)
	}
	defer rows.Close()

	var users []UserSummary
	for rows.Next() {
		var user UserSummary
		var status string
		var isAdmin int
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &status, &user.UserType, &isAdmin); err != nil {
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
