package mail

import (
	"context"
	"errors"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

var ErrUserIMAPManualCapacity = errors.New("manual mail sync capacity is full")
var errUserIMAPDisabled = errors.New("email sync was disabled")

type userIMAPManualRun struct {
	id                                 string
	accounts                           []string
	cancel                             context.CancelFunc
	done, failures, cancelled, skipped int
	active                             string
}

// StartManualSync validates request ownership before detaching from the request.
// At most four owners can run; accounts run sequentially within each owner's run
// and share the receive/body service's global four-session limit.
func (s *UserIMAP) StartManualSync(ctx context.Context, owner string, ids []string) (string, bool, error) {
	if len(ids) == 0 || len(ids) > 256 {
		return "", false, errors.New("manual sync requires between 1 and 256 accounts")
	}
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return "", false, err
	}
	accounts := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		err := s.accounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, db *storage.DB) error {
			cfg, err := local.GetConfig(ctx, id)
			if err != nil {
				return err
			}
			if !s.SupportsAccount(cfg) || !db.IsEmailSyncEnabled(ctx, id) {
				return storage.ErrAccountRoute
			}
			return nil
		})
		if err != nil {
			return "", false, err
		}
		seen[id] = true
		accounts = append(accounts, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		return "", false, errors.New("IMAP worker is shutting down")
	}
	if run := s.manualRuns[owner]; run != nil {
		return run.id, false, nil
	}
	if len(s.manualRuns) >= 4 {
		return "", false, ErrUserIMAPManualCapacity
	}
	if s.manualRuns == nil {
		s.manualRuns = make(map[string]*userIMAPManualRun)
	}
	workCtx, cancel := context.WithTimeout(s.ctx, manualSyncTimeout)
	run := &userIMAPManualRun{id: uuid.NewString(), accounts: accounts, cancel: cancel}
	s.manualRuns[owner] = run
	s.operations.Add(1)
	go s.runManual(workCtx, owner, run)
	return run.id, true, nil
}

// Caller holds mu. Every payload owns a copy of its account identities.
func (s *UserIMAP) manualEventLocked(owner string, run *userIMAPManualRun, kind EventType, status, errorText string) Event {
	payload := map[string]any{
		"user_id": owner, "run_id": run.id, "mode": "sync",
		"account_ids": append([]string(nil), run.accounts...), "accounts_total": len(run.accounts),
		"accounts_done": run.done, "parallelism": 1, "failures": run.failures, "skipped": run.skipped,
		"cancelled": run.cancelled, "not_done": len(run.accounts) - run.done, "status": status,
	}
	if run.active != "" {
		payload["account_index"] = run.done + 1
	}
	if errorText != "" {
		payload["error"] = errorText
	}
	account := run.active
	if kind != EventManualSyncProgress {
		account = ""
	}
	return Event{Type: kind, UserID: owner, AccountID: account, Payload: payload}
}

func (s *UserIMAP) runManual(ctx context.Context, owner string, run *userIMAPManualRun) {
	defer s.operations.Done()
	defer run.cancel()
	defer func() {
		s.mu.Lock()
		if s.manualRuns[owner] == run {
			delete(s.manualRuns, owner)
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	event := s.manualEventLocked(owner, run, EventManualSyncStarted, "running", "")
	s.mu.Unlock()
	s.events.Publish(event)
	for index, id := range run.accounts {
		if ctx.Err() != nil {
			break
		}
		if s.Routing().ValidateUser(ctx, owner) != nil {
			run.cancel()
			break
		}
		s.mu.Lock()
		run.active = id
		event = s.manualEventLocked(owner, run, EventManualSyncProgress, "syncing", "")
		s.mu.Unlock()
		s.events.Publish(event)
		accountCtx := withAccountSyncProgressScope(ctx, accountSyncProgressScope{
			kind: string(accountSyncManual), mode: "sync", userID: owner, runID: run.id,
			accountIDs: run.accounts, accountsTotal: len(run.accounts), accountIndex: index + 1, parallelism: 1,
		})
		err := s.Sync(accountCtx, owner, id)
		status, errorText := "synced", ""
		s.mu.Lock()
		run.done++
		if err != nil {
			errorText = err.Error()
			if errors.Is(err, errUserIMAPDisabled) {
				status = "skipped"
				run.skipped++
			} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				status = "cancelled"
				run.cancelled++
			} else {
				status = "error"
				run.failures++
			}
		}
		event = s.manualEventLocked(owner, run, EventManualSyncProgress, status, errorText)
		event.Payload["account_index"] = index + 1
		run.active = ""
		s.mu.Unlock()
		s.events.Publish(event)
	}
	s.mu.Lock()
	status := "ok"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if run.failures == len(run.accounts) {
		status = "error"
	} else if run.failures > 0 || run.cancelled > 0 || run.skipped > 0 {
		status = "partial"
	}
	event = s.manualEventLocked(owner, run, EventManualSyncComplete, status, "")
	s.mu.Unlock()
	s.events.Publish(event)
}

func (s *UserIMAP) CancelManualSync(ctx context.Context, owner string) (bool, error) {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.manualRuns[owner]; run != nil {
		run.cancel()
		return true, nil
	}
	return false, nil
}

func (s *UserIMAP) ManualSyncSnapshot(ctx context.Context, owner string) ([]Event, error) {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.manualRuns[owner]; run != nil {
		return []Event{s.manualEventLocked(owner, run, EventManualSyncStarted, "running", ""), s.manualEventLocked(owner, run, EventManualSyncProgress, "syncing", "")}, nil
	}
	return nil, nil
}
