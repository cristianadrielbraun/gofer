package auth

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// A named policy can require two-factor sign-in of the people it reaches,
// and let a passkey be the second step or not. Like their individual
// requirement, it applies at their next sign-in, and sessions they have keep
// working. A change that newly requires it of someone, or stops a passkey
// counting for them — a policy's value, assigning a policy, adding people
// to a group — waits for the administrator to confirm, having seen who it
// reaches, and can sign those people out at once instead.

// TwoFactorChoice is the administrator's answer about the people a change
// newly requires two-factor sign-in of: confirmed, having seen who, and
// whether to sign them out now.
type TwoFactorChoice struct {
	Confirmed bool
	SignOut   bool
}

// TwoFactorPreview is who a change would newly require two-factor sign-in
// of: how many, the first few without an authenticator app and how many in
// all, and how many of their sessions didn't use it.
type TwoFactorPreview struct {
	People          int
	WithoutApp      []InstanceMFAPolicyPerson
	WithoutAppTotal int
	SessionsToEnd   int
}

// TwoFactorConfirmationError refuses a change that newly requires
// two-factor sign-in of someone until it's confirmed, saying who.
type TwoFactorConfirmationError struct {
	Preview TwoFactorPreview
}

func (e *TwoFactorConfirmationError) Error() string {
	return "the change requires two-factor sign-in of more people"
}

// policyTwoFactorPeople are the people a named policy requires two-factor
// sign-in of, and whether a passkey is enough for each: webmail users the
// requirement for everyone doesn't cover. Someone required it one by one
// keeps their own choice too, and a passkey is enough only when both say
// so.
func policyTwoFactorPeople(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	instance, err := readInstanceSecurityPolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	people := map[string]bool{}
	if instance.MFA.RequiresAllUsers() {
		return people, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.mfa_required, u.mfa_passkey_counts, named.passkey_counts
		FROM users u JOIN (`+policy.TwoFactorPoliciesSQL()+`) named ON named.user_id = u.id
		WHERE u.user_type = 'webmail' AND u.is_admin = 0 AND u.deletion_pending = 0`)
	if err != nil {
		return nil, fmt.Errorf("list the people a policy requires two-factor sign-in of: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var individual, individualPasskey, policyPasskey bool
		if err := rows.Scan(&id, &individual, &individualPasskey, &policyPasskey); err != nil {
			return nil, err
		}
		people[id] = policyPasskey && (!individual || individualPasskey)
	}
	return people, rows.Err()
}

// individuallyRequired are the people of ids required two-factor sign-in
// one by one, and whether a passkey is enough for each.
func individuallyRequired(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error) {
	required := map[string]bool{}
	err := inPeopleBatches(ids, func(placeholders string, args []any) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, mfa_passkey_counts FROM users WHERE mfa_required = 1 AND id IN (`+placeholders+`)`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var passkey bool
			if err := rows.Scan(&id, &passkey); err != nil {
				return err
			}
			required[id] = passkey
		}
		return rows.Err()
	})
	return required, err
}

// guardTwoFactor runs change, then works out who it newly requires
// two-factor sign-in of. Unless the administrator confirmed, a change that
// requires it of anyone is refused, with who, and its transaction undone;
// confirmed with SignOut, those people's sessions that didn't use it end.
func (m *Manager) guardTwoFactor(ctx context.Context, tx *sql.Tx, choice TwoFactorChoice, actorUserID, actorSessionID string, now time.Time, target audit.Target, change func() error) error {
	before, err := policyTwoFactorPeople(ctx, tx)
	if err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	after, err := policyTwoFactorPeople(ctx, tx)
	if err != nil {
		return err
	}
	// Newly: required by a policy now, and not before by a policy or one by
	// one; or required before, with a passkey enough then and not now.
	var candidates []string
	for id := range after {
		candidates = append(candidates, id)
	}
	individual, err := individuallyRequired(ctx, tx, candidates)
	if err != nil {
		return err
	}
	// Each group with what its sessions need: the app code only, or a
	// passkey too.
	var newly, appOnly, passkeyEnough []string
	for id, passkeyNow := range after {
		passkeyBefore, wasRequired := before[id]
		if !wasRequired {
			passkeyBefore, wasRequired = individual[id]
		}
		if wasRequired && (!passkeyBefore || passkeyNow) {
			continue
		}
		newly = append(newly, id)
		if passkeyNow {
			passkeyEnough = append(passkeyEnough, id)
		} else {
			appOnly = append(appOnly, id)
		}
	}
	if len(newly) == 0 {
		return nil
	}
	sort.Strings(newly)
	groups := []twoFactorGroup{{people: appOnly}, {people: passkeyEnough, passkey: true}}
	if !choice.Confirmed {
		preview, err := previewTwoFactor(ctx, tx, groups, now)
		if err != nil {
			return err
		}
		return &TwoFactorConfirmationError{Preview: preview}
	}
	if !choice.SignOut {
		return nil
	}
	var ended int64
	for _, group := range groups {
		err := inPeopleBatches(group.people, func(placeholders string, args []any) error {
			result, err := tx.ExecContext(ctx, `
				UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE revoked_at IS NULL AND `+group.weakSessions()+` AND user_id IN (`+placeholders+`)`,
				append([]any{now, actorUserID, SessionRevocationAdminAction}, args...)...)
			if err != nil {
				return fmt.Errorf("sign out the people a policy newly requires two-factor sign-in of: %w", err)
			}
			count, err := result.RowsAffected()
			ended += count
			return err
		})
		if err != nil {
			return err
		}
	}
	return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
		Action: "policy.sessions_ended", Target: target,
		Metadata: map[string]any{"people": len(newly), "count": ended},
	})
}

// twoFactorGroup is people whose second step is the app code alone, or a
// passkey too.
type twoFactorGroup struct {
	people  []string
	passkey bool
}

// weakSessions matches the sessions that don't meet the group's second
// step.
func (group twoFactorGroup) weakSessions() string {
	if group.passkey {
		return `assurance_level NOT IN ('` + string(AssuranceLevelMultiFactor) + `', '` + string(AssuranceLevelPhishingResistant) + `')`
	}
	return `assurance_level != '` + string(AssuranceLevelMultiFactor) + `'`
}

// withoutSecondStep matches the people of the group with nothing set up
// that counts.
func (group twoFactorGroup) withoutSecondStep() string {
	condition := `NOT EXISTS (SELECT 1 FROM totp_credentials t WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL)`
	if group.passkey {
		condition += ` AND NOT EXISTS (SELECT 1 FROM webauthn_credentials w WHERE w.user_id = u.id AND w.revoked_at IS NULL)`
	}
	return condition
}

// previewTwoFactor says who in the groups has nothing set up that counts
// yet, and how many of their working sessions didn't meet their second
// step.
func previewTwoFactor(ctx context.Context, tx *sql.Tx, groups []twoFactorGroup, now time.Time) (TwoFactorPreview, error) {
	var preview TwoFactorPreview
	for _, group := range groups {
		preview.People += len(group.people)
		err := inPeopleBatches(group.people, func(placeholders string, args []any) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT u.id, u.username FROM users u
				WHERE u.id IN (`+placeholders+`) AND `+group.withoutSecondStep(), args...)
			if err != nil {
				return fmt.Errorf("list people without an authenticator app: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var person InstanceMFAPolicyPerson
				if err := rows.Scan(&person.ID, &person.Username); err != nil {
					return err
				}
				preview.WithoutAppTotal++
				preview.WithoutApp = append(preview.WithoutApp, person)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			var sessions int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM sessions
				WHERE revoked_at IS NULL AND `+group.weakSessions()+` AND idle_expires_at > ? AND absolute_expires_at > ?
				  AND user_id IN (`+placeholders+`)`,
				append([]any{now, now}, args...)...).Scan(&sessions); err != nil {
				return fmt.Errorf("count the sessions two-factor sign-in would end: %w", err)
			}
			preview.SessionsToEnd += sessions
			return nil
		})
		if err != nil {
			return preview, err
		}
	}
	// The first few by username, as the requirement for everyone lists them.
	sort.Slice(preview.WithoutApp, func(i, j int) bool {
		left, right := strings.ToLower(preview.WithoutApp[i].Username), strings.ToLower(preview.WithoutApp[j].Username)
		if left != right {
			return left < right
		}
		return preview.WithoutApp[i].ID < preview.WithoutApp[j].ID
	})
	if len(preview.WithoutApp) > InstanceMFAPolicyPreviewLimit {
		preview.WithoutApp = preview.WithoutApp[:InstanceMFAPolicyPreviewLimit]
	}
	return preview, nil
}

// peopleBatch is how many people one query names, well inside SQLite's
// limit on a query's arguments.
const peopleBatch = 500

// inPeopleBatches calls do with each batch of people as placeholders and
// arguments.
func inPeopleBatches(people []string, do func(placeholders string, args []any) error) error {
	for start := 0; start < len(people); start += peopleBatch {
		batch := people[start:min(start+peopleBatch, len(people))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		if err := do(strings.TrimSuffix(strings.Repeat("?,", len(batch)), ","), args); err != nil {
			return err
		}
	}
	return nil
}
