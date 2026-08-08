package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	defaultSetupTokenLifetime        = 30 * time.Minute
	minimumConfiguredSetupTokenBytes = 32
	maximumConfiguredSetupTokenBytes = 1024
)

var (
	ErrSetupAlreadyInitialized    = errors.New("authentication setup is already complete")
	ErrConfiguredSetupTokenLength = errors.New("GOFER_SETUP_TOKEN must contain between 32 and 1024 bytes")
)

type SetupState struct {
	Initialized     bool
	OwnerUserID     string
	InitializedAt   *time.Time
	TokenConfigured bool
	TokenExpiresAt  *time.Time
	TokenAttempts   int64
	TokenRotatedAt  *time.Time
	CutoverVersion  int64
}

type SetupTokenProvision struct {
	State      SetupState
	Token      string
	Created    bool
	Configured bool
}

// SetupState reads the singleton authentication initialization state without
// inferring it from user rows. A missing singleton is an uninitialized
// instance whose setup token has not been provisioned yet.
func (m *Manager) SetupState(ctx context.Context) (SetupState, error) {
	return readSetupState(ctx, m.db.Read())
}

// EnsureSetupToken provisions the first setup token if the instance is still
// uninitialized and no token has been persisted. It never rotates or reprints
// an existing token. A caller-supplied token is accepted only for initial
// provisioning and is never returned by this method.
func (m *Manager) EnsureSetupToken(ctx context.Context, configuredToken string) (*SetupTokenProvision, error) {
	current, err := m.SetupState(ctx)
	if err != nil {
		return nil, err
	}
	if current.Initialized || current.TokenConfigured {
		return &SetupTokenProvision{State: current}, nil
	}
	if configuredToken != "" {
		if tokenBytes := len([]byte(configuredToken)); tokenBytes < minimumConfiguredSetupTokenBytes || tokenBytes > maximumConfiguredSetupTokenBytes {
			return nil, ErrConfiguredSetupTokenLength
		}
	}

	rawToken := configuredToken
	configured := rawToken != ""
	if !configured {
		rawToken, err = m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate setup token: %w", err)
		}
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup token event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	expiresAt := now.Add(defaultSetupTokenLifetime)
	result := &SetupTokenProvision{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		state, err := readSetupState(ctx, tx)
		if err != nil {
			return err
		}
		if state.Initialized || state.TokenConfigured {
			result.State = state
			return nil
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_system_state (
				id, initialized, setup_token_hash, setup_expires_at,
				setup_attempts, cutover_version
			) VALUES (1, 0, ?, ?, 0, 0)
			ON CONFLICT(id) DO UPDATE SET
				setup_token_hash = excluded.setup_token_hash,
				setup_expires_at = excluded.setup_expires_at,
				setup_attempts = 0
			WHERE auth_system_state.initialized = 0
			  AND auth_system_state.setup_token_hash IS NULL`,
			hashToken(rawToken), expiresAt,
		); err != nil {
			return fmt.Errorf("persist initial setup token: %w", err)
		}
		source := "generated"
		if configured {
			source = "environment"
		}
		metadata, err := setupTokenEventJSON(expiresAt, source)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, 1, ?, ?)`,
			eventID, now, AuthEventSetupTokenIssued,
			AuthEventReasonSystemInitialization, metadata,
		); err != nil {
			return fmt.Errorf("record initial setup token event: %w", err)
		}
		result.State = SetupState{
			TokenConfigured: true,
			TokenExpiresAt:  timePointer(expiresAt),
		}
		result.Created = true
		result.Configured = configured
		if !configured {
			result.Token = rawToken
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RotateSetupTokenLocally replaces an expired, lost, or otherwise unusable
// setup token. The caller must hold Gofer's exclusive runtime lock. The raw
// replacement is returned only after its hash and audit event commit.
func (m *Manager) RotateSetupTokenLocally(ctx context.Context) (*SetupTokenProvision, error) {
	current, err := m.SetupState(ctx)
	if err != nil {
		return nil, err
	}
	if current.Initialized {
		return nil, ErrSetupAlreadyInitialized
	}

	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate replacement setup token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup token rotation event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	expiresAt := now.Add(defaultSetupTokenLifetime)
	result := &SetupTokenProvision{
		Token:   rawToken,
		Created: true,
		State: SetupState{
			TokenConfigured: true,
			TokenExpiresAt:  timePointer(expiresAt),
			TokenRotatedAt:  timePointer(now),
		},
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		state, err := readSetupState(ctx, tx)
		if err != nil {
			return err
		}
		if state.Initialized {
			return ErrSetupAlreadyInitialized
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_system_state (
				id, initialized, setup_token_hash, setup_expires_at,
				setup_attempts, setup_rotated_at, cutover_version
			) VALUES (1, 0, ?, ?, 0, ?, 0)
			ON CONFLICT(id) DO UPDATE SET
				setup_token_hash = excluded.setup_token_hash,
				setup_expires_at = excluded.setup_expires_at,
				setup_attempts = 0,
				setup_rotated_at = excluded.setup_rotated_at
			WHERE auth_system_state.initialized = 0`,
			hashToken(rawToken), expiresAt, now,
		); err != nil {
			return fmt.Errorf("rotate setup token: %w", err)
		}
		metadata, err := setupTokenEventJSON(expiresAt, "local_operator")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, 1, ?, ?)`,
			eventID, now, AuthEventSetupTokenRotated,
			AuthEventReasonLocalOperator, metadata,
		); err != nil {
			return fmt.Errorf("record setup token rotation: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type setupStateScanner interface {
	Scan(...any) error
}

type setupStateQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSetupState(ctx context.Context, queryer setupStateQueryer) (SetupState, error) {
	var state SetupState
	var initialized int
	var ownerUserID, tokenHash sql.NullString
	var initializedAt, expiresAt, rotatedAt sql.NullTime
	err := scanSetupState(queryer.QueryRowContext(ctx, `
		SELECT initialized, owner_user_id, initialized_at, setup_token_hash,
		       setup_expires_at, setup_attempts, setup_rotated_at, cutover_version
		FROM auth_system_state WHERE id = 1`),
		&initialized, &ownerUserID, &initializedAt, &tokenHash,
		&expiresAt, &state.TokenAttempts, &rotatedAt, &state.CutoverVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SetupState{}, nil
	}
	if err != nil {
		return SetupState{}, fmt.Errorf("read authentication setup state: %w", err)
	}
	state.Initialized = initialized == 1
	state.OwnerUserID = ownerUserID.String
	state.TokenConfigured = tokenHash.Valid && tokenHash.String != ""
	if initializedAt.Valid {
		state.InitializedAt = timePointer(initializedAt.Time)
	}
	if expiresAt.Valid {
		state.TokenExpiresAt = timePointer(expiresAt.Time)
	}
	if rotatedAt.Valid {
		state.TokenRotatedAt = timePointer(rotatedAt.Time)
	}
	return state, nil
}

func scanSetupState(scanner setupStateScanner, destinations ...any) error {
	return scanner.Scan(destinations...)
}

func setupTokenEventJSON(expiresAt time.Time, source string) (string, error) {
	metadata := struct {
		ExpiresAt string `json:"expires_at"`
		Source    string `json:"source"`
	}{ExpiresAt: expiresAt.UTC().Format(time.RFC3339), Source: source}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode setup token event metadata: %w", err)
	}
	return string(encoded), nil
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
