package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// Refused administrative attempts are recorded too: a session asking for
// something its administrator may not do, or for a change that needs a
// recent verification it didn't have. The second is what a stolen session
// cookie looks like without the second factor. The refused change's
// transaction rolls back, so auth only notes each refusal on the request's
// context (WithDeniedAttempts), and the request records them once it's
// done, each in a transaction of its own.

// AuditActionAccessDenied names a refused attempt in the audit log. Its
// target is the permission, and its metadata says why.
const AuditActionAccessDenied = "access.denied"

// Why an attempt was refused, as its record's "reason" says.
const (
	DeniedReasonForbidden            = "forbidden"
	DeniedReasonVerificationRequired = "verification_required"
)

const (
	// deniedAttemptWindow is how often the same refusal is recorded: one
	// administrator refused one permission for one reason shows once per
	// window, so a script retrying can't flood the log.
	deniedAttemptWindow = 10 * time.Minute
	// maxDeniedAttemptsPerRequest bounds what one request notes.
	maxDeniedAttemptsPerRequest = 4
	// maxDeniedAttemptLimits bounds the windows kept in memory.
	maxDeniedAttemptLimits = 4096
)

type deniedAttempt struct {
	actorUserID    string
	actorSessionID string
	permission     authz.Permission
	reason         string
}

type deniedAttemptsKey struct{}

type deniedAttempts struct {
	mu       sync.Mutex
	attempts []deniedAttempt
}

// noteDeniedAttempt keeps a refusal for the request to record once it's
// done. Errors that aren't refusals, such as a failed query, aren't noted.
func noteDeniedAttempt(ctx context.Context, actorUserID, actorSessionID string, permission authz.Permission, err error) {
	notes, _ := ctx.Value(deniedAttemptsKey{}).(*deniedAttempts)
	if notes == nil || actorUserID == "" {
		return
	}
	var reason string
	switch {
	case errors.Is(err, authz.ErrForbidden):
		reason = DeniedReasonForbidden
	case errors.Is(err, ErrRecentStepUpRequired):
		reason = DeniedReasonVerificationRequired
	default:
		return
	}
	attempt := deniedAttempt{actorUserID: actorUserID, actorSessionID: actorSessionID, permission: permission, reason: reason}
	notes.mu.Lock()
	defer notes.mu.Unlock()
	for _, noted := range notes.attempts {
		if noted == attempt {
			return
		}
	}
	if len(notes.attempts) < maxDeniedAttemptsPerRequest {
		notes.attempts = append(notes.attempts, attempt)
	}
}

// WithDeniedAttempts returns ctx noting the administrative attempts
// refused under it, and the function that records them. Call it once the
// request is done, outside any transaction.
func (m *Manager) WithDeniedAttempts(ctx context.Context) (context.Context, func()) {
	notes := &deniedAttempts{}
	return context.WithValue(ctx, deniedAttemptsKey{}, notes), func() {
		notes.mu.Lock()
		attempts := append([]deniedAttempt(nil), notes.attempts...)
		notes.mu.Unlock()
		for _, attempt := range attempts {
			if err := m.recordDeniedAttempt(ctx, attempt); err != nil {
				log.Printf("auth: record refused administrative attempt: %v", err)
			}
		}
	}
}

// recordDeniedAttempt writes a refusal in a transaction of its own, unless
// the same one was recorded within deniedAttemptWindow.
func (m *Manager) recordDeniedAttempt(ctx context.Context, attempt deniedAttempt) error {
	if !m.deniedAttemptLimits.allow(attempt, m.clock.Now().UTC()) {
		return nil
	}
	// The response may already be on its way; the record still gets
	// written if the browser goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refused attempt record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	event := audit.Event{
		Action:   AuditActionAccessDenied,
		Outcome:  audit.OutcomeDenied,
		Target:   audit.Target{Kind: "permission", ID: string(attempt.permission)},
		Metadata: map[string]any{"reason": attempt.reason},
	}
	if err := m.auditAdministratorChange(ctx, tx, attempt.actorUserID, attempt.actorSessionID, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refused attempt record: %w", err)
	}
	return nil
}

// deniedAttemptLimits remembers when each refusal was last recorded.
type deniedAttemptLimits struct {
	mu   sync.Mutex
	last map[deniedAttempt]time.Time
}

// allow reports whether attempt may be recorded at now, and if so starts
// its window. Sessions don't matter: signing in again doesn't start a new
// window.
func (limits *deniedAttemptLimits) allow(attempt deniedAttempt, now time.Time) bool {
	attempt.actorSessionID = ""
	limits.mu.Lock()
	defer limits.mu.Unlock()
	if last, ok := limits.last[attempt]; ok && now.Sub(last) < deniedAttemptWindow && !now.Before(last) {
		return false
	}
	if limits.last == nil {
		limits.last = make(map[deniedAttempt]time.Time)
	}
	if len(limits.last) >= maxDeniedAttemptLimits {
		for key, last := range limits.last {
			if now.Sub(last) >= deniedAttemptWindow || now.Before(last) {
				delete(limits.last, key)
			}
		}
		if len(limits.last) >= maxDeniedAttemptLimits {
			return false
		}
	}
	limits.last[attempt] = now
	return true
}
