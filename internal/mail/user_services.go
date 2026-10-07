package mail

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// RunMessageWork joins external work on cached message content to the same
// bounded gate and lifecycle as body fetching. The callback must lease separately
// for snapshots/publication and must not recursively fetch this message's body.
func (s *UserIMAP) RunMessageWork(ctx context.Context, owner string, messageID int64, fn func(context.Context, string) error) error {
	if fn == nil || messageID <= 0 {
		return errors.New("message work needs a valid message and operation")
	}
	var account string
	if err := s.Routing().WithUser(ctx, owner, func(db *storage.DB) error {
		info, err := db.GetMessageStorageInfoForUser(ctx, messageID, owner)
		if err == nil && info == nil {
			return sql.ErrNoRows
		}
		if err == nil {
			account = info.AccountID
		}
		return err
	}); err != nil {
		return err
	}
	return s.operation(ctx, owner, account, messageID, 2*time.Minute, func(ctx context.Context) error {
		scope, err := s.snapshot(ctx, owner, account)
		if err != nil {
			return err
		}
		validate := func() error {
			return scope.call(ctx, func(db *storage.DB) error {
				info, err := db.GetMessageStorageInfoForUser(ctx, messageID, owner)
				if err != nil {
					return err
				}
				if info == nil || info.AccountID != account {
					return sql.ErrNoRows
				}
				return ctx.Err()
			})
		}
		if err := validate(); err != nil {
			return err
		}
		if err := fn(ctx, account); err != nil {
			return err
		}
		return validate()
	})
}

// AccountService names independent contacts/calendar operation gates. Service
// identity is a separate key field, so every migrated message ID remains usable.
type AccountService int

const (
	AccountServiceContacts AccountService = iota + 1
	AccountServiceCalendar
)

// RunAccountService uses independent bounded contacts/calendar slots and shares lifecycle
// cancellation, account edit cancellation and deletion drain. Its callback
// receives no database handle: snapshot/commit callbacks must lease separately,
// and publication must validate its saved identity/configuration in its SQL
// transaction. Do not recursively enter the same account/service operation.
func (s *UserIMAP) RunAccountService(ctx context.Context, owner, id string, service AccountService, timeout time.Duration, fn func(context.Context) error) error {
	if service != AccountServiceContacts && service != AccountServiceCalendar {
		return errors.New("unknown account service")
	}
	if timeout <= 0 || fn == nil {
		return errors.New("account service needs a timeout and operation")
	}
	slots := s.contactSlots
	if service == AccountServiceCalendar {
		slots = s.calendarSlots
	}
	return s.operationWithSlots(ctx, owner, id, 0, service, timeout, slots, fn)
}

// RunUserServiceWork joins the whole owner job to runtime shutdown, including
// queue claims/finalization between account callbacks. It takes no protocol slot:
// nested account operations acquire their own independent service slots.
func (s *UserIMAP) RunUserServiceWork(ctx context.Context, owner string, fn func(context.Context) error) error {
	if owner == "" || fn == nil {
		return errors.New("user service work needs an owner and operation")
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRoot := context.AfterFunc(s.ctx, cancel)
	defer stopRoot()
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return context.Canceled
	}
	s.operations.Add(1)
	run := &userIMAPOperation{cancel: cancel}
	s.trackUserRunLocked(run, owner)
	s.mu.Unlock()
	defer s.operations.Done()
	defer func() {
		s.mu.Lock()
		delete(s.userRuns, run)
		s.mu.Unlock()
	}()
	if err := s.Routing().ValidateUser(workCtx, owner); err != nil {
		return err
	}
	err := fn(workCtx)
	if workCtx.Err() != nil {
		return workCtx.Err()
	}
	return err
}

// StartBackgroundService joins a bounded service's dispatcher and workers to
// runtime shutdown. The callback must join any goroutines it starts before
// returning. Both the caller's context and the runtime root stop the service.
func (s *UserIMAP) StartBackgroundService(ctx context.Context, fn func(context.Context)) error {
	if ctx == nil || fn == nil {
		return errors.New("background service needs a context and operation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		return context.Canceled
	}
	workCtx, cancel := context.WithCancel(s.ctx)
	stopCaller := context.AfterFunc(ctx, cancel)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer stopCaller()
		fn(workCtx)
	}()
	return nil
}
