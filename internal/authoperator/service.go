package authoperator

import (
	"context"
	"database/sql"
	"fmt"

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
	ActiveAdministrators int64
}

type UserSummary struct {
	ID       string
	Username string
	Email    string
	Status   auth.UserStatus
	IsAdmin  bool
}

func NewService(db *storage.DB) *Service {
	return &Service{db: db, manager: auth.NewManager(&auth.Config{}, db)}
}

func (service *Service) Recover(ctx context.Context, userID string) (*auth.LocalRecoveryResult, error) {
	return service.manager.RecoverUserLocally(ctx, userID)
}

func (service *Service) Status(ctx context.Context) (InstanceStatus, error) {
	status := InstanceStatus{SchemaVersion: storage.CurrentSchemaVersion}
	var initialized int
	var ownerUserID sql.NullString
	err := service.db.Read().QueryRowContext(ctx, `
		SELECT initialized, owner_user_id
		FROM auth_system_state WHERE id = 1`,
	).Scan(&initialized, &ownerUserID)
	if err != nil && err != sql.ErrNoRows {
		return InstanceStatus{}, fmt.Errorf("read authentication system state: %w", err)
	}
	if err == nil {
		status.Initialized = initialized == 1
		if ownerUserID.Valid {
			status.OwnerUserID = ownerUserID.String
		}
	}
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
		SELECT id, COALESCE(username, ''), email, status, is_admin
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
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &status, &isAdmin); err != nil {
			return nil, fmt.Errorf("scan authentication user: %w", err)
		}
		user.Status = auth.UserStatus(status)
		if user.Status != auth.UserStatusPending && user.Status != auth.UserStatusActive && user.Status != auth.UserStatusDisabled {
			return nil, fmt.Errorf("user %q has invalid status %q", user.ID, status)
		}
		user.IsAdmin = isAdmin == 1
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authentication users: %w", err)
	}
	return users, nil
}
