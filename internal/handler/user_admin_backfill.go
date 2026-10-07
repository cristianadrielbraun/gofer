package handler

import (
	"context"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) startOwnedContactBackfill(ctx context.Context, scope models.AdminWebmailScope) bool {
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return false
	}
	actor := storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion}
	owned := h.ownedMailbox
	admission, cancel := h.ownedAdminDiagnosticContext(ctx)
	defer cancel()
	if owned.userStorage.ValidateDiagnosticsAdministrator(admission, actor) != nil {
		return false
	}
	owners := []string{}
	for _, owner := range scope.Users {
		if scope.SelectedUserID == "" || scope.SelectedUserID == owner.ID {
			owners = append(owners, owner.ID)
		}
	}
	owned.contactBackfillMu.Lock()
	defer owned.contactBackfillMu.Unlock()
	if owned.userBackfillContext == nil || owned.userBackfillContext.Err() != nil || owned.contactBackfillState.InProgress {
		return false
	}
	select {
	case owned.userBackfillQueue <- userContactBackfillJob{owners: owners, actor: &actor}:
		owned.contactBackfillState = models.ContactBackfillState{InProgress: true, StartedAt: time.Now().UTC()}
		return true
	default:
		return false
	}
}

func (h *Handler) runAdminContactBackfill(ctx context.Context, job userContactBackfillJob) {
	set := func(update func(*models.ContactBackfillState)) {
		h.contactBackfillMu.Lock()
		update(&h.contactBackfillState)
		state := h.contactBackfillState
		h.contactBackfillMu.Unlock()
		h.publishInstanceContactBackfill(state)
	}
	total := 0
	workErr := h.userStorage.ValidateDiagnosticsAdministrator(ctx, *job.actor)
	for _, owner := range job.owners {
		if workErr != nil {
			break
		}
		var n int
		n, workErr = h.userStorage.CountUserContactBackfill(ctx, owner, job.actor)
		if workErr != nil {
			break
		}
		total += n
	}
	set(func(s *models.ContactBackfillState) { s.Total = total })
	processed := 0
	if workErr == nil {
		for _, owner := range job.owners {
			before := processed
			workErr = h.userStorage.BackfillUserContacts(ctx, owner, job.actor, "", func(n int) {
				processed = before + n
				set(func(s *models.ContactBackfillState) { s.Processed = processed })
			})
			if workErr != nil {
				break
			}
		}
	}
	if workErr == nil {
		workErr = h.userStorage.ValidateDiagnosticsAdministrator(ctx, *job.actor)
	}
	set(func(s *models.ContactBackfillState) {
		s.InProgress = false
		s.FinishedAt = time.Now().UTC()
		if workErr != nil {
			s.LastError = workErr.Error()
		}
	})
}
