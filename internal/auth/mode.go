package auth

import (
	"context"
	"errors"
	"strings"
)

type Mode string

const (
	ModeOpen     Mode = "open"
	ModePersonal Mode = "personal"
	ModeManaged  Mode = "managed"
)

func (c *Config) AuthenticationMode() Mode {
	if c.Mode != "" {
		return c.Mode
	}
	if c.Enabled {
		return ModeManaged
	}
	return ModeOpen
}

func (c *Config) ValidateMode() error {
	switch c.AuthenticationMode() {
	case ModeOpen, ModePersonal, ModeManaged:
		return nil
	default:
		return errors.New("GOFER_AUTH_MODE must be open, personal, or managed")
	}
}
func (m *Manager) IsPersonal() bool { return m.config.AuthenticationMode() == ModePersonal }

// ValidateRuntimeMode never selects a user from a multi-user database. A personal
// installation reuses only the explicit local profile, preserving its ownership.
func (m *Manager) ValidateRuntimeMode(ctx context.Context) error {
	if err := m.config.ValidateMode(); err != nil {
		return err
	}
	state, err := m.SetupState(ctx)
	if err != nil {
		return err
	}
	if m.IsPersonal() {
		var count, incompatible int
		if err := m.db.Read().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN id != 'default' OR user_type != 'webmail' OR is_admin != 0 THEN 1 ELSE 0 END),0) FROM users`).Scan(&count, &incompatible); err != nil {
			return err
		}
		if incompatible != 0 || count > 1 || (state.Initialized && (count != 1 || state.OwnerUserID != "default")) {
			return errors.New("personal mode requires a fresh database or the single local profile; this database belongs to a different authentication mode")
		}
	} else if m.config.AuthenticationMode() == ModeManaged && state.Initialized {
		user, err := m.GetUserByID(ctx, state.OwnerUserID)
		if err != nil {
			return err
		}
		if user != nil && !user.IsManagement() {
			return errors.New("personal profile cannot be opened in managed mode; use personal mode")
		}
	}
	return nil
}

func (m *Manager) requirePersonalProfile(ctx context.Context, q authenticationPolicyQueryer, userID string) error {
	if !m.IsPersonal() {
		return nil
	}
	var owner string
	if err := q.QueryRowContext(ctx, `SELECT owner_user_id FROM auth_system_state WHERE id=1 AND initialized=1`).Scan(&owner); err != nil {
		return ErrUserNotActive
	}
	if strings.TrimSpace(owner) == "" || owner != userID || owner != "default" {
		return ErrUserNotActive
	}
	return nil
}

var ErrPersonalSetupBlocked = errors.New("personal setup requires a fresh database or an unprotected local profile")
