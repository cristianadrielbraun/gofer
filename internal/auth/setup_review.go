package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrSetupRecoveryAcknowledgementRequired = errors.New("an acknowledged setup recovery-code batch is required")

type SetupReview struct {
	TopologyKind SetupOwnerTopologyKind
	Mode         SetupOwnerMode

	TargetUserID    string
	CurrentName     string
	CurrentUsername string
	CurrentEmail    string
	CurrentStatus   UserStatus
	CurrentIsAdmin  bool

	OwnerName     string
	OwnerUsername string
	OwnerEmail    string

	ExistingUserCount      int
	TotalMailboxCount      int64
	TargetMailboxCount     int64
	UnassignedMailboxCount int64
	UnrevokedSessionCount  int64
	TargetLegacySessions   int64

	ExistingPasswordCredentials int64
	RetainedPasskeys            int64
	ReplacedTOTPs               int64
	RetainedIdentities          int64
	ReplacedRecoveryCodes       int64

	CreatesNewOwner     bool
	ClaimsLegacyDefault bool
	ClaimsExistingUser  bool
	BlockedMessage      string
}

// GetSetupReview builds a transactionally consistent, secret-free view of the
// exact owner and migration consequences. It performs no enrollment writes.
func (m *Manager) GetSetupReview(ctx context.Context, token, origin string) (*SetupReview, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin setup review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := m.clock.Now().UTC()
	_, draft, topology, err := m.currentSetupSecurityDraft(ctx, tx, token, canonicalOrigin, now)
	if err != nil {
		return nil, err
	}
	if draft.TOTPSecret == "" || draft.TOTPConfirmedStep == nil {
		return nil, ErrSetupTOTPConfirmationRequired
	}
	if !validSetupRecoveryDraft(draft) || draft.RecoveryBatchID == "" || !draft.RecoveryAcknowledged {
		return nil, ErrSetupRecoveryAcknowledgementRequired
	}

	review := &SetupReview{
		TopologyKind:        topology.Kind,
		Mode:                draft.Mode,
		TargetUserID:        draft.TargetUserID,
		OwnerName:           draft.Name,
		OwnerUsername:       draft.Username,
		OwnerEmail:          draft.Email,
		ExistingUserCount:   len(topology.Candidates),
		CreatesNewOwner:     draft.Mode == SetupOwnerModeCreate,
		ClaimsLegacyDefault: topology.Kind == SetupOwnerTopologyLegacyDefault && draft.Mode == SetupOwnerModeExisting,
		ClaimsExistingUser:  topology.Kind == SetupOwnerTopologyExisting && draft.Mode == SetupOwnerModeExisting,
	}

	if draft.Mode == SetupOwnerModeExisting {
		candidate, found := setupReviewCandidate(topology, draft.TargetUserID)
		if !found {
			return nil, ErrSetupOwnerDraftRequired
		}
		review.CurrentName = candidate.Name
		review.CurrentUsername = candidate.Username
		review.CurrentEmail = candidate.Email
		review.CurrentStatus = candidate.Status
		review.CurrentIsAdmin = candidate.IsAdmin
		review.TargetMailboxCount = candidate.MailboxCount
		review.TargetLegacySessions = candidate.LegacySessions
		review.ExistingPasswordCredentials = candidate.PasswordCredentialCount
		review.RetainedPasskeys = candidate.ActivePasskeyCount
		review.ReplacedTOTPs = candidate.ActiveTOTPCount
		review.RetainedIdentities = candidate.IdentityCount
		review.ReplacedRecoveryCodes = candidate.ActiveRecoveryCodeCount
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM accounts),
			(SELECT COUNT(*) FROM accounts account
			 WHERE account.user_id IS NULL OR NOT EXISTS (SELECT 1 FROM users WHERE users.id = account.user_id)),
			(SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL)`,
	).Scan(&review.TotalMailboxCount, &review.UnassignedMailboxCount, &review.UnrevokedSessionCount); err != nil {
		return nil, fmt.Errorf("read setup review impact: %w", err)
	}
	if review.UnassignedMailboxCount > 0 {
		review.BlockedMessage = fmt.Sprintf(
			"Gofer found %d mail account(s) without a valid user owner. Repair those ownership records locally before completing setup.",
			review.UnassignedMailboxCount,
		)
	}
	return review, nil
}

func setupReviewCandidate(topology SetupOwnerTopology, userID string) (SetupOwnerCandidate, bool) {
	for _, candidate := range topology.Candidates {
		if candidate.ID == userID {
			return candidate, true
		}
	}
	return SetupOwnerCandidate{}, false
}
